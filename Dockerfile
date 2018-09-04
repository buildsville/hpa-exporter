FROM golang:alpine AS builder

RUN apk update && \
    apk add git build-base && \
    rm -rf /var/cache/apk/* && \
    mkdir -p "$GOPATH/src/github.com/buildsville/" && \
    git clone https://github.com/buildsville/hpa-exporter.git && \
    mv hpa-exporter "$GOPATH/src/github.com/buildsville/" && \
    cd "$GOPATH/src/github.com/buildsville/hpa-exporter" && \
    GOOS=linux GOARCH=amd64 go build -o /hpa-exporter

FROM alpine:3.7

RUN apk add --update ca-certificates

COPY --from=builder /hpa-exporter /hpa-exporter

ENTRYPOINT ["/hpa-exporter"]
