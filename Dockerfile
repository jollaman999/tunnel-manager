FROM golang:1.27.1-bookworm AS builder

RUN apt-get update && apt-get install -y make bash

WORKDIR /go/src/github.com/jollaman999/tunnel-manager/

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

RUN make

FROM alpine:3.21.0 AS prod

RUN apk --no-cache add tzdata
RUN echo "Asia/Seoul" >  /etc/timezone
RUN cp -f /usr/share/zoneinfo/Asia/Seoul /etc/localtime

WORKDIR /

COPY --from=builder /go/src/github.com/jollaman999/tunnel-manager/tunnel-manager /tunnel-manager

USER root

# -db is given here rather than left to the default. The default is worked out
# from os.UserConfigDir, and docker sets HOME=/root, so a container started
# without it would put the database at /root/.config/tunnel-manager inside the
# writable layer: it would come up and work, and everything in it would be gone
# the moment the container is replaced. /data is the path a deployment mounts
# (docker-compose.yaml), so naming it here is what keeps the data.
CMD ["/tunnel-manager", "-db", "/data/tunnel-manager.db"]

EXPOSE 8888
