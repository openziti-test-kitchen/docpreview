# docpreview, as a container.
#
# The daemon runs docker builds, so the interesting part of this file is not what it
# installs — it is that the container must reach a docker daemon and must agree with it
# about paths. Both are the deployment's job rather than the image's; see the header of
# docker-compose.yml and docs/design/20-container.md.

# ── Build ────────────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Dependencies before source, so a change to the code does not re-download the module
# cache. This layer changes only when go.mod or go.sum does.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off deliberately. The sqlite driver is modernc.org/sqlite, which is pure Go — so
# there is nothing to link against and the result runs on any glibc or musl base. Linking
# a C sqlite here would tie the image to the builder's libc for no gain.
#
# -trimpath so the binary does not carry /src paths, which end up in stack traces and in
# error messages an operator reads.
#
# Only ./cmd/docpreview. Every subcommand — serve, webhook-only, dashboard-only, vault,
# doctor — is in this one binary, which is what lets the three containers below share one
# image and differ only in their command.
RUN CGO_ENABLED=0 go build -trimpath -o /out/docpreview ./cmd/docpreview

# ── Runtime ──────────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

# git, because the pipeline clones with the git binary rather than a Go implementation —
# it is what supports partial clone, and the whole point of `--depth 1` is not fetching
# history nobody reads.
#
# ca-certificates, because every outbound call is TLS: the GitHub and Bitbucket APIs, the
# zrok controller, and the git clone itself.
#
# curl only for the healthcheck below. It is 250 KB and the alternative is a health
# subcommand that exists to be run by docker, which is a worse trade.
RUN apt-get update \
 && apt-get install --no-install-recommends -y ca-certificates git curl \
 && rm -rf /var/lib/apt/lists/*

# The docker CLI, and only the CLI.
#
# The pipeline shells out to `docker` — create, start, logs, wait, volume rm — so the
# binary has to be here. There is no docker *daemon* in this image and there must not be:
# the deployment mounts the host's socket, which is the whole design. Copied from Docker's
# own published CLI image rather than installed from an apt repository, because it pins one
# file at one version instead of adding a third-party source to the image.
#
# Pinned to a major, and kept at or above the engine you point it at. A newer engine with an
# older client works — the API is versioned and negotiated — but the reverse eventually is
# not, and the failure lands in the middle of a build log as an API version error rather than
# anywhere that suggests the client.
COPY --from=docker:29-cli /usr/local/bin/docker /usr/local/bin/docker

COPY --from=build /out/docpreview /usr/local/bin/docpreview

# Root, on purpose, and this is the one thing about the image worth arguing with.
#
# The mounted docker socket is root-owned and mode 0660 on every distribution, so a
# non-root process cannot use it without being in the host's docker group — and that group
# id differs per host, which makes a baked-in `--group-add` wrong somewhere. Since access
# to that socket is already equivalent to root on the host, dropping privileges inside the
# container buys nothing it does not immediately hand back. Run this on a host you are
# willing to treat as dedicated to it.
USER root

# DOCPREVIEW_CONFIG is the one environment variable the binary reads (main.go), so the
# subcommands need no -config and a deployment that moves the file has one thing to change.
#
# The directory it lives in must be mounted at the same path inside and outside the
# container — see the path note in docker-compose.yml. That is a property of the deployment,
# which is why the path is only a default here.
ENV DOCPREVIEW_CONFIG=/srv/docpreview/config.yml
VOLUME ["/srv/docpreview"]

# The daemon binds loopback by design and the admin surface refuses anything else, so this
# port is documentation rather than something to publish. Reaching the dashboard from
# another machine is what `dashboard-only` is for.
EXPOSE 8471

# /healthz rather than /status: it answers while the daemon is still recovering, which is
# exactly when a healthcheck on /status would kill a container that is doing its job.
# The start period covers the recovery itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8471/healthz || exit 1

ENTRYPOINT ["docpreview"]
# No -config: DOCPREVIEW_CONFIG above is the default path, and passing the flag as well
# would mean two places to change when it moves. Without either, `serve` reads
# ~/.docpreview/config.yml — a different file with its own empty database, which starts
# happily, restores nothing, and looks exactly like data loss.
CMD ["serve"]
