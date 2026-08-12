# The image carries the Go binary AND the Claude CLI, because the decision was to use
# the subscription rather than the per-token API. `serve` raises the daemon; every
# other subcommand is run by `dokku run` against this same image, sharing the
# persistent volume.
#
# The daemon also raises one ephemeral container per analysis, and that
# container runs this same image — which is why the CLI and the Go binary ship
# together.

# ---------- build ----------
FROM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off: modernc.org/sqlite is pure Go, so the binary is static and the runtime
# image needs no C toolchain.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nightwatcher ./cmd/nightwatcher

# ---------- runtime ----------
FROM debian:12-slim

# Pin the CLI. Native installs auto-update in the background by default, which is
# exactly what you do not want in production.
ARG CLAUDE_VERSION=2.1.227

# The deploy host drops container egress to everything but DNS and 443, so apt over
# its default http:// mirrors times out at build time. The certificates come from the
# build stage because debian:12-slim ships none, and apt cannot fetch the
# ca-certificates package over https without already having them — the chicken and egg
# that makes copying them the only way in.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN rm -f /etc/apt/sources.list.d/debian.sources \
 && printf '%s\n' \
      'deb https://deb.debian.org/debian bookworm main' \
      'deb https://deb.debian.org/debian bookworm-updates main' \
      'deb https://deb.debian.org/debian-security bookworm-security main' \
      > /etc/apt/sources.list

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git ripgrep docker.io \
 && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 10001 -s /bin/bash nightwatcher
USER nightwatcher
WORKDIR /home/nightwatcher

RUN curl -fsSL https://claude.ai/install.sh | bash -s ${CLAUDE_VERSION}

ENV PATH="/home/nightwatcher/.local/bin:${PATH}" \
    DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    USE_BUILTIN_RIPGREP=0

RUN claude --version

COPY --from=build /out/nightwatcher /usr/local/bin/nightwatcher

ENV NIGHTWATCHER_DB=/data/nightwatcher.db \
    NIGHTWATCHER_CLONES=/data/clones \
    PORT=5000

EXPOSE 5000

ENTRYPOINT ["/usr/local/bin/nightwatcher"]
CMD ["serve"]
