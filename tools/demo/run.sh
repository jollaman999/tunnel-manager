#!/usr/bin/env bash
# Records the demo GIF: builds the Host image, starts six containers of it on
# three networks of their own, starts a fresh tunnel-manager with its own data
# directory, drives the UI through Chrome and turns the frames into a GIF.
# Everything it started is stopped on the way out.
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
PREFIX=tunnel-manager-demo
UI_ADDR=127.0.0.1:8888
SERVICE_IP=127.0.0.1
SERVICE_PORTS=(8000 8001 8002)
# The ports the Hosts open for the service ports, one for each.
HOST_OPEN_PORTS=(8080 8081 8082)
FORWARD_ADDR=127.0.0.1:18080
SOCKS_ADDR=127.0.0.1:1080
MAX_GIF_BYTES=$((5 * 1024 * 1024))

# The networks, in addresses set aside for documentation. Site A is reached from
# here, as the network this system and the clients are on. Site B and the LAN
# behind its gateway are internal networks that this system has no address and
# no route on, so what is on them is reached only through the Hosts on the way.
SITE_A_NET=$PREFIX-site-a
SITE_A_SUBNET=198.51.100.0/24
SITE_B_NET=$PREFIX-site-b
SITE_B_SUBNET=203.0.113.0/24
SITE_B_LAN_NET=$PREFIX-site-b-lan
SITE_B_LAN_SUBNET=2001:db8:b::/64
NETWORKS=("$SITE_A_NET" "$SITE_B_NET" "$SITE_B_LAN_NET")

BASTION_IP=198.51.100.10
BASTION_SITE_B_IP=203.0.113.10
SITE_A_APP_IP=198.51.100.21
SITE_A_DB_IP=198.51.100.22
SITE_B_GW_IP=203.0.113.20
SITE_B_APP_IP=2001:db8:b::21
SITE_B_DB_IP=2001:db8:b::22
SITE_B_GW_LAN_IP=2001:db8:b::20

# The Hosts, in the order the recording adds them: the name each is shown by,
# where it is logged in to, and its jump route as the names of the Hosts before
# it. The SSH servers of site A let the user in from the bastion alone.
HOSTS=(bastion site-a-app site-a-db site-b-gw site-b-app site-b-db)
CONTAINERS=()
for name in "${HOSTS[@]}"; do
	CONTAINERS+=("$PREFIX-$name")
done
HOST_ADDRESSES="bastion=$BASTION_IP:22,site-a-app=$SITE_A_APP_IP:22,site-a-db=$SITE_A_DB_IP:22,site-b-gw=$SITE_B_GW_IP:22,site-b-app=[$SITE_B_APP_IP]:22,site-b-db=[$SITE_B_DB_IP]:22"
HOST_ROUTES="site-a-app=bastion,site-a-db=bastion,site-b-gw=bastion,site-b-app=bastion+site-b-gw,site-b-db=bastion+site-b-gw"
# The Hosts that pass the others on and carry no service port of their own.
HOP_HOSTS="bastion,site-b-gw"
# The Host whose ports for the service ports the clients open, the one the local
# forward and the SOCKS5 proxy are made on, and the web server that proxy opens.
SERVICE_HOST=site-a-app
FORWARD_HOST=site-b-app
SOCKS_URL="http://[$SITE_B_DB_IP]/"
# The Host on the way that is switched off for a moment.
PAUSE_HOST=site-b-gw

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

	for container in "${CONTAINERS[@]}"; do
		if docker container inspect "$container" >/dev/null 2>&1; then
			timeout 30 docker rm -f "$container" >/dev/null
		fi
	done

	for network in "${NETWORKS[@]}"; do
		if docker network inspect "$network" >/dev/null 2>&1; then
			timeout 30 docker network rm "$network" >/dev/null
		fi
	done

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

SERVICE_ADDRS=()
for port in "${SERVICE_PORTS[@]}"; do
	SERVICE_ADDRS+=("$SERVICE_IP:$port")
done

join() {
	local IFS=,
	echo "$*"
}

for addr in "$UI_ADDR" "${SERVICE_ADDRS[@]}" "$FORWARD_ADDR" "$SOCKS_ADDR"; do
	if ! port_free "$addr"; then
		echo "$addr is already in use. Stop what listens there and run this again." >&2
		exit 1
	fi
done

for container in "${CONTAINERS[@]}"; do
	if docker container inspect "$container" >/dev/null 2>&1; then
		echo "a container named $container is already there. Remove it and run this again." >&2
		exit 1
	fi
done

for network in "${NETWORKS[@]}"; do
	if docker network inspect "$network" >/dev/null 2>&1; then
		echo "a network named $network is already there. Remove it and run this again." >&2
		exit 1
	fi
done

# A route of this system inside one of the subnets would be shadowed by the
# network of the demo, or the other way round.
for subnet in "$SITE_A_SUBNET" "$SITE_B_SUBNET" "$SITE_B_LAN_SUBNET"; do
	if [ -n "$(ip route show root "$subnet")" ]; then
		echo "this system already has a route in $subnet. The demo uses that range for a network of its own." >&2
		exit 1
	fi
done

log "building tunnel-manager from $TM_SRC"
(cd "$TM_SRC" && go build -o "$WORK/tm" .)

log "building the recorder and the demo service"
(cd "$DEMO_DIR" && go build -o "$WORK/recorder" ./recorder && go build -o "$WORK/service" ./service)

log "building the Host image"
timeout 600 docker build -q -t "$IMAGE" "$DEMO_DIR" >/dev/null

log "making the networks"
timeout 30 docker network create --subnet "$SITE_A_SUBNET" "$SITE_A_NET" >/dev/null
timeout 30 docker network create --internal --subnet "$SITE_B_SUBNET" \
	-o com.docker.network.bridge.gateway_mode_ipv4=isolated "$SITE_B_NET" >/dev/null
timeout 30 docker network create --internal --ipv4=false --ipv6 --subnet "$SITE_B_LAN_SUBNET" \
	-o com.docker.network.bridge.gateway_mode_ipv6=isolated "$SITE_B_LAN_NET" >/dev/null

# start_host starts the container of the Host named, with the options given
# before the image.
start_host() {
	local name="$1"
	shift

	timeout 60 docker run -d --rm --name "$PREFIX-$name" --hostname "$name" "$@" "$IMAGE" >/dev/null
}

log "starting the Hosts"
start_host bastion \
	--network "name=$SITE_A_NET,ip=$BASTION_IP" \
	--network "name=$SITE_B_NET,ip=$BASTION_SITE_B_IP"
start_host site-a-app --network "name=$SITE_A_NET,ip=$SITE_A_APP_IP" -e "SSH_FROM=$BASTION_IP"
start_host site-a-db --network "name=$SITE_A_NET,ip=$SITE_A_DB_IP" -e "SSH_FROM=$BASTION_IP"
start_host site-b-gw \
	--network "name=$SITE_B_NET,ip=$SITE_B_GW_IP" \
	--network "name=$SITE_B_LAN_NET,ip6=$SITE_B_GW_LAN_IP"
start_host site-b-app --network "name=$SITE_B_LAN_NET,ip6=$SITE_B_APP_IP"
start_host site-b-db --network "name=$SITE_B_LAN_NET,ip6=$SITE_B_DB_IP" -e "WEB_ADDR=[::]:80"

log "starting the demo service on $(join "${SERVICE_ADDRS[@]}")"
"$WORK/service" -listen "$(join "${SERVICE_ADDRS[@]}")" >"$WORK/service.log" 2>&1 &
SERVICE_PID=$!

log "starting tunnel-manager with its data in $WORK/data"
mkdir -p "$WORK/data"
"$WORK/tm" -db "$WORK/data/tunnel-manager.db" >"$WORK/tm.log" 2>&1 &
TM_PID=$!

for ip in "$BASTION_IP" "$SITE_A_APP_IP" "$SITE_A_DB_IP"; do
	wait_for "the SSH server on $ip" 30 timeout 2 bash -c "exec 3<>/dev/tcp/$ip/22 && head -c 4 <&3 | grep -q SSH"
done
for container in "$PREFIX-site-b-gw" "$PREFIX-site-b-app" "$PREFIX-site-b-db"; do
	wait_for "the SSH server of $container" 30 timeout 5 docker exec "$container" sh -c "nc -z 127.0.0.1 22"
done
for addr in "${SERVICE_ADDRS[@]}"; do
	wait_for "the demo service on $addr" 30 timeout 2 curl -fsS "http://$addr/"
done
wait_for "tunnel-manager" 60 timeout 2 curl -kfsS "https://$UI_ADDR/ui/"
wait_for "the initial password" 30 test -s "$WORK/data/initial-password"

log "recording"
"$WORK/recorder" \
	-base "https://$UI_ADDR" \
	-initial-password-file "$WORK/data/initial-password" \
	-frames "$WORK/frames" \
	-chrome "$CHROME" \
	-service-ip "$SERVICE_IP" \
	-service-ports "$(join "${SERVICE_PORTS[@]}")" \
	-local-ports "$(join "${HOST_OPEN_PORTS[@]}")" \
	-hosts "$(join "${HOSTS[@]}")" \
	-host-addresses "$HOST_ADDRESSES" \
	-host-routes "$HOST_ROUTES" \
	-hop-hosts "$HOP_HOSTS" \
	-service-host "$SERVICE_HOST" \
	-forward-host "$FORWARD_HOST" \
	-pause-host "$PAUSE_HOST" \
	-socks-url "$SOCKS_URL" \
	-forward-port "${FORWARD_ADDR##*:}" \
	-socks-port "${SOCKS_ADDR##*:}"

log "making the GIF"
mkdir -p "$(dirname "$OUT")"
for step in 128:bayer 64:bayer 32:bayer 32:none; do
	colors=${step%:*}
	dither=${step#*:}
	if [ "$dither" = bayer ]; then
		dither=bayer:bayer_scale=5
	fi

	timeout 600 ffmpeg -v error -y -f concat -safe 0 -i "$WORK/frames/frames.txt" \
		-vf "scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=$colors:stats_mode=diff[p];[b][p]paletteuse=dither=$dither:diff_mode=rectangle" \
		-fps_mode vfr -loop 0 "$WORK/demo.gif"

	size=$(stat -c %s "$WORK/demo.gif")
	log "$colors colors, dither ${dither%%:*}: $size bytes"
	if [ "$size" -le "$MAX_GIF_BYTES" ]; then
		break
	fi
done

cp "$WORK/demo.gif" "$OUT"
log "wrote $OUT ($(stat -c %s "$OUT") bytes, $(grep -c '^duration' "$WORK/frames/frames.txt") frames)"
