<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-outpost-duotone.svg">
    <source media="(prefers-color-scheme: light)" srcset="assets/logo-outpost-duotone-dark.svg">
    <img src="assets/logo-outpost-duotone.svg" alt="outpost" width="440">
  </picture>
</p>

<p align="center">
  <strong>A remote check probe for <a href="https://github.com/upcore-app/upcore">upcore</a>.</strong><br>
  Monitor from more than one place. One static binary, no dependencies.
</p>

<p align="center">
  <a href="LICENSE.md"><img alt="License: FSL-1.1-ALv2" src="https://img.shields.io/badge/license-FSL--1.1--ALv2-0f766e.svg"></a>
  <img alt="Go 1.23" src="https://img.shields.io/badge/Go-1.23-00ADD8.svg">
  <img alt="Dependencies: none" src="https://img.shields.io/badge/dependencies-none-4c1.svg">
  <img alt="Image: ghcr.io/upcore-app/outpost" src="https://img.shields.io/badge/ghcr.io-upcore--app%2Foutpost-2496ED.svg">
</p>

<p align="center">
  <a href="https://upcore.app">Website</a> ·
  <a href="https://docs.upcore.app">Documentation</a> ·
  <a href="https://go.upcore.app/register">Cloud</a> ·
  <a href="https://github.com/upcore-app/upcore">upcore</a> ·
  <a href="LICENSE.md">License</a>
</p>

<p align="center">
  A partner project by <strong><a href="https://galaxybot.app">GalaxyBot</a></strong> and <strong><a href="https://onesrv.net">onesrv</a></strong>.
</p>

---

An outpost runs somewhere upcore is not — another country, another provider, the
other side of a firewall — and answers one question: *can this location reach
this target right now?* upcore dispatches a batch of checks over HTTP, the
outpost runs them concurrently and returns one result per check.

**The dispatch model is one-way.** upcore calls the outpost; the outpost does
not call back, opens no connection to upcore, and needs no inbound access to
anything but its own port. It keeps no history, no queue and no database: the
only state on disk is its API key. Losing an outpost costs you nothing but the
vantage point — the monitors, heartbeats and status pages all live in upcore.

The one exception is [auto-setup](#auto-setup): deployed with a setup key, the
outpost posts to upcore exactly once, at first start, to register itself.
Everything after that is inbound again.

Written in Go with the standard library only: no third-party modules, no
`go.sum`, a single static binary.

Not running upcore yourself? [upcore Cloud](https://go.upcore.app/register)
dispatches to your outposts the same way — the probe never needs to know which
of the two it is talking to. The documentation for both lives at
[docs.upcore.app](https://docs.upcore.app).

## Quick start

The short path is to let upcore write the deploy command for you: **Admin →
Outposts → Add** creates a one-time setup key and hands you an install script
line, a `docker run`, a compose file or a Kubernetes manifest with the key
already in it. Run it, and the outpost registers itself — see
[Auto-setup](#auto-setup).

### As a systemd service, without Docker

The install script downloads one binary, installs it as a systemd service and
waits until the outpost has registered itself:

```bash
curl -fsSL https://raw.githubusercontent.com/upcore-app/outpost/main/install.sh | sudo sh -s -- \
  --upcore-url https://status.example.com \
  --setup-key ost_… \
  --location Frankfurt
```

It owns these paths and nothing else:

| Path | What |
| --- | --- |
| `/usr/local/bin/outpost` | the binary |
| `/usr/local/bin/outpost-update` | the updater, run as root by systemd |
| `/usr/local/lib/outpost/install.sh` | a copy of this script, for the updater |
| `/etc/outpost/outpost.env` | the configuration (`0640`, readable by the service user) |
| `/etc/systemd/system/outpost.service` | the unit |
| `/etc/systemd/system/outpost-update.{service,path}` | the updater and its trigger |
| `/var/lib/outpost` | the API key and the enrollment marker (`0700`) |

Running it again upgrades in place: the binary is replaced, the data directory
is left alone, and any option not given again is carried over from the existing
configuration. So a bare `install.sh` is the upgrade command, and because the
API key lives in the data directory, upcore keeps talking to the same outpost.

The last three paths are what makes the **Aktualisieren** button in upcore work
— it runs exactly this upgrade, from the other side of a file the unprivileged
service is allowed to write. See [Updating](#updating).

```bash
systemctl status outpost      # is it running
journalctl -u outpost -f      # what is it doing
sudo sh install.sh --uninstall   # remove everything but /var/lib/outpost
```

Linux on amd64 or arm64, with systemd. Anything else takes the container image.

### With Docker

```bash
docker compose up -d
docker compose logs outpost | grep opk_
```

The second command prints the API key the outpost generated on first start. Copy
it into upcore (see [Wiring it into upcore](#wiring-it-into-upcore)) — the key is
printed once, in a banner, and never again.

### Verify either one

```bash
curl -s localhost:8080/healthz
curl -s -H "Authorization: Bearer opk_…" localhost:8080/v1/info
```

### From source

```bash
go build -o outpost .
OUTPOST_DATA_DIR=./data OUTPOST_LOCATION=Frankfurt ./outpost
```

## Configuration

Every setting is an environment variable; there is no config file. Values are
trimmed, and an unusable one is corrected with a warning rather than refusing to
start.

| Variable | Default | Notes |
| --- | --- | --- |
| `OUTPOST_ADDR` | `:8080` | Listen address |
| `OUTPOST_LOCATION` | `""` | Free text, e.g. `Frankfurt`. Shown in upcore |
| `OUTPOST_PROVIDER` | `""` | Free text, e.g. `Hetzner` |
| `OUTPOST_COUNTRY` | `""` | ISO 3166-1 alpha-2, e.g. `DE`. Upper-cased; anything but two letters is ignored with a warning |
| `OUTPOST_DATA_DIR` | `/data` | Where `apikey` is stored |
| `OUTPOST_API_KEY` | `""` | Pin the key instead of generating one. Must match `opk_<8 hex>_<48 hex>` |
| `OUTPOST_MAX_CONCURRENCY` | `50` | Probes in flight at once, clamped to [1, 500] |
| `OUTPOST_MAX_CHECKS` | `200` | Checks per request, clamped to [1, 1000] |
| `OUTPOST_LOG_REQUESTS` | `true` | One log line per request |
| `OUTPOST_UPDATE_ENABLED` | `true` | Whether upcore may trigger an upgrade. `false` turns `POST /v1/update` into a 501 whatever is installed on the host |
| `OUTPOST_UPDATE_COMMAND` | `""` | Run this instead of the built-in path. The escape hatch for a deployment that is neither a systemd install nor a container — see [Updating](#updating) |
| `OUTPOST_UPCORE_URL` | `""` | upcore instance to enroll with, e.g. `https://status.example.com` |
| `OUTPOST_SETUP_KEY` | `""` | One-time setup key from upcore (`ost_…`). Needs `OUTPOST_UPCORE_URL` |
| `OUTPOST_PUBLIC_URL` | `""` | Where upcore reaches this outpost. Defaults to the source address of the enrollment plus `OUTPOST_ADDR`'s port |

`location`, `provider` and `country` are metadata only. They change nothing
about how checks run; upcore reads them from `/v1/info` to label the probe.

## Auto-setup

An outpost deployed with `OUTPOST_UPCORE_URL` and `OUTPOST_SETUP_KEY` registers
itself instead of being typed in. In upcore, **Admin → Outposts → Add** mints the
key and shows the deploy command with it already filled in.

What happens on first start:

1. The outpost resolves its API key as always — generated and stored in the data
   directory (`/data` in the container, `/var/lib/outpost` under systemd), or
   pinned with `OUTPOST_API_KEY`.
2. It waits for its own listener, then posts once to
   `POST <upcore>/api/outposts/enroll`:
   `{"token": "ost_…", "apiKey": "opk_…", "port": 8080, "addresses": [...]}`,
   plus `"url"` when `OUTPOST_PUBLIC_URL` is set. `addresses` are this host's own
   globally routable interface addresses — private, loopback, link-local and
   carrier-grade NAT ranges are left out, because none of them tells upcore
   anything about where to call back.
3. **upcore calls back.** It asks `GET /v1/info` with the key it was just given,
   trying every address the outpost could be at — the one it reported, and the
   source address of the enrollment — repeatedly, for about twenty seconds. A
   container that has only just started is a moving target, so one attempt
   against one guessed address is not enough.
4. The outpost writes an `enrolled` marker into its data directory and never
   enrolls again. The setup key is single use and expires after 24 hours.

**A callback that does not answer no longer throws the enrollment away.** upcore
answers `202` instead of `201`, creates the outpost anyway, and shows it in the
admin list as *wird geprüft* with the reason underneath — `connection refused`,
`timeout`, whichever it was. It keeps retrying in the background, so opening the
port is enough to make it go green; nothing has to be deployed again. Until it
verifies, no checks are dispatched to it, so an unreachable probe cannot become
a location that silently never votes.

upcore tries the candidates in that order: an explicit `OUTPOST_PUBLIC_URL`
first, then the addresses the outpost reported about itself, and only last the
source address of the enrollment. That order matters — behind a CDN or a reverse
proxy the source address is the edge that forwarded the request, not the probe.
An enrollment through Cloudflare arrives from a Cloudflare address, and without
the reported list upcore would record *that* as the outpost.

What none of it can guess is a NATed or port-forwarded deployment: the outpost
then only sees an RFC 1918 address, which is correctly left out of the list, and
the source address is the NAT gateway rather than a published port. Set the URL
explicitly there:

```
OUTPOST_PUBLIC_URL: https://fra.example.com
```

Failures are retried with a growing backoff for about eight minutes, except the
three that no retry can fix — an unknown, spent or expired key. Whatever
happens, the outpost keeps serving: a probe that could not enroll is still a
working probe, and it can be added by hand.

In Kubernetes, pin `OUTPOST_API_KEY` in the secret. A pod has no volume to keep
a generated key in, so without it every restart would invent a new key while
upcore still presents the old one.

## The API key

The outpost has exactly one credential, resolved at startup in this order:

1. `OUTPOST_API_KEY`, if set and well-formed. It is used as-is and never written
   to disk.
2. `<OUTPOST_DATA_DIR>/apikey`, if it exists and parses. This is why the compose
   file mounts a volume: it keeps the outpost's identity stable across restarts
   and image updates.
3. Otherwise a fresh key is generated, written with mode `0600`, and printed
   once in a banner.

If the data dir is not writable the outpost still starts, but with an in-memory
key that changes on every restart — it says so loudly in the log.

Finding the key again:

```bash
docker compose logs outpost | grep opk_        # only right after first start
docker compose exec outpost cat /data/apikey   # any time
```

To rotate a key, delete `/data/apikey` and restart, or set `OUTPOST_API_KEY` to
a value you generated yourself. Then update the outpost in upcore.

## Updating

upcore's admin list shows each probe's version and marks the ones behind the
newest release. Where the deployment supports it, an **Aktualisieren** button
does the upgrade from there.

**The outpost never replaces its own binary.** The systemd service runs as an
unprivileged user under `ProtectSystem=strict` with only its data directory
writable — deliberately, because a probe that could rewrite `/usr/local/bin/
outpost` would turn any bug in a check into persistence. So the request crosses
the privilege boundary as a file:

1. `POST /v1/update` writes `<data dir>/update-request` — the one directory the
   service may write — and answers `202`.
2. `outpost-update.path`, a systemd path unit, notices the file.
3. `outpost-update.service` runs as root, removes the request, validates the
   version, and re-runs the stored copy of `install.sh`.
4. The installer downloads the release, **verifies its published checksum**,
   replaces the binary and restarts the service.
5. The new process compares the version it started as with the one recorded in
   `update.json`. That comparison — not the installer's exit code — is what
   makes the upgrade "done", and it is reported on the next `GET /v1/info`.

`install.sh` writes all of that; an outpost installed before this existed picks
it up by running the script once more. The installer it re-runs is the local
copy at `/usr/local/lib/outpost/install.sh`, not a fresh download: a remotely
triggered root script should run the code that was reviewed when the outpost was
installed. Re-running `install.sh` by hand is what refreshes it.

Watch it happen:

```bash
journalctl -u outpost-update -f
```

### The other deployments

| `updateMethod` | What it means |
| --- | --- |
| `systemd` | The units above are installed. The button works. |
| `command` | `OUTPOST_UPDATE_COMMAND` is set and is run instead, with `OUTPOST_TARGET_VERSION` and `OUTPOST_CURRENT_VERSION` in its environment. |
| `docker` | A container. It cannot meaningfully replace its own image, so the button is not offered — `docker compose pull && docker compose up -d`. |
| `none` | Nothing is wired up, or `OUTPOST_UPDATE_ENABLED=false`. |

`OUTPOST_UPDATE_COMMAND` is the escape hatch for a Kubernetes rollout, a
configuration-management run, or anything else that owns the lifecycle. Note
that a command which restarts this service kills its own caller — systemd stops
the whole control group — which is fine: the next start works out what happened
from the recorded version. It is also why the systemd path uses a separate unit
rather than a child process.

The version to install is only ever passed on as a release tag, and is checked
against `^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$` in the Go handler *and* again in
the root-run script. A check on the other side of a file is not a check.

## API

All requests and responses are JSON. Authentication is a single API key,
presented as either header:

```
Authorization: Bearer opk_…
X-API-Key: opk_…
```

Failures return `401` with `WWW-Authenticate: Bearer`. Malformed requests return
`400`, unknown paths `404`, wrong methods `405` — always as `{"error":"…"}`.
Request bodies are limited to 1 MiB.

### `GET /healthz` — no auth

The container healthcheck and whatever load balancer sits in front of the
outpost. It reveals nothing but liveness.

```bash
curl -s localhost:8080/healthz
```

```json
{"status":"ok","version":"0.1.0"}
```

### `GET /v1/info` — auth required

How upcore verifies a key and prefills the outpost's metadata in its admin UI.

```bash
curl -s -H "Authorization: Bearer opk_…" localhost:8080/v1/info
```

```json
{
  "version": "0.1.0",
  "location": "Frankfurt",
  "provider": "Hetzner",
  "country": "DE",
  "checkTypes": ["http", "keyword", "ping", "tcp", "dns", "ssl"],
  "maxConcurrency": 50,
  "maxChecks": 200,
  "startedAt": "2026-08-31T18:00:00Z",
  "uptimeSeconds": 1234,
  "updateMethod": "systemd",
  "canUpdate": true,
  "update": {
    "state": "done",
    "fromVersion": "0.1.0",
    "targetVersion": "v0.2.0",
    "startedAt": "2026-09-04T10:12:00Z",
    "finishedAt": "2026-09-04T10:12:44Z",
    "message": "Aktualisiert von 0.1.0 auf 0.2.0."
  }
}
```

`updateMethod` is `systemd`, `command`, `docker` or `none`, and `canUpdate` is
not implied by it: a container names its method and still cannot replace its own
image. `updateHint` — present only when there is no button to offer — is the
sentence upcore shows in its place. See [Updating](#updating).

### `POST /v1/update` — auth required

Asks the outpost to upgrade itself. An empty `version` means the newest release.

```bash
curl -s -X POST localhost:8080/v1/update \
  -H "Authorization: Bearer opk_…" \
  -H "Content-Type: application/json" \
  -d '{"version": "v0.2.0"}'
```

```json
{
  "accepted": true,
  "method": "systemd",
  "update": {"state": "running", "fromVersion": "0.1.0", "targetVersion": "v0.2.0",
             "startedAt": "2026-09-04T10:12:00Z"}
}
```

`202` on acceptance — and that is all it can be, because a successful upgrade
restarts this process out from under the response. The outcome is read back from
`GET /v1/info` afterwards; the state survives the restart in a file. `409` means
an update is already running, `501` that this deployment has no way to carry one
out, `400` that the version is not shaped like a release tag.

### `POST /v1/checks` — auth required

Runs a batch. Results come back in request order, one per requested check.

```bash
curl -s -X POST localhost:8080/v1/checks \
  -H "Authorization: Bearer opk_…" \
  -H "Content-Type: application/json" \
  -d '{
    "checks": [
      {"id": "17", "type": "http", "target": "https://api.example.com/health", "timeout": 10,
       "httpMethod": "GET", "httpHeaders": [{"name": "Authorization", "value": "Bearer x"}]},
      {"id": "18", "type": "keyword", "target": "https://example.com", "keyword": "ok"},
      {"id": "19", "type": "ping", "target": "example.com"},
      {"id": "20", "type": "tcp", "target": "db.internal", "port": 5432},
      {"id": "21", "type": "dns", "target": "example.com", "dnsRecordType": "A"},
      {"id": "22", "type": "ssl", "target": "example.com", "port": 443}
    ]
  }'
```

```json
{
  "results": [
    {"id": "17", "status": 1, "latency": 42, "message": "200 OK"},
    {"id": "18", "status": 1, "latency": 87, "message": "200 · Keyword gefunden"},
    {"id": "19", "status": 1, "latency": 12, "message": "Pong"},
    {"id": "20", "status": 1, "latency": 3, "message": "Port 5432 offen"},
    {"id": "21", "status": 1, "latency": 8, "message": "1 × A: 93.184.216.34"},
    {"id": "22", "status": 1, "latency": 61, "message": "Gültig, läuft in 74 Tagen ab"}
  ]
}
```

Result messages are German because upcore's UI copy is German; they are meant to
be displayed unchanged.

#### Request fields

| Field | Applies to | Notes |
| --- | --- | --- |
| `id` | all | Opaque string chosen by upcore, echoed back verbatim. Required, non-empty, max 64 characters, unique within a batch |
| `type` | all | `http`, `keyword`, `ping`, `tcp`, `dns` or `ssl` |
| `target` | all | URL for `http`/`keyword`; for the rest, a host — a full URL is accepted and reduced to its host |
| `timeout` | all | Seconds, clamped to [1, 120]. Missing or `0` means 10 |
| `httpMethod` | `http`, `keyword` | Default `GET`. One of `GET HEAD POST PUT PATCH DELETE OPTIONS` |
| `httpHeaders` | `http`, `keyword` | `[{"name":…,"value":…}]`, may be null or absent, first 20 used. Entries with an empty name, or a value containing CR/LF, are skipped |
| `keyword` | `keyword` | The string to look for in the body |
| `port` | `tcp`, `ssl` | Required for `tcp`; defaults to 443 for `ssl` |
| `dnsRecordType` | `dns` | Default `A`. One of `A AAAA CNAME MX NS TXT` |

#### Result fields

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | The id from the request |
| `status` | integer | `1` = up, `0` = down. Never any other value |
| `latency` | integer or null | Milliseconds; `null` when the probe produced no measurement |
| `message` | string | Short, human-readable, always present |

#### What each check does

- **http** — sends the request, follows redirects, drains the body. Up when the
  status code is in `[200, 400)`. Message is `"<code> <status text>"`.
- **keyword** — like `http`, but reads up to 2 MiB of the body. An error status
  is down regardless of the body; otherwise the verdict is whether the body
  contains the keyword.
- **ping** — one ICMP echo via the system `ping` binary (never a shell), with the
  round-trip time parsed from its output. Down as `"Timeout"` or
  `"Host unreachable"`.
- **tcp** — a TCP connect to `host:port`, closed immediately. Latency is the
  connect time.
- **dns** — resolves the requested record type against the host's own resolvers.
  Zero records is down; otherwise the message reports the count and the first
  record.
- **ssl** — a TLS handshake, then a verdict of our own: the chain is verified
  against the system roots plus the presented intermediates, including a
  hostname match. An expired or untrusted certificate is reported with its
  reason rather than as a connection error, and a valid one reports the days
  remaining.

Errors that make a single check unusable — an unknown type, an empty target, a
`tcp` check without a port — are reported as a down result for that check. The
batch still succeeds. Only errors that make a result unattributable (a missing,
oversized or duplicate `id`) reject the whole request with `400`.

## ICMP and ping

`ping` needs a raw or ICMP socket. In practice:

- **Docker grants `NET_RAW` by default**, and the image runs `setcap
  cap_net_raw+ep` on the ping binary, so ping usually works with no extra
  configuration.
- If your runtime drops capabilities, add `--cap-add=NET_RAW` (or
  `cap_add: [NET_RAW]` in compose).
- Alternatively, allow unprivileged ICMP sockets on the host:
  `sysctl -w net.ipv4.ping_group_range="0 2147483647"`. The compose file sets
  this per container; some runtimes (Docker Desktop on macOS and Windows) reject
  the sysctl, in which case remove that block.

Every other check type works without any of this — if ICMP is impossible in your
environment, use a `tcp` check instead of a `ping` check.

## Wiring it into upcore

With [auto-setup](#auto-setup) steps 1–4 are the deploy command; skip to the
check strategy below. By hand:

1. Deploy the outpost where you want to measure from, and make its port
   reachable by upcore. Put TLS in front of it if it crosses the internet: the
   API key is a bearer token.
2. Grab the key: `docker compose exec outpost cat /data/apikey`.
3. In upcore: **Admin → Outposts → Add → Manual**. Give it a name, the base URL
   (e.g. `https://fra.example.com`) and the API key; location, provider and
   country are prefilled from what the outpost reports about itself.
4. Hit **Test connection**. upcore calls `GET /v1/info` with the key — that is
   the whole handshake, and it is the point at which a wrong URL, a wrong key or
   a firewall shows up rather than three days later in a false incident.
5. Then, per monitor, choose the **check strategy**:
   - `local` — upcore checks it itself. The default; outposts are ignored.
   - `outposts` — only the assigned outposts check it.
   - `both` — upcore and the assigned outposts each check it.
6. Assign the outposts that monitor should be checked from, and pick the **down
   policy**, which decides how the votes combine:
   - `any` — one location reporting down is enough. Catches regional outages
     first, and is the noisiest.
   - `majority` — more than half the voting locations must agree.
   - `all` — every voting location must be down. Quietest, and slowest to alarm.

upcore dispatches the checks in batches and stores the results as heartbeats,
exactly as it does for locally run checks.

**A location upcore cannot reach does not vote.** If an outpost is down, being
rebuilt, or simply unreachable, it is left out of the tally instead of counting
as a `down` — so an outpost failing never raises an incident for the target it
was watching. With `all` and every outpost unreachable there is nothing left to
vote at all, and the monitor keeps its previous state rather than flipping.

## Development

```bash
go vet ./...
go build ./...
go test ./...       # offline: the tests use httptest, never the network
go run .            # OUTPOST_DATA_DIR=./data is handy outside a container
```

The layout:

```
main.go                 config → server → graceful shutdown
internal/config         environment parsing, clamping, warnings
internal/apikey         key format, constant-time comparison, resolution
internal/check          check types, dispatcher, concurrency limiter, probes
internal/server         routes, handlers, auth/logging/recovery middleware
internal/enroll         the one outbound call: auto-setup against upcore
```

Images are built and pushed to GHCR only when a GitHub release is published; a
push to a branch runs `go vet`, `go build` and `go test` and stops there.

## Security

The API key is a bearer token and the outpost will connect to whatever target
upcore names, so where you put it and who can reach it matters. See
[SECURITY.md](SECURITY.md) for the threat model, how to harden a deployment, and
how to report a vulnerability privately.

Please do not open a public issue for a security problem.

## License

[Functional Source License 1.1 with an Apache 2.0 future grant](LICENSE.md)
(FSL-1.1-ALv2) — the same license as upcore.

Use it, modify it, run it yourself, for anything that is not a competing
commercial product or service. Two years after each version is published it
becomes available under the Apache License 2.0.

Copyright 2026 GalaxyBot &amp; onesrv.
