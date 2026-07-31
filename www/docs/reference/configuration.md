---
id: configuration
title: Server configuration
sidebar_position: 1
---

# Server configuration

Read from `~/.docpreview/config.yml`, or `$DOCPREVIEW_CONFIG`, or `-config <path>`.

Every field has a default, and a missing file is not an error — running with no config at all gives you a
working zrok-backed daemon that only needs GitHub credentials.

## Complete example

```yaml
# Where the webhook ingress binds. Loopback plus a zrok share is the
# recommended arrangement: nothing is exposed except through the overlay.
listen: 127.0.0.1:8471

# Vault, database, workspaces, and build artifacts all live under here.
data_dir: /home/you/.docpreview

# Concurrent builds. Each one is an npm install, so this is bounded by disk
# and RAM long before CPU.
workers: 2

# Where the dashboard is reachable from, for the link in a failed build's
# comment. Unset means the comment names the dashboard without linking to it.
dashboard_url: ""

exposer:
  kind: zrok2                    # zrok2 | frontdoor | local

  zrok2:
    namespace: ""                # blank = the environment's default
    name_template: "{{.Repo.Name}}-{{.Name}}"
    open: true
    access_grants: []
    oauth_provider: ""           # "google" or "github"
    oauth_email_domains: []

  frontdoor:
    # The instance id belongs in the path. Frontdoor's routes are unversioned and
    # carry tenancy in this segment, so an api_base without it 404s every call.
    api_base: https://gateway.production.netfoundry.io/frontdoor/<frontdoorId>
    # A frontend ID, not a name. "public" is a name and matches no frontend, which
    # creates a share that serves nothing.
    frontend: bMTHPrtQ
    # The enrolled Frontdoor agent's ziti identity. Required on every share; without
    # it every publish is refused, and startup says so.
    env_z_id: ijcrWb-ZOq
    agent_reachable_host: 127.0.0.1
    name_template: "{{.Repo.Name}}-{{.Name}}"

  ziti:
    identity_file: /home/you/.docpreview/ziti/docpreview-host.json
    service: docpreview-svc
    domain: docpreview.ziti
    name_template: "{{.Repo.Name}}-{{.Name}}"

github:
  app_id: 1234567
  api_base: https://api.github.com    # change for GitHub Enterprise

build:
  driver: local                  # local | docker
  image: node:24-bookworm-slim   # docker driver only
  timeout: 15m
  keep_logs: 168h
  # cache_dir: D:\docpreview-cache  # default <data_dir>/cache
  # secrets:                        # env var -> vault key, for every build
  #   ALGOLIA_WRITE_KEY: algolia.write_key

preview:
  ttl: 72h
  teardown_on_close: true
  keep_builds: 10
```

## Top level

| Field | Default | Notes |
|---|---|---|
| `listen` | `127.0.0.1:8471` | Bind loopback and reach it through a zrok share rather than binding `0.0.0.0`. |
| `listeners` | — | The general form of `listen`. See below. |
| `data_dir` | `~/.docpreview` | Created with `0700`. Holds the vault, so treat it accordingly. |
| `workers` | `2` | Concurrent builds. Must be at least 1. |
| `dashboard_url` | unset | Where the dashboard is reachable from, for the link in a failed build's comment. See below. |

### `dashboard_url`

A failed build's pull request comment quotes no build output and no error text — both carry host paths and whatever
a build script chose to print, and the comment is public on any public repository. It says where to look instead,
and this is the address it points at.

```yaml
dashboard_url: https://docpreview-dash.shares.zrok.io
```

Configured rather than derived, because the daemon cannot work it out. Its listener is loopback, so the one address
it knows is the one address a link in a pull request must not use, and whatever makes the dashboard reachable — a
tunnel, a reverse proxy, a VPN — is outside this process. [`dashboard-only`](./cli.md#dashboard-only) is how to
produce such an address.

Unset is the honest default: the comment then names the dashboard without pretending to link to it.

## `listeners`

`listen` binds one TCP address. `listeners` binds any number, TCP or OpenZiti, all served by one HTTP server:

```yaml
listeners:
  - tcp: "127.0.0.1:8471"
  - ziti:
      identity_file: "/home/you/.docpreview/ziti/docpreview-host.json"
      service: "docpreview-admin"
```

A bare string is shorthand for `tcp`, so `- "127.0.0.1:8471"` works too.

**Set one or the other, never both.** A config with `listen` *and* `listeners` is refused at load. There is no
safe way to guess which one was meant: whichever lost would be an address the operator believes is live — a
webhook endpoint nobody answers, or an admin port they meant to stop using.

`listen` remains the recommended spelling when one TCP address is all you need, and stays the default.

### A `ziti` listener

Binds an OpenZiti service instead of a port, so the dashboard and the webhook endpoint have **no address on
the underlay at all** — the same posture the `ziti` exposer gives previews, applied to the admin surface.

| Field | Notes |
|---|---|
| `identity_file` | An enrolled identity, named by a **Bind** policy on the service. `docpreview configure ziti` produces one. |
| `service` | The service to bind. |
| `admin_identities` | Identity **ids** allowed to edit projects and store credentials through this listener. Empty — the default — means the dashboard is read-only over it. |

The first two are required; a `ziti` listener missing either is a config error at startup, not a surprise on the
first request. `docpreview doctor` goes further and actually binds each one, which is the only way to catch an
identity that will not authenticate or a service with no Bind policy naming it.

#### Who may change things over the overlay

The dashboard serves over a ziti listener in full. The two surfaces that *change* something — projects, which
decide what command runs on the build host, and credentials, which decide what it runs with — are refused unless
the dialing identity is named in `admin_identities`.

```yaml
listeners:
  - ziti:
      identity_file: "/home/you/.docpreview/ziti/docpreview-host.json"
      service: "docpreview-admin"
      admin_identities: ["abc123"]
```

Get the ids from `ziti edge list identities` — the **ID** column, not the name. A name can be changed on the
controller without the grant following it, which would silently widen or void this list.

:::note Empty is read-only, and that is deliberate

Being enrolled on the network is not authorization to write a credential. So a listener that names nobody serves
the dashboard, `/status` and every build log, and refuses every write with a message saying why — rather than
offering buttons that will fail.

The refusal names the identity that was turned away, so adding it is a copy and paste.

:::

An identity that dials with no id at all is refused too. That is what a router which never sent the header
produces, and "we cannot tell who this is" is not a grant.

:::warning One process per service

Binding a ziti service creates a *terminator*. Two docpreviews binding the same service create two, and the
router load-balances between them under the default strategy — so roughly half the dashboard requests reach
an instance that knows nothing about them. Give a second instance its own service.

:::

This matters more for the admin surface than for previews. `/status` and the dashboard enumerate every open
documentation pull request; the previews themselves are one branch each.

## `exposer`

`kind` selects the implementation. See [Exposers](../exposers.md) for what each one does and when to pick it.

### `name_template`

A Go `text/template` evaluated against the pull request, with one extra field. Default
`{{.Repo.Name}}-{{.Name}}`.

| Expression | Example |
|---|---|
| `{{.Name}}` | `feature-new-guide` — the sanitized branch |
| `{{.Branch}}` | `feature/new-guide` — the raw branch. Not a valid hostname on its own. |
| `{{.Repo.Name}}` | `docs` |
| `{{.Repo.Owner}}` | `acme` |
| `{{.Number}}` | `42` |
| `{{.HeadSHA}}` | `4f0c2a1…` — the full commit |

The result is sanitized again after templating, so a repository name containing characters that are legal in
GitHub but not in DNS cannot produce an invalid hostname.

:::danger The name must be unique per preview

Every exposer keys a live publication on this name. Two previews that render to the same one collide, and the
collision is not always loud: zrok and Frontdoor refuse it and fail the build with a message naming the other
preview, but the `local` exposer has no namespace to conflict in and simply replaces the listener — which
looks like a `ready` preview whose link answers connection-refused.

The default includes the repository for exactly this reason. `{{.Name}}` alone reads better and was the
original default, but every repository with a `main` branch renders to `main`.

| Situation | Template |
|---|---|
| One organization | `{{.Repo.Name}}-{{.Name}}` (default) |
| Several organizations, one namespace | `{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}` |
| Short over readable | `pr-{{.Number}}-{{.Repo.Name}}` |
| Immutable per commit | `{{.Repo.Name}}-{{.Name}}-{{.HeadSHA}}` |

:::

:::note Why not the commit hash by default

Vercel gives every deployment an immutable URL and puts a branch alias on top. docpreview has one comment per
pull request, edited in place, so the link a reviewer already opened has to keep working after the next push.
Including `{{.HeadSHA}}` gives you per-commit URLs and strands the previous one on every push — which is a
reasonable thing to want, and is not the default.

:::

### `ziti`

| Field | Notes |
|---|---|
| `identity_file` | docpreview's own enrolled identity, named by a Bind policy on the service. |
| `service` | One wildcard service carries every preview; requests are separated by `Host`. |
| `domain` | Must match the addresses in the service's `intercept.v1` config, or the tunneler resolves names docpreview does not answer to. |
| `name_template` | As above. |

`docpreview configure ziti` creates all of it and writes these four fields for you.

## `github`

| Field | Notes |
|---|---|
| `app_id` | From the App settings page. |
| `api_base` | `https://api.github.com`, or `https://your-ghe-host/api/v3` for Enterprise. The git host is derived from it. |

The private key and webhook secret are **not here**. They live in the vault — see
[Security](./security.md).

## `build`

| Field | Default | Notes |
|---|---|---|
| `driver` | `local` | `local` runs npm on this host. `docker` runs it in a throwaway container. |
| `image` | `node:24-bookworm-slim` | Docker driver only. |
| `timeout` | `15m` | Per build. Must be positive. |
| `keep_logs` | `168h` (7 days) | How long a build log survives on disk. See [build logs](./build-logs.md#retention). |
| `secrets` | — | Environment variable → vault key, injected into **every** build. See [build logs](./build-logs.md#giving-a-build-a-secret), and [Projects](./projects.md#environment-variables-scoped-to-a-project) for the per-repository form. |
| `cache_dir` | `<data_dir>/cache` | Package downloads, one directory per pull request, under the docker driver. |

### Choosing a driver

**`local`** runs `package.json` scripts written by whoever opened the pull request, with this process's
privileges. That is fine for a repository whose contributors you already trust, and it is fast because the npm
cache is warm between builds.

**`docker`** runs the same thing in a container with a memory and CPU cap and no host environment. The correct
choice if the set of people who can open a pull request is larger than the set of people you would give a shell to.

### What the docker driver mounts

The workspace is **bind-mounted** at `/workspace`, not copied in. The container is created with `docker create`,
run, and removed with `docker rm -fv` afterwards, so nothing survives it except what is on a mount. Caps are 4 GB
of memory and 2 CPUs, and the install and build commands run as one `sh -lc` line inside the build directory.

:::warning The workspace must be on a lettered drive

On Windows the host path is translated for the docker daemon's own view of the disk: `D:\...\workspace` becomes
`/mnt/d/.../workspace`. Nothing else translates. A UNC path, a mapped share, or anything without a drive letter
fails the build before the container is created:

```text
cannot express \\nas\builds\ws as a path the docker daemon can mount;
the docker driver needs the workspace on a lettered drive
```

An error rather than a guess. Docker Desktop has had more than one spelling for the host mount root, and a wrong
guess produces an empty `/workspace` and a build that fails on a missing `package.json` — which looks like the
repository's fault.

Move `data_dir` onto a lettered drive, or use the `local` driver.

:::

`node_modules` gets **its own anonymous docker volume**, mounted over `<build dir>/node_modules` on top of the bind
mount. That is where build time actually goes, and it is not a small effect: installing this project's own `www/`
took **5m46s** writing `node_modules` through the bind mount and **14s** writing it to a volume, with the same warm
package cache. Tens of thousands of small files through a cross-filesystem mount is roughly twenty times slower to
write than the same files on a volume.

The volume is anonymous, so `docker rm -fv` reaps it with the container. `node_modules` is never cached between
builds and never reaches the host disk at all.

Finally, before publishing, the **build output is walked and refused if it contains a symlink**:

```text
the build output contains a symlink (assets/leak), which cannot be published
```

The preview file server blocks path traversal but follows symlinks out of its root, so `build/leak -> /etc/passwd`
would otherwise be published to whoever holds the preview URL. The check runs on the output directory only, after
the build, under the docker driver.

### The build cache

A workspace is created per commit and deleted with its siblings, so nothing a build downloads survives inside it.
`cache_dir` is where the downloads go instead, so a second build of the same pull request does not refetch the
dependency tree.

Set your expectations from the measurement rather than from the idea, because the measurement is unflattering. Two
builds of this project's own `www/`, one with an empty cache and one with a full one: **4m28s cold, 4m21s warm.**
Both reported `added 1325 packages in 2m`. The cache bought seven seconds out of four and a half minutes.

**Do not credit the cache with the speed-up.** That came from moving `node_modules` off the bind mount, described
above, and the same 1325 packages now install in 14 seconds *downloading every one*. The cache earns its keep on a
slow link, a very large tree, or a registry having a bad day, and it is close to free the rest of the time.

One cache per pull request, at `<cache_dir>/<preview-id>/`, holding `npm/`, `yarn/` and `pnpm/`, bind-mounted into
the container at `/cache/npm`, `/cache/yarn` and `/cache/pnpm`. Each package manager is pointed at its own by an
environment variable — `npm_config_cache`, `YARN_CACHE_FOLDER`, `npm_config_store_dir` — set before the repository's
own `build.env`, so a repository that names one of those variables wins.

Three directories rather than one because pnpm hard-links out of its store, and a store sharing a directory with
npm's cache is a store npm may delete under it.

**The cache is deleted when the preview is** — the same teardown that removes the workspace, the artifacts, the
build logs and the pull request comment. That is the reason for the per-preview key: a cache shared by every branch
has no moment at which anything knows it is safe to remove, and grows until somebody notices the disk.

The branch name is not part of the key — it is the preview ID, which is derived from the pull request — so a
force-push or a rename keeps the cache the pull request already filled.

This is the largest and most frequently rewritten directory docpreview owns, so it is worth pointing at a disk with
room.

#### Clearing it by hand

Reach for this when a build fails on a package that has not changed.

**Every preview's**, from the dashboard: **Clear caches** in the top bar, beside the Projects and Secrets links. It
appears only for a request that originated on the machine running docpreview, like every other control that changes
something.

**One preview's**, over the API:

```powershell
curl.exe -X DELETE http://127.0.0.1:8471/api/cache/9f2a1c4b7e01
```

```bash
curl -X DELETE http://127.0.0.1:8471/api/cache/9f2a1c4b7e01
```

The preview ID is the twelve hex characters shown on its card and in `/status`; anything else is refused. Either
form empties `npm/`, `yarn/` and `pnpm/` and recreates them, and never touches `cache_dir` itself. With no
`cache_dir` configured — which cannot happen through the loader, only by constructing a config another way — it
answers `409` and says there is no cache to clear.

If a build is running and holding a file open in the cache, the clear fails and says so:

```text
could not clear D:\docpreview-cache\9f2a1c4b7e01\npm: ... — a build may be running and
holding a file open in it; try again once the queue is idle
```

Fork pull requests are refused at the webhook under either driver, so the exposure is limited to people with
push access to a branch. Whether that is a meaningful limit depends on your repository.

## `preview`

| Field | Default | Notes |
|---|---|---|
| `ttl` | `72h` | Idle lifetime. Refreshed on every rebuild, so an active pull request never expires. |
| `teardown_on_close` | `true` | Remove the preview and its comment when the pull request closes. |
| `keep_builds` | `10` | How many builds of one pull request keep their artifacts, and so stay openable. |

### Every build gets its own URL

A pull request has one **branch** URL that always serves whatever built last, and one URL per **build** pinned to
the commit it was built from. Five builds of a branch means six URLs. The pull request comment links to the branch
URL, because that is the one that stays current; the dashboard's build picker has an **Open build ↗** button beside
it for the rest.

The build's name is the branch's name with the short commit appended — `docs-new-guide` becomes
`docs-new-guide-a1b2c3d`:

```text
https://docs-new-guide.shares.zrok.io/          the branch, always the latest build
https://docs-new-guide-a1b2c3d.shares.zrok.io/  one commit, forever
```

Derived from the rendered branch name rather than templated again, so a `name_template` that separates repositories
keeps doing so here, and the two names sort next to each other in any list of shares.

This is why `keep_builds` exists. Artifacts are stored per build so an older commit still has something to serve,
which means disk use grows with every push instead of staying at one built site per pull request. When the limit is
reached the oldest build's artifacts are deleted; the build that just published is never pruned. Its log survives
independently, under `build.keep_logs`, so a build whose site is gone can still be read for why it failed.

An activity entry whose build has been cleaned up stops being clickable and says so, rather than offering a link to
an empty page.

A build's own URL is best effort. If the exposer refuses it — a quota on reserved names is the likely reason — the
build still succeeds, the branch URL is unaffected, and the daemon logs a warning:

```text
level=WARN msg="this build has no URL of its own; the branch URL is unaffected"
  pr=acme/docs#42 build=20260729-141207-a1b2c3d name=docs-new-guide-a1b2c3d
  error="quota exceeded: too many reserved names"
```

The comment never mentions it, so a reviewer sees no difference.

:::note Under zrok, a name is an object with a quota

Each URL needs a **reserved name**, which zrok counts against your account separately from shares. docpreview
registers one before creating the share, and **releases it when the preview is torn down** — de-reserving it first,
then deleting it, so a crash halfway through self-heals at the next startup reap.

A name is *not* released on a rebuild, a supersede, or a shutdown. That is what keeps the URL in the pull request
comment working across every push, and it is the whole reason the comment can be written once and edited thereafter.

One name per pull request accumulated slowly. One name per build does not, so if you raise `keep_builds` against
many repositories, watch the account:

```powershell
zrok2 list names
```

Hitting the limit answers `409` with `names limit reached; cannot reserve additional names`, and it is the build
share that fails first, not the branch share.

:::

The reaper runs hourly. Preview TTLs are measured in days, so finer is wasted wakeups and coarser leaves a
dead link in a comment for most of a working day.

:::warning A pruned build's share outlives its artifacts until the next restart

`keep_builds` deletes artifact directories. It does not withdraw that build's share or release its name — those are
tidied at the next startup, when the recorded URL is dropped and the share is reaped. In between the name still
counts against your zrok quota and the URL still resolves, onto a directory that is gone. The dashboard marks such a
build as no longer openable; a link somebody saved does not.

Restart the daemon if that matters to you. It is on the backlog, not in the code.

:::

## `vault`

Where the master key that decrypts `<data_dir>/vault.age` comes from.

| Key | Default | Meaning |
|---|---|---|
| `key_source` | unset | Where to read the master key at startup. Unset means nowhere: the daemon boots with a locked vault and is unlocked from the dashboard. |

```yaml
vault:
  # A command, which is the recommended form: the key exists in the daemon
  # process and nowhere else on the machine.
  key_source: "exec:op read op://ops/docpreview/master-key"

  # Or a file. It must not be inside data_dir — that would put the key beside
  # the ciphertext it decrypts — and on Unix it must not be group- or
  # world-readable. Mint one with:
  #   docpreview vault keygen -out /etc/docpreview/master.key
  key_source: "file:/etc/docpreview/master.key"
```

Leaving it unset is the only configuration where the key is not at rest anywhere. The price is that the daemon
comes back from a reboot inert, waiting for somebody to unlock it. See
[security](./security.md#the-master-key) for the trade.

## Environment variables

| Variable | Purpose |
|---|---|
| `DOCPREVIEW_MASTER_KEY` | The vault key. An age identity, or a passphrase. Consulted only when `vault.key_source` is unset, and **not recommended** — an environment variable is readable by every process under the same user, and lands in service definitions, process listings and crash dumps. |
| `DOCPREVIEW_CONFIG` | Config file path. |

## Related

- [Repository configuration](./repo-config.md) — the `.docpreview.yml` that ships in the repository
- [Projects](./projects.md) — per-repository settings and environment variables, which override both of these
- [CLI reference](./cli.md)
- [Security model](./security.md)
