# Repository metadata

What to paste into GitHub → **About** (the gear icon next to the repo
description). Kept here so it does not drift out of the repo.

## Description

> Remote check probe for upcore — run uptime checks from multiple locations. A
> single static Go binary with no dependencies.

Alternatives, if the above reads too long in a listing:

- `Remote check probe for upcore. Run your uptime checks from multiple locations.`
- `Multi-location uptime probe for upcore. One static Go binary, zero dependencies.`

## Website

`https://github.com/upcore-app/upcore`

## Topics

```
uptime-monitoring  monitoring  golang  go  self-hosted  docker  healthcheck
observability  status-page  sre  devops  probe  upcore
```

## Settings worth turning on

- **Releases** — the image workflow only publishes on a published release.
- **Packages** — surfaces `ghcr.io/upcore-app/outpost` on the repo page.
- **Private vulnerability reporting** (Settings → Code security) — `SECURITY.md`
  sends reporters to `/security/advisories/new`, which needs this enabled.
