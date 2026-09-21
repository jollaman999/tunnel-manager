FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS builder

RUN apt-get update && apt-get install -y make bash

WORKDIR /go/src/github.com/jollaman999/tunnel-manager/

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

RUN make

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS prod

RUN apk --no-cache add tzdata
RUN echo "Asia/Seoul" >  /etc/timezone
RUN cp -f /usr/share/zoneinfo/Asia/Seoul /etc/localtime

WORKDIR /

COPY --from=builder /go/src/github.com/jollaman999/tunnel-manager/tunnel-manager /tunnel-manager

# root is kept on purpose. The container writes the database, the encryption
# key and the log into /data, and /data is a host bind mount
# (docker-compose.yaml). A bind mount keeps the ownership of the host
# directory, and docker creates that directory as root when it is missing, so
# any other user here fails to create the database on a first start. Moving off
# root needs the host side to hand the directory over as well, which is a
# change to the deployment, not to this file. The listening port is 8888, above
# 1024, so it is not what holds root here.
USER root

# -db is given here rather than left to the default. The default is worked out
# from os.UserConfigDir, and docker sets HOME=/root, so a container started
# without it would put the database at /root/.config/tunnel-manager inside the
# writable layer: it would come up and work, and everything in it would be gone
# the moment the container is replaced. /data is the path a deployment mounts
# (docker-compose.yaml), so naming it here is what keeps the data.
CMD ["/tunnel-manager", "-db", "/data/tunnel-manager.db"]

EXPOSE 8888
