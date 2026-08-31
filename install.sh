#!/bin/sh
# Installs an outpost as a Docker Compose service and, when a setup key is
# given, lets it register itself with upcore.
#
# The script owns exactly one directory (/opt/outpost by default): a compose
# file and the volume beside it. It never touches anything else, and running it
# again over an existing install rewrites the compose file and restarts the
# service — which is also how an outpost is upgraded.
#
#   curl -fsSL https://raw.githubusercontent.com/upcore-app/outpost/main/install.sh | sudo sh -s -- \
#     --upcore-url https://status.example.com \
#     --setup-key ost_…
#
# Without --setup-key it installs a plain outpost and prints the API key to
# paste into upcore by hand.

set -eu

IMAGE="ghcr.io/upcore-app/outpost:latest"
DIR="/opt/outpost"
PORT="8080"
UPCORE_URL=""
SETUP_KEY=""
PUBLIC_URL=""
LOCATION=""
PROVIDER=""
COUNTRY=""

usage() {
	cat <<'EOF'
Usage: install.sh [options]

  --upcore-url URL   upcore instance the outpost enrolls with
  --setup-key KEY    one-time setup key from upcore (Admin → Outposts → Add)
  --public-url URL   how upcore reaches this outpost, if not <source ip>:<port>
  --location NAME    free text, e.g. Frankfurt
  --provider NAME    free text, e.g. Hetzner
  --country CODE     ISO 3166-1 alpha-2, e.g. DE
  --port PORT        host port to publish (default 8080)
  --dir PATH         install directory (default /opt/outpost)
  --image REF        container image (default ghcr.io/upcore-app/outpost:latest)
  -h, --help         this text
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

need_value() {
	[ -n "${2:-}" ] || die "$1 needs a value"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--upcore-url) need_value "$1" "${2:-}"; UPCORE_URL="$2"; shift 2 ;;
	--setup-key) need_value "$1" "${2:-}"; SETUP_KEY="$2"; shift 2 ;;
	--public-url) need_value "$1" "${2:-}"; PUBLIC_URL="$2"; shift 2 ;;
	--location) need_value "$1" "${2:-}"; LOCATION="$2"; shift 2 ;;
	--provider) need_value "$1" "${2:-}"; PROVIDER="$2"; shift 2 ;;
	--country) need_value "$1" "${2:-}"; COUNTRY="$2"; shift 2 ;;
	--port) need_value "$1" "${2:-}"; PORT="$2"; shift 2 ;;
	--dir) need_value "$1" "${2:-}"; DIR="$2"; shift 2 ;;
	--image) need_value "$1" "${2:-}"; IMAGE="$2"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $1" ;;
	esac
done

# A setup key without a URL enrolls nowhere, and a URL without a key enrolls
# nothing. Catch it here rather than in a container that then looks healthy.
if [ -n "$SETUP_KEY" ] && [ -z "$UPCORE_URL" ]; then
	die "--setup-key needs --upcore-url"
fi
if [ -n "$UPCORE_URL" ] && [ -z "$SETUP_KEY" ]; then
	die "--upcore-url needs --setup-key"
fi

command -v docker >/dev/null 2>&1 || die "docker is not installed — see https://docs.docker.com/engine/install/"

# `docker compose` is the plugin, `docker-compose` the standalone v1 binary.
if docker compose version >/dev/null 2>&1; then
	COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
	COMPOSE="docker-compose"
else
	die "neither 'docker compose' nor 'docker-compose' is available"
fi

mkdir -p "$DIR" || die "cannot create $DIR (run with sudo?)"

COMPOSE_FILE="$DIR/docker-compose.yml"
[ -f "$COMPOSE_FILE" ] && cp "$COMPOSE_FILE" "$COMPOSE_FILE.bak"

# The heredoc is quoted, so nothing in it is expanded; the environment block is
# appended afterwards with the values that were actually given.
cat >"$COMPOSE_FILE" <<EOF
# Written by install.sh. Re-run the script to change it.
services:
  outpost:
    image: $IMAGE
    restart: unless-stopped
    ports:
      - "$PORT:8080"
    volumes:
      # Keeps the outpost's own API key across restarts and image updates
      - outpost-data:/data
    environment:
      OUTPOST_ADDR: ":8080"
      OUTPOST_DATA_DIR: "/data"
EOF

append_env() {
	[ -n "$2" ] || return 0
	printf '      %s: "%s"\n' "$1" "$2" >>"$COMPOSE_FILE"
}

append_env OUTPOST_UPCORE_URL "$UPCORE_URL"
append_env OUTPOST_SETUP_KEY "$SETUP_KEY"
append_env OUTPOST_PUBLIC_URL "$PUBLIC_URL"
append_env OUTPOST_LOCATION "$LOCATION"
append_env OUTPOST_PROVIDER "$PROVIDER"
append_env OUTPOST_COUNTRY "$COUNTRY"

cat >>"$COMPOSE_FILE" <<'EOF'
    sysctls:
      # Lets the unprivileged container user open ICMP sockets, which is what
      # ping needs without root. Drop this block if your runtime rejects it.
      net.ipv4.ping_group_range: "0 2147483647"

volumes:
  outpost-data:
EOF

chmod 600 "$COMPOSE_FILE"

echo "→ pulling $IMAGE"
(cd "$DIR" && $COMPOSE pull --quiet 2>/dev/null || $COMPOSE pull)

echo "→ starting the outpost in $DIR"
(cd "$DIR" && $COMPOSE up -d)

# Give the container a moment to bind, enroll and log the outcome.
sleep 5

echo
if [ -n "$SETUP_KEY" ]; then
	echo "The outpost is starting and registering itself with $UPCORE_URL."
	echo "It appears under Admin → Outposts as soon as upcore has called it back."
	echo
	echo "If it does not, the reason is in the log:"
	echo "  cd $DIR && $COMPOSE logs -f outpost"
else
	echo "The outpost is running. Its API key:"
	echo
	(cd "$DIR" && $COMPOSE exec -T outpost cat /data/apikey 2>/dev/null) || \
		echo "  (not readable yet — try: cd $DIR && $COMPOSE exec outpost cat /data/apikey)"
	echo
	echo "Add it in upcore under Admin → Outposts → Add."
fi
echo
echo "Health check: curl -s localhost:$PORT/healthz"
