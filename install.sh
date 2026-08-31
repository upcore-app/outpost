#!/bin/sh
# Installs an outpost as a systemd service: one binary, no Docker, and — when a
# setup key is given — an outpost that registers itself with upcore before this
# script has finished printing.
#
#   curl -fsSL https://raw.githubusercontent.com/upcore-app/outpost/main/install.sh | sudo sh -s -- \
#     --upcore-url https://status.example.com \
#     --setup-key ost_…
#
# The script owns exactly four paths and nothing else:
#
#   /usr/local/bin/outpost            the binary
#   /etc/outpost/outpost.env          the configuration
#   /etc/systemd/system/outpost.service
#   /var/lib/outpost                  the API key and the enrollment marker
#
# Running it again is how an outpost is upgraded: the binary is replaced, the
# data directory is left alone, and any setting not given again is carried over
# from the existing configuration. The API key therefore survives an upgrade,
# which is what keeps upcore talking to the same outpost afterwards.
#
# Without --setup-key it installs a plain outpost and prints the API key to
# paste into upcore by hand.

set -eu

REPO="upcore-app/outpost"
BIN="/usr/local/bin/outpost"
ETC_DIR="/etc/outpost"
ENV_FILE="$ETC_DIR/outpost.env"
DATA_DIR="/var/lib/outpost"
UNIT="/etc/systemd/system/outpost.service"
SERVICE_USER="outpost"

VERSION=""
PORT=""
UPCORE_URL=""
SETUP_KEY=""
PUBLIC_URL=""
LOCATION=""
PROVIDER=""
COUNTRY=""
UNINSTALL="no"

usage() {
	cat <<'EOF'
Usage: install.sh [options]

  --upcore-url URL   upcore instance the outpost enrolls with
  --setup-key KEY    one-time setup key from upcore (Admin → Outposts → Add)
  --public-url URL   how upcore reaches this outpost, if not <source ip>:<port>
  --location NAME    free text, e.g. Frankfurt
  --provider NAME    free text, e.g. Hetzner
  --country CODE     ISO 3166-1 alpha-2, e.g. DE
  --port PORT        port to listen on (default 8080)
  --version TAG      release to install (default: the latest one)
  --uninstall        stop the service and remove everything this script wrote
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
	--version) need_value "$1" "${2:-}"; VERSION="$2"; shift 2 ;;
	--uninstall) UNINSTALL="yes"; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $1" ;;
	esac
done

# Argument mistakes are worth catching before the root check: a typo should not
# have to be rediscovered under sudo.
#
# A setup key without a URL enrolls nowhere, and a URL without a key enrolls
# nothing. Catch it here rather than in a service that then looks healthy.
if [ "$UNINSTALL" = "no" ]; then
	if [ -n "$SETUP_KEY" ] && [ -z "$UPCORE_URL" ]; then
		die "--setup-key needs --upcore-url"
	fi
	if [ -n "$UPCORE_URL" ] && [ -z "$SETUP_KEY" ]; then
		die "--upcore-url needs --setup-key"
	fi
fi

[ "$(id -u)" -eq 0 ] || die "run this as root (sudo sh -s -- …)"
command -v systemctl >/dev/null 2>&1 || die "this system does not run systemd — use the container image instead"

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

if [ "$UNINSTALL" = "yes" ]; then
	systemctl disable --now outpost.service >/dev/null 2>&1 || true
	rm -f "$UNIT"
	systemctl daemon-reload
	rm -f "$BIN"
	rm -rf "$ETC_DIR"
	echo "→ removed the service, the binary and $ETC_DIR"
	echo "  $DATA_DIR was kept: it holds the API key upcore knows this outpost by."
	echo "  Remove it with: rm -rf $DATA_DIR"
	exit 0
fi

# ---------------------------------------------------------------------------
# Carry the existing configuration over
# ---------------------------------------------------------------------------

# An upgrade is `install.sh` with no arguments, so anything not given again has
# to come from the file that is already there. Read in a subshell-free way: the
# file is written by this script and is plain KEY="value".
if [ -f "$ENV_FILE" ]; then
	# shellcheck disable=SC1090
	. "$ENV_FILE"
	[ -n "$UPCORE_URL" ] || UPCORE_URL="${OUTPOST_UPCORE_URL:-}"
	[ -n "$PUBLIC_URL" ] || PUBLIC_URL="${OUTPOST_PUBLIC_URL:-}"
	[ -n "$LOCATION" ] || LOCATION="${OUTPOST_LOCATION:-}"
	[ -n "$PROVIDER" ] || PROVIDER="${OUTPOST_PROVIDER:-}"
	[ -n "$COUNTRY" ] || COUNTRY="${OUTPOST_COUNTRY:-}"
	if [ -z "$PORT" ] && [ -n "${OUTPOST_ADDR:-}" ]; then
		PORT="${OUTPOST_ADDR##*:}"
	fi
	# The setup key is deliberately *not* carried over: it is single use, and a
	# spent key in the configuration is only a secret with nothing left to open.
fi
[ -n "$PORT" ] || PORT="8080"

# ---------------------------------------------------------------------------
# Download
# ---------------------------------------------------------------------------

command -v curl >/dev/null 2>&1 || die "curl is required"

case "$(uname -s)" in
Linux) ;;
*) die "$(uname -s) is not supported — the service is Linux only" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) ARCH="amd64" ;;
aarch64 | arm64) ARCH="arm64" ;;
*) die "unsupported architecture: $(uname -m)" ;;
esac

# The redirect on /releases/latest names the newest tag without spending one of
# the unauthenticated API's sixty requests per hour.
if [ -z "$VERSION" ]; then
	LATEST_URL=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null) ||
		die "cannot reach github.com to find the latest release — pass --version"
	VERSION="${LATEST_URL##*/}"
	case "$VERSION" in
	"" | *releases*) die "cannot determine the latest release — pass --version" ;;
	esac
fi

ASSET="outpost_linux_${ARCH}"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d)
# The trap covers every exit, including the `die`s below: a half-downloaded
# binary in /tmp is litter, and on a failed upgrade it is confusing litter.
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "→ downloading outpost $VERSION ($ARCH)"
curl -fsSL "$BASE/$ASSET" -o "$TMP/outpost" ||
	die "cannot download $BASE/$ASSET — does that release have Linux binaries?"

# Checksums are published beside the binaries. A missing file is a hard failure:
# skipping the check on a download that came over the network unverified is
# exactly the wrong thing to be lenient about.
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt" ||
	die "cannot download $BASE/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	EXPECTED=$(awk -v want="$ASSET" '$2 == want || $2 == "*" want { print $1 }' "$TMP/checksums.txt")
	[ -n "$EXPECTED" ] || die "$ASSET is not listed in checksums.txt"
	ACTUAL=$(sha256sum "$TMP/outpost" | awk '{ print $1 }')
	[ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch for $ASSET — refusing to install"
	echo "→ checksum ok"
else
	die "sha256sum is required to verify the download"
fi

chmod 755 "$TMP/outpost"

# ---------------------------------------------------------------------------
# User, directories, configuration
# ---------------------------------------------------------------------------

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
	# --system: no password ageing, no login shell, an id below the human range.
	useradd --system --no-create-home --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null ||
		adduser --system --no-create-home --home "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null ||
		die "cannot create the '$SERVICE_USER' system user"
	echo "→ created the '$SERVICE_USER' system user"
fi

mkdir -p "$DATA_DIR" "$ETC_DIR"
chown "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
# 0700: the data dir holds the API key, which is the only thing between the
# internet and a probe that will connect anywhere it is told to.
chmod 700 "$DATA_DIR"
chmod 755 "$ETC_DIR"

write_env() {
	printf '%s="%s"\n' "$1" "$2"
}

# Written fresh every run, from the arguments merged with what was there before.
umask 077
{
	echo "# Written by install.sh. Re-run the script to change it."
	write_env OUTPOST_ADDR ":$PORT"
	write_env OUTPOST_DATA_DIR "$DATA_DIR"
	[ -n "$UPCORE_URL" ] && write_env OUTPOST_UPCORE_URL "$UPCORE_URL"
	[ -n "$SETUP_KEY" ] && write_env OUTPOST_SETUP_KEY "$SETUP_KEY"
	[ -n "$PUBLIC_URL" ] && write_env OUTPOST_PUBLIC_URL "$PUBLIC_URL"
	[ -n "$LOCATION" ] && write_env OUTPOST_LOCATION "$LOCATION"
	[ -n "$PROVIDER" ] && write_env OUTPOST_PROVIDER "$PROVIDER"
	[ -n "$COUNTRY" ] && write_env OUTPOST_COUNTRY "$COUNTRY"
	true
} >"$ENV_FILE"
umask 022

chown "root:$SERVICE_USER" "$ENV_FILE" 2>/dev/null || true
# The setup key lives in here, so the service user may read it and nobody else.
chmod 640 "$ENV_FILE"

cat >"$UNIT" <<EOF
# Written by install.sh. Re-run the script to change it.
[Unit]
Description=upcore outpost
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=$SERVICE_USER
Group=$SERVICE_USER
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=always
RestartSec=5s

# The ping check runs the system ping, which needs CAP_NET_RAW. Granting it
# ambiently means it works even where /bin/ping carries no file capabilities,
# and bounding the set means this is the only privilege the service can hold.
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW
NoNewPrivileges=yes

# A probe reads nothing and writes only its own state directory.
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=$DATA_DIR
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
# AF_INET/AF_INET6 for the checks, AF_NETLINK because ping asks the kernel for
# its route before it sends anything.
RestrictAddressFamilies=AF_INET AF_INET6 AF_NETLINK AF_UNIX

[Install]
WantedBy=multi-user.target
EOF

chmod 644 "$UNIT"

# ---------------------------------------------------------------------------
# Install and start
# ---------------------------------------------------------------------------

# Replace the binary with a rename rather than a copy: an overwrite in place
# would corrupt the running process's image, a rename only unlinks it.
install -m 755 "$TMP/outpost" "$BIN.new"
mv -f "$BIN.new" "$BIN"

systemctl daemon-reload
systemctl enable outpost.service >/dev/null 2>&1 || true
systemctl restart outpost.service

# ---------------------------------------------------------------------------
# Report what actually happened
# ---------------------------------------------------------------------------

# Wait for the outpost to answer its own health endpoint. Failing here is worth
# saying out loud: everything below depends on the service being up.
echo "→ waiting for the outpost to come up"
HEALTHY="no"
i=0
while [ "$i" -lt 30 ]; do
	if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
		HEALTHY="yes"
		break
	fi
	sleep 1
	i=$((i + 1))
done

echo
if [ "$HEALTHY" != "yes" ]; then
	echo "The service did not answer on http://127.0.0.1:$PORT/healthz."
	echo
	systemctl --no-pager --lines=20 status outpost.service || true
	echo
	echo "Full log: journalctl -u outpost -f"
	exit 1
fi

echo "outpost $VERSION is running as a systemd service."
echo

if [ -n "$SETUP_KEY" ]; then
	# The Go side writes this marker once upcore has accepted the enrollment,
	# so it is the one signal that says the registration went through rather
	# than merely that the process is alive.
	echo "→ registering with $UPCORE_URL"
	i=0
	while [ "$i" -lt 60 ]; do
		if [ -f "$DATA_DIR/enrolled" ]; then
			echo
			echo "Registered. It is listed in upcore under Admin → Outposts."
			echo
			sed 's/^/  /' "$DATA_DIR/enrolled"
			echo
			echo "If upcore shows it as \"wird geprüft\", it has not reached this host yet:"
			echo "  open port $PORT for upcore, or re-run with --public-url."
			echo "  upcore keeps retrying on its own — nothing needs to be run again."
			exit 0
		fi
		sleep 1
		i=$((i + 1))
	done

	echo
	echo "The outpost is running but has not registered yet. The reason is in the log:"
	echo "  journalctl -u outpost -n 50"
	echo
	echo "It keeps retrying for a few minutes. A setup key is valid for 24 hours,"
	echo "so re-running this script with the same key is safe."
else
	echo "Its API key:"
	echo
	if [ -f "$DATA_DIR/apikey" ]; then
		sed 's/^/  /' "$DATA_DIR/apikey"
	else
		echo "  (not written yet — try: cat $DATA_DIR/apikey)"
	fi
	echo
	echo "Add it in upcore under Admin → Outposts → Add."
fi

echo
echo "Health check: curl -s localhost:$PORT/healthz"
echo "Logs:         journalctl -u outpost -f"
