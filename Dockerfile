# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build

WORKDIR /src
# The module has no third-party dependencies, so there is nothing to download
# and no separate dependency layer worth caching.
COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/outpost .

# Alpine rather than scratch or distroless: the ping check shells out to the
# system ping binary, which needs a libc and a real filesystem underneath it.
FROM alpine:3.20

# ca-certificates for the http/ssl checks, iputils for a ping that understands
# -W as seconds (busybox's does too, but iputils behaves predictably across
# architectures). libcap is only needed to grant the capability and is dropped
# again so it cannot be used at runtime.
RUN apk add --no-cache ca-certificates iputils libcap \
 && setcap cap_net_raw+ep "$(command -v ping)" \
 && apk del libcap \
 && adduser -D -H -u 10001 outpost \
 && mkdir -p /data \
 && chown outpost:outpost /data

COPY --from=build /out/outpost /usr/local/bin/outpost

USER outpost
ENV OUTPOST_ADDR=:8080 \
    OUTPOST_DATA_DIR=/data

VOLUME ["/data"]
EXPOSE 8080

# busybox wget ships with alpine, so the healthcheck needs no extra package.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/outpost"]
