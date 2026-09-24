#!/usr/bin/env bash
# Records the demo GIF: builds the Host image, starts a container of it, starts
# a fresh tunnel-manager with its own data directory, drives the UI through
# Chrome and turns the frames into a GIF. Everything it started is stopped on
# the way out.
#
#   tools/demo/run.sh [output.gif]
#
# The output defaults to docs/demo.gif. TM_SRC names another tree to build
# tunnel-manager from, CHROME another Chrome executable, and DEMO_WORK_PARENT
# the directory the work files are put under.
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$DEMO_DIR/../.." && pwd)"
OUT="${1:-$REPO_DIR/docs/demo.gif}"
TM_SRC="${TM_SRC:-$REPO_DIR}"
CHROME="${CHROME:-/usr/bin/google-chrome}"

IMAGE=tunnel-manager-demo-host
CONTAINER=tunnel-manager-demo-host
UI_ADDR=127.0.0.1:8888
SERVICE_ADDR=127.0.0.1:8000
HOST_IP=127.0.0.2
HOST_SSH_PORT=2222
HOST_OPEN_PORT=8080
FORWARD_ADDR=127.0.0.1:18080
MAX_GIF_BYTES=$((5 * 1024 * 1024))

WORK="$(mktemp -d "${DEMO_WORK_PARENT:-${TMPDIR:-/tmp}}/tunnel-manager-demo.XXXXXX")"
TM_PID=""
SERVICE_PID=""

log() {
	printf '== %s\n' "$*"
}

cleanup() {
	set +e

	for pid in "$TM_PID" "$SERVICE_PID"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid"
			wait "$pid" 2>/dev/null
		fi
	done

	if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
		timeout 30 docker rm -f "$CONTAINER" >/dev/null
	fi

	log "work files are left in $WORK"
}
trap cleanup EXIT

port_free() {
	! ss -Hltn "sport = :${1##*:}" | awk '{print $4}' | grep -qE "^(${1%:*}|0\.0\.0\.0|\*|\[::\]):${1##*:}$"
}

# wait_for runs the command given until it succeeds, once a second, for at most
# the number of tries given.
wait_for() {
	local what="$1" tries="$2"
	shift 2

	for _ in $(seq 1 "$tries"); do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done

	echo "gave up waiting for $what" >&2
	return 1
}

for addr in "$UI_ADDR" "$SERVICE_ADDR" "$HOST_IP:$HOST_SSH_PORT" "$HOST_IP:$HOST_OPEN_PORT" "$FORWARD_ADDR"; do
	if ! port_free "$addr"; then
		echo "$addr is already in use. Stop what listens there and run this again." >&2
		exit 1
	fi
done

if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
	echo "a container named $CONTAINER is already there. Remove it and run this again." >&2
	exit 1
fi

log "building tunnel-manager from $TM_SRC"
(cd "$TM_SRC" && go build -o "$WORK/tm" .)

log "building the recorder and the demo service"
(cd "$DEMO_DIR" && go build -o "$WORK/recorder" ./recorder && go build -o "$WORK/service" ./service)

log "building the Host image"
timeout 600 docker build -q -t "$IMAGE" "$DEMO_DIR" >/dev/null

log "starting the Host"
timeout 60 docker run -d --rm --name "$CONTAINER" \
	-p "$HOST_IP:$HOST_SSH_PORT:22" \
	-p "$HOST_IP:$HOST_OPEN_PORT:$HOST_OPEN_PORT" \
	"$IMAGE" >/dev/null

log "starting the demo service on $SERVICE_ADDR"
"$WORK/service" -listen "$SERVICE_ADDR" >"$WORK/service.log" 2>&1 &
SERVICE_PID=$!

log "starting tunnel-manager with its data in $WORK/data"
mkdir -p "$WORK/data"
"$WORK/tm" -db "$WORK/data/tunnel-manager.db" >"$WORK/tm.log" 2>&1 &
TM_PID=$!

wait_for "the Host SSH server" 30 timeout 2 bash -c "exec 3<>/dev/tcp/$HOST_IP/$HOST_SSH_PORT && head -c 4 <&3 | grep -q SSH"
wait_for "the demo service" 30 timeout 2 curl -fsS "http://$SERVICE_ADDR/"
wait_for "tunnel-manager" 60 timeout 2 curl -kfsS "https://$UI_ADDR/ui/"
wait_for "the initial password" 30 test -s "$WORK/data/initial-password"

log "recording"
"$WORK/recorder" \
	-base "https://$UI_ADDR" \
	-initial-password-file "$WORK/data/initial-password" \
	-frames "$WORK/frames" \
	-chrome "$CHROME" \
	-service-ip "${SERVICE_ADDR%:*}" \
	-service-port "${SERVICE_ADDR##*:}" \
	-local-port "$HOST_OPEN_PORT" \
	-host-ip "$HOST_IP" \
	-host-port "$HOST_SSH_PORT" \
	-service-url "http://$HOST_IP:$HOST_OPEN_PORT/" \
	-forward-port "${FORWARD_ADDR##*:}"

log "making the GIF"
mkdir -p "$(dirname "$OUT")"
for colors in 128 64 32; do
	timeout 600 ffmpeg -v error -y -f concat -safe 0 -i "$WORK/frames/frames.txt" \
		-vf "scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=$colors:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
		-fps_mode vfr -loop 0 "$WORK/demo.gif"

	size=$(stat -c %s "$WORK/demo.gif")
	log "$colors colors: $size bytes"
	if [ "$size" -le "$MAX_GIF_BYTES" ]; then
		break
	fi
done

cp "$WORK/demo.gif" "$OUT"
log "wrote $OUT ($(stat -c %s "$OUT") bytes, $(grep -c '^duration' "$WORK/frames/frames.txt") frames)"
