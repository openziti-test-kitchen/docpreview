---
id: container
title: Run in a container
sidebar_position: 7
---

# Run in a container

:::tip Try the systemd service first

[Install on a Linux VM](./linux-service.md) is the simpler path. Use this page when you already run everything in
compose and want NetFoundry Preview to look the same.

The container isolates less than it seems to. The daemon talks to the **host's** docker socket, so a build
container is a sibling on the host, not a child of this one. What you get is packaging, plus the two failure modes
described below.

:::

`Dockerfile` and `docker-compose.yml` run the daemon and its two shares as three containers from one image. The
image carries the `docpreview` binary, `git` and the docker **CLI**. It has no docker daemon of its own. It uses the
host's.

Two facts drive the rest of this page. The daemon runs docker builds, and the host's docker daemon has to agree
with it about paths.

## Step 1: Understand the two rules before you change anything

:::danger The data directory is mounted at the same path inside and outside the container

`/srv/docpreview:/srv/docpreview`. The daemon asks the *host's* docker daemon to bind-mount each cloned workspace
into a build container, and the host resolves that path, not the container.

Mount the data somewhere else internally and every build gets an empty `/workspace`. You see a missing
`package.json` and go looking at the repository instead of the mount. If you change the host side, change the
container side, `DOCPREVIEW_CONFIG` and `data_dir` with it.

:::

:::danger The share containers join the daemon's network namespace

`network_mode: "service:docpreview"` makes `127.0.0.1:8471` mean the daemon to them. It also keeps the daemon
unreachable from anywhere else.

`webhook-only` refuses a non-loopback `-upstream`, because forwarding elsewhere would make it a relay. So you can't
point it at a bridge-network hostname. The daemon's admin surface also requires the request to arrive over
loopback, and on a bridge network every other container there could reach it. See
[Security](../reference/security.md).

:::

## Step 2: Mount the host's docker socket

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
```

It has to be read-write. Creating containers and removing cache volumes are writes.

:::warning This is root on the host

Whoever controls the docker socket controls the machine. Docker-in-docker with `--privileged` is the same trade,
so neither is a boundary. The socket wins because it shares the host's image cache, so a restart does not re-pull
`node:24-bookworm`, and the build cache volumes outlive the container. Run this on a host that does nothing else.

The container runs as root for the same reason. The socket is root-owned and mode 0660, the docker group id
differs per machine, and anything you gave up by dropping privileges an attacker could take back through the
socket.

:::

Check that the container can reach the docker daemon before you go further:

```powershell
docker run --rm -v //var/run/docker.sock:/var/run/docker.sock `
  --entrypoint docker docpreview:latest version
```

You want two version blocks, a client and a server. A client block alone means the socket is not mounted.

## Step 3: Prepare the data directory on the host

```powershell
docker compose run --rm docpreview vault keygen -out /srv/docpreview/master.key
docker compose run --rm docpreview init -config /srv/docpreview/config.yml -exposer zrok2 -yes
```

`-yes` takes every default without asking, since `compose run` is a poor place to answer questions. `init` writes
commented YAML for you to edit.

Edit `config.yml`: set `data_dir: /srv/docpreview/data` and point `vault.key_source` at the key. Both paths must be
the shared path from Step 1.

:::note Keep the key out of the data directory

That directory also holds the vault. A backup of one folder holding both the ciphertext and its key is a copy of
your credentials. Mount the key separately, as a secret or from a directory of its own.

:::

## Step 4: Give it a zrok environment

The image has no zrok CLI, so you can't enable the environment from inside it. Enable it on the host, then copy
the result into the volume the containers mount at `/root/.zrok2`. Run the copy through `docker compose` so it
lands in the project's own `zrok` volume, which compose names after the project (for example `docpreview_zrok`):

```powershell
zrok2 enable <account token>
docker compose run --rm -v "$env:USERPROFILE\.zrok2:/src:ro" `
  --entrypoint sh docpreview -c "cp -a /src/. /root/.zrok2/"
```

A plain `docker run -v zrok:/dest` fills a different volume called `zrok`, which none of the three services mount.

See [Publish previews with zrok v2](./zrok2.md) for what the environment is and why losing it can't be undone from
the new machine.

## Step 5: Start it

```powershell
docker compose up -d
```

The daemon starts first. The two shares forward to it and log errors for the first few seconds until it answers.
The image's healthcheck polls `/healthz`, not `/status`, because `/healthz` answers while the daemon is still
recovering. A healthcheck on `/status` would kill a container that is working.

Watch recovery finish:

```powershell
docker compose logs -f docpreview
```

## Step 6: Confirm a real build

The mount rule from Step 1 only fails at build time, so nothing so far proves it. Trigger a build by pushing to a
tracked pull request or pressing Rebuild on the dashboard, then read the log.

If a build reports a missing `package.json` in a repository that has one, the mount is wrong, not the repository.
The path the daemon passed does not exist on the host. Check that both sides of the data mount are identical.

## What must persist

| | |
|---|---|
| `/srv/docpreview` | the database, the vault, artifacts, build logs |
| the `zrok` volume | the environment identity that owns every reserved name |
| the cache volumes | `docpreview-cache-*`, on the host's daemon, so they survive anyway |

## Upgrading

```powershell
docker compose build
docker compose up -d
```

Compose stops the old containers before it starts the new ones, which the one-daemon-per-environment rule needs.
Never run the new image alongside the old instance. See [Move an installation](./move-an-installation.md).
