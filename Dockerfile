# The builder runs on whatever machine is doing the building and not on the
# platform being built for. Nothing it does needs to run on the target: the
# program is built with CGO_ENABLED=0, so a Go toolchain cross-compiles it by
# being told where it is going. Left to run on the target instead, an image for
# another architecture would compile the whole program under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS builder

RUN apt-get update && apt-get install -y make bash

WORKDIR /go/src/github.com/jollaman999/tunnel-manager/

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# TARGETOS and TARGETARCH are set by the builder for the platform being built
# for. They are declared here rather than at the top of the stage so that what
# comes before them is cached across platforms: the module download and the
# verify are the same work whichever way this is going.
ARG TARGETOS
ARG TARGETARCH

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH make

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS prod

RUN apk --no-cache add tzdata
RUN echo "Asia/Seoul" >  /etc/timezone
RUN cp -f /usr/share/zoneinfo/Asia/Seoul /etc/localtime

WORKDIR /

COPY --from=builder /go/src/github.com/jollaman999/tunnel-manager/tunnel-manager /tunnel-manager

# The program runs as tm, 10888:10888, and not as root. The number is one that
# the usual first uids (1000, 10000, 10001) do not take, so it is unlikely to be
# an account that already means something on the host. The listening port is
# 8888, above 1024, so nothing here needs root once the program is running.
#
# The image still starts as root, because /data is a host bind mount
# (docker-compose.yaml) and a bind mount keeps the ownership of the host
# directory: docker creates it as root when it is missing, and an installation
# from before this change left its database, key and log owned by root. The
# entry script hands /data to tm and then drops to tm with su-exec, so a new
# installation and an old one both come up without anything done by hand on the
# host. A user: other than root given in docker-compose.yaml is honoured as it
# is; a user: of root still ends up running the program as tm.
RUN apk --no-cache add su-exec
RUN addgroup -S -g 10888 tm && adduser -S -D -H -h /nonexistent -s /sbin/nologin -u 10888 -G tm tm

COPY --chmod=0755 docker-entrypoint.sh /docker-entrypoint.sh

ENTRYPOINT ["/docker-entrypoint.sh"]

# -db is given here rather than left to the default. The default is worked out
# from os.UserConfigDir, which is under HOME, and HOME is nowhere near /data. For
# tm it is /nonexistent, which tm cannot create, so the startup would fail. For
# root it is /root, so the database would go to /root/.config/tunnel-manager
# inside the writable layer: it would come up and work, and everything in it
# would be gone the moment the container is replaced. /data is the path a
# deployment mounts (docker-compose.yaml), so naming it here is what keeps the
# data.
CMD ["/tunnel-manager", "-db", "/data/tunnel-manager.db"]

EXPOSE 8888
