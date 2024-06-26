// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mvcc

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/pkg/capnslog"
	"github.com/thistonyuncle/etcd/lease"
	"github.com/thistonyuncle/etcd/mvcc/backend"
	"github.com/thistonyuncle/etcd/mvcc/mvccpb"
	"github.com/thistonyuncle/etcd/pkg/schedule"
	"golang.org/x/net/context"
)

var (
	keyBucketName  = []byte("key")
	metaBucketName = []byte("meta")

	consistentIndexKeyName  = []byte("consistent_index")
	scheduledCompactKeyName = []byte("scheduledCompactRev")
	finishedCompactKeyName  = []byte("finishedCompactRev")

	ErrCompacted = errors.New("mvcc: required revision has been compacted")
	ErrFutureRev = errors.New("mvcc: required revision is a future revision")
	ErrCanceled  = errors.New("mvcc: watcher is canceled")
	ErrClosed    = errors.New("mvcc: closed")

	plog = capnslog.NewPackageLogger("github.com/thistonyuncle/etcd", "mvcc")
)

const (
	// markedRevBytesLen is the byte length of marked revision.
	// The first `revBytesLen` bytes represents a normal revision. The last
	// one byte is the mark.
	markedRevBytesLen      = revBytesLen + 1
	markBytePosition       = markedRevBytesLen - 1
	markTombstone     byte = 't'

	restoreChunkKeys = 10000
)

// ConsistentIndexGetter is an interface that wraps the Get method.
// Consistent index is the offset of an entry in a consistent replicated log.
type ConsistentIndexGetter interface {
	// ConsistentIndex returns the consistent index of current executing entry.
	ConsistentIndex() uint64
}

type store struct {
	ReadView
	WriteView

	// consistentIndex caches the "consistent_index" key's value. Accessed
	// through atomics so must be 64-bit aligned.
	consistentIndex uint64

	// mu read locks for txns and write locks for non-txn store changes.
	mu sync.RWMutex

	ig ConsistentIndexGetter

	b       backend.Backend
	kvindex index

	le lease.Lessor

	// revMuLock protects currentRev and compactMainRev.
	// Locked at end of write txn and released after write txn unlock lock.
	// Locked before locking read txn and released after locking.
	revMu sync.RWMutex
	// currentRev is the revision of the last completed transaction.
	currentRev int64
	// compactMainRev is the main revision of the last compaction.
	compactMainRev int64

	// bytesBuf8 is a byte slice of length 8
	// to avoid a repetitive allocation in saveIndex.
	bytesBuf8 []byte

	fifoSched schedule.Scheduler

	stopc chan struct{}
}

// NewStore returns a new store. It is useful to create a store inside
// mvcc pkg. It should only be used for testing externally.
func NewStore(b backend.Backend, le lease.Lessor, ig ConsistentIndexGetter) *store {
	s := &store{
		b:       b,
		ig:      ig,
		kvindex: newTreeIndex(),

		le: le,

		currentRev:     1,
		compactMainRev: -1,

		bytesBuf8: make([]byte, 8),
		fifoSched: schedule.NewFIFOScheduler(),

		stopc: make(chan struct{}),
	}
	s.ReadView = &readView{s}
	s.WriteView = &writeView{s}
	if s.le != nil {
		s.le.SetRangeDeleter(func() lease.TxnDelete { return s.Write() })
	}

	tx := s.b.BatchTx()
	tx.Lock()
	tx.UnsafeCreateBucket(keyBucketName)
	tx.UnsafeCreateBucket(metaBucketName)
	tx.Unlock()
	s.b.ForceCommit()

	if err := s.restore(); err != nil {
		// TODO: return the error instead of panic here?
		panic("failed to recover store from backend")
	}

	return s
}

func (s *store) compactBarrier(ctx context.Context, ch chan struct{}) {
	if ctx == nil || ctx.Err() != nil {
		s.mu.Lock()
		select {
		case <-s.stopc:
		default:
			f := func(ctx context.Context) { s.compactBarrier(ctx, ch) }
			s.fifoSched.Schedule(f)
		}
		s.mu.Unlock()
		return
	}
	close(ch)
}

func (s *store) Hash() (hash uint32, revision int64, err error) {
	s.b.ForceCommit()
	h, err := s.b.Hash(DefaultIgnores)
	return h, s.currentRev, err
}

func (s *store) Compact(rev int64) (<-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revMu.Lock()
	defer s.revMu.Unlock()

	if rev <= s.compactMainRev {
		ch := make(chan struct{})
		f := func(ctx context.Context) { s.compactBarrier(ctx, ch) }
		s.fifoSched.Schedule(f)
		return ch, ErrCompacted
	}
	if rev > s.currentRev {
		return nil, ErrFutureRev
	}

	start := time.Now()

	s.compactMainRev = rev

	rbytes := newRevBytes()
	revToBytes(revision{main: rev}, rbytes)

	tx := s.b.BatchTx()
	tx.Lock()
	tx.UnsafePut(metaBucketName, scheduledCompactKeyName, rbytes)
	tx.Unlock()
	// ensure that desired compaction is persisted
	s.b.ForceCommit()

	keep := s.kvindex.Compact(rev)
	ch := make(chan struct{})
	var j = func(ctx context.Context) {
		if ctx.Err() != nil {
			s.compactBarrier(ctx, ch)
			return
		}
		if !s.scheduleCompaction(rev, keep) {
			s.compactBarrier(nil, ch)
			return
		}
		close(ch)
	}

	s.fifoSched.Schedule(j)

	indexCompactionPauseDurations.Observe(float64(time.Since(start) / time.Millisecond))
	return ch, nil
}

// DefaultIgnores is a map of keys to ignore in hash checking.
var DefaultIgnores map[backend.IgnoreKey]struct{}

func init() {
	DefaultIgnores = map[backend.IgnoreKey]struct{}{
		// consistent index might be changed due to v2 internal sync, which
		// is not controllable by the user.
		{Bucket: string(metaBucketName), Key: string(consistentIndexKeyName)}: {},
	}
}

func (s *store) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx := s.b.BatchTx()
	tx.Lock()
	s.saveIndex(tx)
	tx.Unlock()
	s.b.ForceCommit()
}

func (s *store) Restore(b backend.Backend) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	close(s.stopc)
	s.fifoSched.Stop()

	atomic.StoreUint64(&s.consistentIndex, 0)
	s.b = b
	s.kvindex = newTreeIndex()
	s.currentRev = 1
	s.compactMainRev = -1
	s.fifoSched = schedule.NewFIFOScheduler()
	s.stopc = make(chan struct{})

	return s.restore()
}

func (s *store) restore() error {
	min, max := newRevBytes(), newRevBytes()
	revToBytes(revision{main: 1}, min)
	revToBytes(revision{main: math.MaxInt64, sub: math.MaxInt64}, max)

	keyToLease := make(map[string]lease.LeaseID)

	// restore index
	tx := s.b.BatchTx()
	tx.Lock()
	_, finishedCompactBytes := tx.UnsafeRange(metaBucketName, finishedCompactKeyName, nil, 0)
	if len(finishedCompactBytes) != 0 {
		s.compactMainRev = bytesToRev(finishedCompactBytes[0]).main
		plog.Printf("restore compact to %d", s.compactMainRev)
	}
	_, scheduledCompactBytes := tx.UnsafeRange(metaBucketName, scheduledCompactKeyName, nil, 0)
	scheduledCompact := int64(0)
	if len(scheduledCompactBytes) != 0 {
		scheduledCompact = bytesToRev(scheduledCompactBytes[0]).main
	}

	// index keys concurrently as they're loaded in from tx
	unorderedc, donec := make(chan map[string]*keyIndex), make(chan struct{})
	go func() {
		defer close(donec)
		for unordered := range unorderedc {
			// restore the tree index from the unordered index.
			for _, v := range unordered {
				s.kvindex.Insert(v)
			}
		}
	}()
	for {
		keys, vals := tx.UnsafeRange(keyBucketName, min, max, restoreChunkKeys)
		if len(keys) == 0 {
			break
		}
		// unbuffered so keys don't pile up in memory
		unorderedc <- s.restoreChunk(keys, vals, keyToLease)
		if len(keys) < restoreChunkKeys {
			// partial set implies final set
			break
		}
		// next set begins after where this one ended
		newMin := bytesToRev(keys[len(keys)-1][:revBytesLen])
		newMin.sub++
		revToBytes(newMin, min)
	}
	close(unorderedc)
	<-donec

	// keys in the range [compacted revision -N, compaction] might all be deleted due to compaction.
	// the correct revision should be set to compaction revision in the case, not the largest revision
	// we have seen.
	if s.currentRev < s.compactMainRev {
		s.currentRev = s.compactMainRev
	}
	if scheduledCompact <= s.compactMainRev {
		scheduledCompact = 0
	}

	for key, lid := range keyToLease {
		if s.le == nil {
			panic("no lessor to attach lease")
		}
		err := s.le.Attach(lid, []lease.LeaseItem{{Key: key}})
		if err != nil {
			plog.Errorf("unexpected Attach error: %v", err)
		}
	}

	tx.Unlock()

	if scheduledCompact != 0 {
		s.Compact(scheduledCompact)
		plog.Printf("resume scheduled compaction at %d", scheduledCompact)
	}

	return nil
}

func (s *store) restoreChunk(keys, vals [][]byte, keyToLease map[string]lease.LeaseID) map[string]*keyIndex {
	// assume half of keys are overwrites
	unordered := make(map[string]*keyIndex, len(keys)/2)
	for i, key := range keys {
		var kv mvccpb.KeyValue
		if err := kv.Unmarshal(vals[i]); err != nil {
			plog.Fatalf("cannot unmarshal event: %v", err)
		}
		rev := bytesToRev(key[:revBytesLen])
		s.currentRev = rev.main
		kstr := string(kv.Key)
		if isTombstone(key) {
			if ki, ok := unordered[kstr]; ok {
				ki.tombstone(rev.main, rev.sub)
			}
			delete(keyToLease, kstr)
			continue
		}
		if ki, ok := unordered[kstr]; ok {
			ki.put(rev.main, rev.sub)
		} else {
			ki = &keyIndex{key: kv.Key}
			ki.restore(revision{kv.CreateRevision, 0}, rev, kv.Version)
			unordered[kstr] = ki
		}
		if lid := lease.LeaseID(kv.Lease); lid != lease.NoLease {
			keyToLease[kstr] = lid
		} else {
			delete(keyToLease, kstr)
		}
	}
	return unordered
}

func (s *store) Close() error {
	close(s.stopc)
	s.fifoSched.Stop()
	return nil
}

func (s *store) saveIndex(tx backend.BatchTx) {
	if s.ig == nil {
		return
	}
	bs := s.bytesBuf8
	ci := s.ig.ConsistentIndex()
	binary.BigEndian.PutUint64(bs, ci)
	// put the index into the underlying backend
	// tx has been locked in TxnBegin, so there is no need to lock it again
	tx.UnsafePut(metaBucketName, consistentIndexKeyName, bs)
	atomic.StoreUint64(&s.consistentIndex, ci)
}

func (s *store) ConsistentIndex() uint64 {
	if ci := atomic.LoadUint64(&s.consistentIndex); ci > 0 {
		return ci
	}
	tx := s.b.BatchTx()
	tx.Lock()
	defer tx.Unlock()
	_, vs := tx.UnsafeRange(metaBucketName, consistentIndexKeyName, nil, 0)
	if len(vs) == 0 {
		return 0
	}
	v := binary.BigEndian.Uint64(vs[0])
	atomic.StoreUint64(&s.consistentIndex, v)
	return v
}

// appendMarkTombstone appends tombstone mark to normal revision bytes.
func appendMarkTombstone(b []byte) []byte {
	if len(b) != revBytesLen {
		plog.Panicf("cannot append mark to non normal revision bytes")
	}
	return append(b, markTombstone)
}

// isTombstone checks whether the revision bytes is a tombstone.
func isTombstone(b []byte) bool {
	return len(b) == markedRevBytesLen && b[markBytePosition] == markTombstone
}

// revBytesRange returns the range of revision bytes at
// the given revision.
func revBytesRange(rev revision) (start, end []byte) {
	start = newRevBytes()
	revToBytes(rev, start)

	end = newRevBytes()
	endRev := revision{main: rev.main, sub: rev.sub + 1}
	revToBytes(endRev, end)

	return start, end
}
