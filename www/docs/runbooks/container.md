---
id: container
title: Runbook — the container
sidebar_position: 6
---

# Runbook: the container

:::tip Try the systemd service first

[Install on a Linux VM](./linux-service.md) is the simpler path and the one to reach for. This page is worth it
when you already run everything in compose and want docpreview to look the same.

The container gives you less isolation than it appears to: the daemon talks to the **host's** docker socket, so a
build container is a sibling on the host, not a child of this one. What you get is packaging — and two extra
failure modes, both below.

:::

`Dockerfile` and `docker-compose.yml` run the daemon and its two shares as three containers from one image. The
image carries the `docpreview` binary, `git`, and the docker **CLI** — no docker daemon, because it uses the host's.

Two facts decide everything else on this page. The daemon runs docker builds, and it needs the host's docker daemon
to agree with it about paths.

## Step 1 — Understand the two rules before you change anything

:::danger The data directory is mounted at the same path inside and outside the container

`/srv/docpreview:/srv/docpreview`. This is not tidiness. The daemon asks the *host's* docker daemon to bind-mount
each cloned workspace into a build container, so the path it passes is resolved out there, not inside.

Mount the data somewhere else internally and every build gets an empty `/workspace`. That surfaces as a missing
`package.json`, which sends whoever debugs it looking at the repository rather than at the mount. If you change the
host side, change the container side, `DOCPREVIEW_CONFIG` and `data_dir` together.

:::

:::danger The share containers join the daemon's network namespace

`network_mode: "service:docpreview"` is what makes `127.0.0.1:8471` mean the daemon to them, and it is what keeps
the daemon unreachable from anywhere else.

`webhook-only` refuses a non-loopback `-upstream` — forwarding elsewhere would make it a relay — so it cannot be
pointed at a bridge-network hostname. And the daemon's admin surface is gated on the request arriving over
loopback: on a bridge network it would be reachable from every other container there. See
[Security](../reference/security.md).

:::

## Step 2 — Mount the host's docker socket

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
```

Read-write, necessarily: creating containers and removing cache volumes are writes.

:::warning This is root on the host

Control of the docker socket is control of the machine. It is the same trade docker-in-docker makes with
`--privileged`, so neither option is a boundary — the socket is chosen because it shares the host's image cache,
which keeps a restart from re-pulling `node:24-bookworm`, and because the build cache volumes then survive the
container. Run this on a host dedicated to docpreview.

The container runs as root for the same reason: the socket is mode 0660 and root-owned, the host's docker group id
differs per machine, and dropping privileges would give up nothing an attacker could not take back through the
socket.

:::

Confirm the container can reach the daemon before going further:

```powershell
docker run --rm -v //var/run/docker.sock:/var/run/docker.sock `
  --entrypoint docker docpreview:latest version
```

Two version blocks, a client and a server, is the answer you want. A client block alone means the socket is not
mounted.

## Step 3 — Prepare the data directory on the host

```powershell
docker compose run --rm docpreview vault keygen -out /srv/docpreview/master.key
docker compose run --rm docpreview init -config /srv/docpreview/config.yml -exposer zrok2 -yes
```

`-yes` takes every default rather than asking, because `compose run` is a poor place to answer questions. `init`
writes commented YAML you are then expected to edit, which is Step 3's other half.

Then edit `config.yml`: set `data_dir: /srv/docpreview/data` and point `vault.key_source` at the key. Both paths
must be the shared path from Step 1.

:::note The key does not have to live in the data directory

Nor should it: that directory also holds the vault, and a backup of one folder that contains both the ciphertext
and the key protecting it is not a backup, it is a copy of your credentials. Mount the key separately — as a
secret, or from a directory of its own.

:::

## Step 4 — Give it a zrok environment

The image has no zrok CLI, so the environment cannot be enabled from inside it. Enable it on the host, then place
the resulting directory in the volume the containers mount at `/root/.zrok2`:

```powershell
zrok2 enable <account token>
docker run --rm -v zrok:/dest -v "$env:USERPROFILE\.zrok2:/src:ro" `
  --entrypoint sh docpreview:latest -c "cp -a /src/. /dest/"
```

See [Runbook — zrok v2](./zrok2.md) for what the environment is and why losing it is not recoverable from the new
machine.

## Step 5 — Start it

```powershell
docker compose up -d
```

The daemon starts first; the two shares forward to it and log errors until it answers, which is expected for the
first few seconds. The image's healthcheck polls `/healthz` rather than `/status`, because `/healthz` answers while
the daemon is still recovering — a healthcheck on `/status` would kill a container that is doing its job.

Watch recovery finish:

```powershell
docker compose logs -f docpreview
```

## Step 6 — Confirm a real build

The mount rule in Step 1 fails only at build time, so nothing before this proves it. Trigger a build — push to a
tracked pull request, or press Rebuild on the dashboard — and read the log.

A build that reports a missing `package.json` in a repository that has one is the mount, not the repository: the
path the daemon passed does not exist on the host. Recheck that both sides of the data mount are identical.

## What must persist

| | |
|---|---|
| `/srv/docpreview` | the database, the vault, artifacts, build logs |
| the `zrok` volume | the environment identity that owns every reserved name |
| the cache volumes | `docpreview-cache-*`, on the host's daemon, so they survive by construction |

## Upgrading

```powershell
docker compose build
docker compose up -d
```

Compose stops the old containers before starting the new ones, which is what the one-daemon-per-account rule
requires. Never run the new image alongside the old instance — see
[Runbook — move an installation](./move-an-installation.md).
