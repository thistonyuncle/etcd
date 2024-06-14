FROM golang
ADD . /go/src/github.com/thistonyuncle/etcd
ADD cmd/vendor /go/src/github.com/thistonyuncle/etcd/vendor
RUN go install github.com/thistonyuncle/etcd
EXPOSE 2379 2380
ENTRYPOINT ["etcd"]
