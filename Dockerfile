FROM golang:1.23.0-bookworm AS builder

RUN apt-get update && apt-get install -y make bash

WORKDIR /go/src/github.com/jollaman999/tunnel-manager/

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

RUN make

FROM alpine:3.21.0 as prod

RUN apk --no-cache add tzdata
RUN echo "Asia/Seoul" >  /etc/timezone
RUN cp -f /usr/share/zoneinfo/Asia/Seoul /etc/localtime

COPY --from=builder /go/src/github.com/jollaman999/tunnel-manager/config/config.yaml /config.yaml
COPY --from=builder /go/src/github.com/jollaman999/tunnel-manager/tunnel-manager /tunnel-manager

USER root
CMD ["/tunnel-manager"]

EXPOSE 8083
