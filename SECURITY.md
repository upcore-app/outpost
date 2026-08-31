# Security Policy

An outpost is a small thing with an unusually sharp edge: it holds one bearer
token, and whoever presents it can make the outpost open a connection to any
host the outpost can reach. That is not a flaw, it is the entire product — but
it means the key is, in effect, network access to wherever you deployed the
probe. Read [Understanding the blast radius](#understanding-the-blast-radius)
before you put one inside a network you care about.

## Supported versions

Fixes go onto the latest release. There are no long-lived maintenance branches,
so upgrading to the current release is the supported way to receive a fix.

| version | supported |
| --- | --- |
| latest release | ✅ |
| older releases | ❌ — upgrade |
| `main` branch | ✅ for reports, but it is not a release |

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion for a
vulnerability.**

Report it privately through GitHub:

1. Go to <https://github.com/upcore-app/outpost/security/advisories/new>
   (repository → **Security** → **Report a vulnerability**).
2. Describe the issue, the impact, and how to reproduce it.

That opens a private advisory only you and the maintainers can read.

Useful in a report:

- The affected version or commit, and whether it runs from the published image
  or a local build.
- A minimal reproduction — a request body, a config, a proof of concept.
- What an attacker gains: reaching a host they should not, escaping the check
  they were given, recovering the key, running code.
- Whether it needs the API key, or works without one.

If the issue is in how upcore *dispatches* to an outpost rather than in the
outpost itself, report it at
<https://github.com/upcore-app/upcore/security/advisories/new> instead. When in
doubt, either is fine — we will move it.

### What to expect

- **Acknowledgement within 72 hours** that the report arrived.
- An initial assessment — accepted, needs more information, or out of scope —
  within 7 days.
- A fix released as soon as it is ready. We aim for 90 days at the outside and
  will tell you if something takes longer.
- Credit in the advisory and the release notes, unless you would rather not be
  named.

Please give us a reasonable window to ship a fix before disclosing publicly.

## Understanding the blast radius

The outpost exists to reach things. A holder of the API key can name any target
in a check and read back whether the connection succeeded, how long it took, and
a short message — including, for a `keyword` check, whether an arbitrary string
appears in the response body. **That is a deliberate, documented capability, not
a vulnerability**, and it has three consequences worth stating plainly:

- **The key is as valuable as the outpost's network position.** An outpost on a
  public VPS can probe the internet. An outpost inside your VPC can probe your
  VPC. Deploy accordingly.
- **The outpost is an oracle, not a proxy.** It never returns a response body,
  only a status, a latency and a short message. `keyword` narrows that to one
  bit per request about a string you chose, which is a slow but real read
  primitive against anything the outpost can reach.
- **Losing the key is not losing your monitoring data.** The outpost stores no
  history, no monitor list and no credentials of yours. The blast radius is
  network reach, plus the key itself.

Anything that lets someone do *more* than the above — without the key, or beyond
the check they were handed — is a vulnerability. Please report it.

## Scope

**In scope** — anything that breaks one of these boundaries:

- Authentication: reaching `/v1/info` or `/v1/checks` without a valid key,
  recovering the key from a response, a timing side channel in the comparison,
  or the key appearing in a log, an error or a header.
- Escaping the check: turning a check descriptor into command execution,
  argument injection into the `ping` binary, a path traversal, a request smuggled
  onto a connection the descriptor did not name, or a redirect chain that reaches
  somewhere the target did not.
- Parser and memory safety: a crash, hang or unbounded allocation triggered by a
  request body, a check descriptor, or a hostile response from a checked target
  (an oversized body, a malicious certificate, a crafted DNS answer).
- The TLS verdict: making an expired, self-signed or hostname-mismatched
  certificate report as `Gültig`.
- Key handling on disk: `/data/apikey` written with weaker permissions than
  `0600`, or readable by another container or user in a supported deployment.
- Container issues: privilege escalation inside the image, escape, or writes
  outside `OUTPOST_DATA_DIR`.

**Out of scope:**

- **The outpost connecting to a host named in a check.** That is what it does.
  See [Understanding the blast radius](#understanding-the-blast-radius). If you
  can make it reach a host *without* the key, or make it do something other than
  a check against that host, that is in scope.
- **No rate limiting.** The outpost does not throttle requests or checks. It
  bounds concurrency (`OUTPOST_MAX_CONCURRENCY`), checks per request
  (`OUTPOST_MAX_CHECKS`) and body size (1 MiB), but a key holder can issue
  requests as fast as they like. Put a proxy in front if that matters.
- **The version string on `/healthz`.** That endpoint is unauthenticated so a
  load balancer and the container healthcheck can use it; it reports liveness and
  a version and nothing else. The disclosure is deliberate.
- **Plaintext HTTP on an outpost deployed without TLS in front of it.** See
  *Hardening* — that is a deployment decision, and the README says to put TLS
  there. Tell us anyway if you can chain it into something worse.
- Denial of service through sheer volume, and anything requiring physical access
  or a compromised host.
- Reports produced by a scanner with no demonstrated impact.
- Social engineering of maintainers or users.

Test against **your own outpost**. Do not test against a probe you do not
operate, and never use someone else's outpost to reach a third party.

## How the outpost protects itself

Context for a report, and for anyone auditing a deployment.

- **One credential, no accounts.** The key is `opk_<8 hex>_<48 hex>` — a
  non-secret prefix and 24 random bytes from `crypto/rand`. It is compared with
  `crypto/subtle.ConstantTimeCompare` and **never logged, not even masked**: a
  near-miss in a log is a working key for whoever reads the log. Both
  `Authorization: Bearer` and `X-API-Key` are accepted, so a proxy that injects
  its own `Authorization` header cannot lock you out.
- **The key at rest** lives at `<OUTPOST_DATA_DIR>/apikey`, written `0600` in a
  directory created `0700`. Set `OUTPOST_API_KEY` instead and nothing is written
  to disk at all.
- **`ping` never touches a shell.** The host is the final argument of an
  `exec.CommandContext` argv, and it is validated against a character class that
  excludes a leading `-`, so a target cannot be read by `ping` as an option.
- **TLS is verified, deliberately by hand.** The `ssl` check dials with
  `InsecureSkipVerify` and then runs `x509.Certificate.Verify` itself against the
  system roots, the presented intermediates and the hostname. This is not a
  disabled check — it is the only way to report *why* a certificate is bad
  instead of failing the connection and reporting nothing. A certificate that
  does not verify is reported as down.
- **Bounded everywhere.** 1 MiB request bodies (`http.MaxBytesReader`),
  `OUTPOST_MAX_CHECKS` per request, `OUTPOST_MAX_CONCURRENCY` probes in flight,
  2 MiB read from a `keyword` body, per-check timeouts clamped to 120 s, and a
  batch context that cannot outlive the server's write timeout.
- **A panic is contained.** The recovery middleware is the outermost layer, so
  one malformed request cannot take down a probe other monitors depend on.
- **Nothing else persists.** No database, no queue, no history, no inbound
  connections to upcore. An outpost is disposable by design.
- **The access log records** method, path, status, duration and remote address —
  never the key, never the check targets.
- **The image runs as a non-root user.** The only capability it wants is
  `NET_RAW`, and only for `ping`; every other check works without it.

## Hardening a deployment

The defaults are safe for the intended shape — one container, reachable by your
upcore instance and by nothing else. These are the decisions that are yours:

- **Terminate TLS in front of the outpost.** The API key is a bearer token: over
  plain HTTP across the internet it is readable by anyone on the path, and
  replayable. This is the single most important item on this list.
- **Do not expose the outpost to the internet.** Restrict it at the firewall,
  the reverse proxy or the security group to the address your upcore instance
  calls from. `/healthz` is the only endpoint that needs to be reachable more
  widely, and only if a load balancer requires it.
- **Choose the network position on purpose.** A key holder can probe whatever
  the outpost can. Putting a probe inside a sensitive network is a legitimate
  thing to want — just do it knowingly, and segment it so it reaches the hosts
  you intend to monitor and not the rest.
- **Keep the data volume.** Without it the key is regenerated on every restart
  and upcore stops being able to reach the probe until you paste the new one.
  Alternatively pin `OUTPOST_API_KEY` from your secret manager and mount no
  volume at all — that is the better fit for immutable or scale-to-zero deploys.
- **Rotate deliberately.** Delete `/data/apikey` and restart, or set a new
  `OUTPOST_API_KEY`, then update the outpost in upcore. There is one key and no
  overlap window, so the probe is unreachable between the two steps — upcore
  treats an unreachable outpost as *not voting*, so this does not raise a false
  incident.
- **Give each outpost its own key.** They are generated independently; never
  copy one key across probes. A single leaked key should cost you one vantage
  point, not all of them.
- **Watch the logs after first start.** The generated key is printed once, in a
  banner. If your log pipeline ships container output somewhere central, that
  banner goes with it — rotate the key if that is not somewhere you would keep a
  credential.
- **Pin the image tag** you deploy, and update deliberately. `latest` moves.

## Security advisories

Published advisories are listed at
<https://github.com/upcore-app/outpost/security/advisories>. Watch the
repository for releases to be notified when a fix ships.
