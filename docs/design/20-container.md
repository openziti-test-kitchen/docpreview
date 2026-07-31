# 20 — Running docpreview in a container

Status: **describes code that exists** — `Dockerfile`, `docker-compose.yml`, `.dockerignore`.

This document is the reasoning. `docker-compose.yml` is the deployment, and its header carries the
operator-facing version of the two rules below.

## The problem containerising this has that most services do not

docpreview *runs docker builds*. Every other decision here follows from that one sentence: the daemon
shells out to `docker create`, `docker start`, `docker logs`, `docker wait` and `docker volume rm`, and it
asks for a bind mount of the workspace it just cloned. So the container needs a docker daemon, and it
needs to agree with that daemon about paths.

## Why the host's socket, and not docker-in-docker

Both were considered. Neither is a security boundary — that is the part worth stating first, because the
socket looks worse than it is and DinD looks better than it is:

- `-v /var/run/docker.sock:/var/run/docker.sock` is control of the host's daemon, which is root on the
  host.
- Docker-in-docker needs `--privileged`, which is also root on the host.

With security equal, the difference is operational, and it goes one way:

| | host socket | docker-in-docker |
|---|---|---|
| image cache | shared with the host, warm across restarts | its own, empty after every restart |
| first build after a restart | unchanged | plus a `node:24-bookworm` pull |
| the cache volumes (`docpreview-cache-*`) | already work; they are daemon-side names | die with the DinD container unless `/var/lib/docker` is itself a volume |
| storage driver | the host's | overlay on overlay |
| visibility | `docker ps` on the host shows build containers | invisible without exec'ing in |
| the workspace bind mount | **needs the path rule below** | works, all paths internal |

DinD's only real advantage is isolating the build daemon from the host's, and `--privileged` hands that
back immediately. So: the socket.

## The path rule, which is the one that breaks builds

`pipeline.hostMountPath` spells the workspace directory the way the docker daemon can see it — and the
daemon resolves that path in *its* filesystem, not the caller's. On Linux it returns the path unchanged.
That is correct on a host and wrong in a container whose data directory is mounted somewhere else,
because `/data/workspaces/…` does not exist out where the daemon looks.

**So the data directory is bind-mounted at the same path inside and outside the container.**
`/srv/docpreview:/srv/docpreview`. It is not tidiness; it is what makes the sibling mount resolve.

The failure when it is wrong is bad in a specific way: the build container gets an *empty* `/workspace`,
so the build fails with a missing `package.json` and everybody debugging it looks at the repository. It
does not look like a mount problem, which is why this is the first thing in the compose header.

The same rule covers the reverse direction: the daemon reads the build *output* back out of that
directory to publish it, so both processes have to mean the same bytes by the same path.

### The alternative, not taken yet

Move the workspace onto a named volume and mount it into the build container the way `cacheMounts`
already does, with the daemon mounting the same volume. That removes host-path coupling entirely, makes
the image portable to a remote or rootless daemon — and, on the evidence of this project's own
measurements, is probably *faster*: it is the last bind mount in the build path, and taking `node_modules`
off one took an install from 5m46s to 14s (`internal/pipeline/dockermount.go`, `CacheVolume`).

It is a real change to `build.go` and `clone.go`, and it gives up being able to look at a workspace from
the host. Worth doing; deliberately not bundled with getting the image to exist.

## Three containers, one image, one network namespace

The workstation runs three processes — `serve`, `webhook-only`, `dashboard-only` — for reasons in
`docs/design/14-production-deployment.md` and `cmd/docpreview/CLAUDE.md`: sharing the daemon directly
would publish every route it serves, including `/api/secrets`, whose write gate only checks that the
daemon's listeners are loopback. They are.

The compose file reproduces that exactly, and the two share containers use
`network_mode: "service:docpreview"`. This is load-bearing twice over:

- `webhook-only` **refuses** a non-loopback `-upstream` — forwarding elsewhere would make it a relay —
  so it cannot be pointed at a bridge-network hostname.
- The daemon's admin surface is gated on `isLocalRequest`. A bridge network would make the daemon
  reachable from every other container on it, which is the hole `Available()` cannot see (a loopback
  listener says nothing about who can dial it — see `internal/daemon/CLAUDE.md`).

Sharing the namespace means `127.0.0.1:8471` means the daemon to all three, and nothing else can reach
it.

## What must persist, and what happens if it does not

| | why |
|---|---|
| the data directory | the database, the vault file, workspaces, artifacts, build logs |
| `~/.zrok2` | the **environment identity**. It owns every reserved name. A fresh environment does not merely lose the URLs — it orphans them on the old account, where nothing running can reclaim them |
| the cache volumes | daemon-side, so they survive the container by construction |

## One daemon per exposer account, and what that means for a migration

`Reap` runs at startup on the documented reasoning that everything it finds is an orphan of a previous
process. That is true for one daemon per account and false for two: **two daemons sharing one zrok
account delete each other's live shares.**

So a migration is stop, move, start — never an overlap. Starting the container while the old instance is
still running is not a cutover, it is both of them tearing down each other's previews while every log
looks healthy.

Moving an instance therefore means moving the data directory *and* the zrok environment, together, with
nothing running in between. That is what an import/export command is for, and it does not exist yet —
today the move is a stopped-daemon copy of two directories.

## Root, on purpose

The mounted socket is root-owned and mode 0660 everywhere, so a non-root process needs membership of the
host's docker group — whose gid differs per host, making any baked-in `--group-add` wrong somewhere. And
access to that socket is already root-equivalent, so dropping privileges inside the container gives up
nothing an attacker could not immediately take back. Run this on a host dedicated to it.

## The build context

`.dockerignore` matters more than usual here. The repository carries `www/node_modules` (436 MB) and
`demo/template` (298 MB), and a Docker build context is copied to the daemon whole — the first version of
the ignore file used `node_modules/`, which matches at the root only, and shipped an 851 MB context to
build 3 MB of Go. Patterns for nested directories need `**/`.

It also keeps `.docpreview/` out, which is the vault, the database and every credential this instance
holds.
