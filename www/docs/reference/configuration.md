---
id: configuration
title: Server configuration
sidebar_position: 1
---

# Server configuration

NetFoundry Preview reads its config from `~/.docpreview/config.yml`, `$DOCPREVIEW_CONFIG`, or `-config <path>`.

A missing file is not an error. With no config at all you get a zrok-backed daemon that only needs GitHub
credentials. The exceptions are the settings that have no sensible default, such as Frontdoor's `env_z_id`.

## Complete example

```yaml
# Where the webhook ingress binds. Loopback plus a zrok share is the
# recommended setup: nothing is reachable except through the overlay.
listen: 127.0.0.1:8471

# Vault, database, workspaces and build artifacts all live under here.
data_dir: /home/you/.docpreview

# The daemon's own log, in addition to stderr. Appended to, never rotated.
log_file: ""

# Concurrent builds. Each one is an npm install, so disk and RAM run out
# long before CPU does.
workers: 2

# Where the dashboard is reachable from, for the log link in a build's
# comment. Unset means the comment names the dashboard without linking to it.
dashboard_url: ""

exposer:
  kind: zrok2                    # zrok2 | frontdoor | ziti | local
  prefix: ""                     # starts every published name
  build_shares: false            # a second share per build, pinned to its commit

  zrok2:
    namespace: ""                # blank = the environment's default
    name_template: "{{.Repo.Name}}-{{.Name}}"
    open: true
    access_grants: []
    oauth_provider: ""           # "google" or "github"
    oauth_email_domains: []

  # NetFoundry Frontdoor, from nfconsole.io. Not zrok's own "zrok frontdoor"
  # feature, which shares the name and issues none of these values.
  # Replace the three values below with your own from the Frontdoor console.
  frontdoor:
    # The instance id goes in the path. Frontdoor's routes are unversioned and
    # carry tenancy in this segment, so an api_base without it 404s every call.
    api_base: https://gateway.production.netfoundry.io/frontdoor/<frontdoorId>
    # A frontend ID, not a name. "public" is a name and matches no frontend,
    # which creates a share that serves nothing.
    frontend: bMTHPrtQ
    # The enrolled Frontdoor agent's ziti identity. Required on every share.
    # Without it every publish is refused, and startup says so.
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

bitbucket:
  enabled: false
  auth: access_token             # access_token | api_token

build:
  driver: docker                 # docker | local
  # allow_local_driver: true     # required before driver: local does anything
  image: node:24-bookworm        # docker driver only
  cpus: 4
  memory: 6g
  timeout: 15m
  keep_logs: 168h
  # cache_dir: D:\docpreview-cache  # default <data_dir>/cache
  # secrets:                        # env var -> vault key, for every build
  #   ALGOLIA_WRITE_KEY: algolia.write_key

preview:
  ttl: 72h
  teardown_on_close: true
  keep_builds: 10
  title: "NetFoundry Preview"
```

## Top level

| Field | Default | Notes |
|---|---|---|
| `listen` | `127.0.0.1:8471` | Bind loopback and reach it through a zrok share rather than binding `0.0.0.0`. |
| `listeners` | none | The general form of `listen`. See below. |
| `data_dir` | `~/.docpreview` | Created with `0700`. It holds the vault, so protect it like one. |
| `log_file` | unset | The daemon's own log, written in addition to stderr. Unset means stderr only. Appended to and never rotated, so rotate it with whatever supervises the process. |
| `workers` | `2` | Concurrent builds. Must be at least 1. |
| `dashboard_url` | unset | Where the dashboard is reachable from. See below. |

### `dashboard_url`

A build's pull request comment quotes no build output and no error text. Both carry host paths and whatever a build
script printed, and the comment is public on a public repository. Instead the comment says where to look, and this
is the address it links to. A failed build links to its log, and a build in progress links the word "Building".

```yaml
dashboard_url: https://docpreview-dash.shares.zrok.io
```

You have to set it because the daemon can't work it out. It listens on loopback, and a loopback address is exactly
what a link in a pull request must not use. Whatever makes the dashboard reachable, whether a tunnel, a reverse proxy
or a VPN, is outside this process. [`dashboard-only`](./cli.md#dashboard-only) is one way to get such an address.

When it is unset, the comment names the dashboard without linking to it.

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

**Set one or the other, never both.** A config with both `listen` and `listeners` is refused at load. There's no safe
way to guess which one you meant, and the one that lost would be an address you think is live: a webhook endpoint
nobody answers, or an admin port you meant to retire.

If one TCP address is all you need, use `listen`. It is still the default.

### A `ziti` listener

A `ziti` listener binds an OpenZiti service instead of a port, so the dashboard and the webhook endpoint have **no
address on the underlay at all**. It gives the admin surface the same posture the `ziti` exposer gives previews.

| Field | Notes |
|---|---|
| `identity_file` | An enrolled identity, named by a **Bind** policy on the service. `docpreview configure ziti` makes one. |
| `service` | The service to bind. |
| `admin_identities` | Identity **ids** allowed to edit projects and store credentials through this listener. Empty, the default, means the dashboard is read-only over it. |

The first two are required. A `ziti` listener missing either one fails at startup instead of on the first request.
`docpreview doctor` goes further and binds each one. That's the only way to catch an identity that won't
authenticate, or a service with no Bind policy naming it.

#### Who can change things over the overlay

The whole dashboard is served over a ziti listener. Two parts of it change things: projects, which decide what
command runs on the build host, and credentials, which decide what it runs with. Both are refused unless the dialing
identity is listed in `admin_identities`.

```yaml
listeners:
  - ziti:
      identity_file: "/home/you/.docpreview/ziti/docpreview-host.json"
      service: "docpreview-admin"
      admin_identities: ["abc123"]
```

Get the ids from the **ID** column of `ziti edge list identities`, not the name column. A name can be changed on the
controller without the grant following it, which would quietly widen or empty this list.

:::note Empty means read-only, on purpose

Being enrolled on the network doesn't mean you may write a credential. A listener that names nobody serves the
dashboard, `/status` and every build log, and refuses every write with a message saying why. It doesn't show buttons
that will only fail.

The refusal names the identity it turned away, so adding it is a copy and paste.

:::

An identity that dials with no id at all is refused too. That's what you get from a router that never sent the
header, and "we can't tell who this is" is not a grant.

:::warning One process per service

Binding a ziti service creates a *terminator*. Two NetFoundry Preview instances binding the same service create two,
and under the default strategy the router load-balances between them. About half the dashboard requests then reach
an instance that knows nothing about them. Give a second instance its own service.

:::

This matters more for the admin surface than for previews. `/status` and the dashboard list every open pull request
with a preview, while each preview is only one branch.

## `exposer`

`kind` picks the implementation: `zrok2`, `frontdoor`, `ziti` or `local`. See [Exposers](../exposers.md) for what
each one does and when to pick it.

| Field | Default | Notes |
|---|---|---|
| `kind` | `zrok2` | Which exposer publishes previews. |
| `prefix` | empty | Starts every published name, so two installations can share one exposer account. `a` and `a-` mean the same thing. This is only the starting value: the dashboard can change it, and the value it saves wins. `webhook-only` and `dashboard-only` take their name from `-zrok-name` instead. |
| `build_shares` | `false` | Give every successful build a second share pinned to its commit. See [Per-build URLs](#per-build-urls). |

### `name_template`

A Go `text/template` evaluated against the pull request, with one extra field. The default is
`{{.Repo.Name}}-{{.Name}}`.

| Expression | Example |
|---|---|
| `{{.Name}}` | `feature-new-guide`, the sanitized branch |
| `{{.Branch}}` | `feature/new-guide`, the raw branch. Not a valid hostname on its own. |
| `{{.Repo.Name}}` | `docs` |
| `{{.Repo.Owner}}` | `acme` |
| `{{.Number}}` | `42` |
| `{{.HeadSHA}}` | `4f0c2a1…`, the full commit |

The result is sanitized again after templating. A repository name can hold characters GitHub allows and DNS doesn't,
and this keeps them out of the hostname.

:::danger The name must be unique per preview

Every exposer keys a live preview on this name, so two previews that render to the same name collide. zrok and
Frontdoor refuse the second one and fail the build with a message naming the other preview. The `local` exposer has
no namespace to conflict in, so it replaces the listener. That looks like a `ready` preview whose link answers
connection-refused.

That's why the default includes the repository. `{{.Name}}` alone reads better, but every repository with a `main`
branch renders to `main`.

| Situation | Template |
|---|---|
| One organization | `{{.Repo.Name}}-{{.Name}}` (default) |
| Several organizations, one namespace | `{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}` |
| Short over readable | `pr-{{.Number}}-{{.Repo.Name}}` |
| Immutable per commit | `{{.Repo.Name}}-{{.Name}}-{{.HeadSHA}}` |

:::

:::note Why the commit hash isn't in the default

Vercel gives every deployment an immutable URL with a branch alias on top, and that works well for Vercel.
NetFoundry Preview edits one comment per pull request in place, so the link a reviewer already opened has to keep
working after the next push. Put `{{.HeadSHA}}` in the template if you want a URL per commit, knowing that every push
strands the previous one.

:::

### `ziti`

| Field | Notes |
|---|---|
| `identity_file` | NetFoundry Preview's own enrolled identity, named by a Bind policy on the service. |
| `service` | One wildcard service carries every preview. Requests are told apart by `Host`. |
| `domain` | Must match the addresses in the service's `intercept.v1` config, or the tunneler resolves names nothing answers to. |
| `name_template` | As above. |

`docpreview configure ziti` creates all of it and writes these four fields for you.

### `frontdoor`

| Field | Default | Notes |
|---|---|---|
| `api_base` | `https://gateway.production.netfoundry.io/frontdoor` | Add your Frontdoor instance id to the path, as in the example. Without it the daemon does not start. |
| `frontend` | none | A frontend **ID**, such as `bMTHPrtQ`. A name matches no frontend. Without it the daemon does not start. |
| `env_z_id` | none | The enrolled Frontdoor agent's ziti identity. Required. |
| `agent_reachable_host` | `127.0.0.1` | Where the Frontdoor agent connects to reach a preview. Loopback is right when the agent runs on this host. |
| `name_template` | as above | |

## `github`

| Field | Notes |
|---|---|
| `app_id` | From the App settings page. |
| `api_base` | `https://api.github.com`, or `https://your-ghe-host/api/v3` for Enterprise. The git host is derived from it. |

The private key and webhook secret are **not here**. They live in the vault. See [Security](./security.md).

## `bitbucket`

| Field | Default | Notes |
|---|---|---|
| `enabled` | `false` | Turns Bitbucket Cloud on. Storing a credential doesn't turn it on by itself. |
| `api_base` | `https://api.bitbucket.org` | |
| `auth` | `access_token` | `access_token` is a repository, project or workspace access token. `api_token` is an Atlassian account email plus API token, which can see everything that person can and posts every comment as them. |

The tokens and webhook secret live in the vault. See [Connect Bitbucket Cloud](../guides/bitbucket.md).

## `local`

The [local platform](../local-platform.md): bare git repositories on this machine instead of a hosted platform.

| Field | Default | Notes |
|---|---|---|
| `enabled` | `false` | An endpoint that clones and builds whatever it is told to shouldn't be on by accident. |
| `repos_dir` | `~/.docpreview/repos` | The bare repositories that act as remotes. |
| `default_base` | `main` | The branch changed files are compared against when an event doesn't name one. |
| `webhook_secret` | empty | When set, the local webhook requires the same HMAC-SHA256 signature GitHub sends. Empty means unauthenticated. |

## `build`

| Field | Default | Notes |
|---|---|---|
| `driver` | `docker` | `docker` runs the build in a throwaway container. `local` runs it on this host, and only if `allow_local_driver` is set. |
| `allow_local_driver` | `false` | Required for `driver: local`, whether the server config or a project asks for it. Without it, those builds fail. |
| `image` | `node:24-bookworm` | Docker driver only. Use an image with git in it: the `-slim` images have none, and a build that clones another repository fails with `git: not found`. |
| `cpus` | `4` | Passed to docker as `--cpus`. `0` means no flag. |
| `memory` | `6g` | Passed to docker as `--memory`. Empty means no flag. |
| `timeout` | `15m` | Per build. Must be positive. |
| `keep_logs` | `168h` (7 days) | How long a build log stays on disk. See [build logs](./build-logs.md#retention). |
| `secrets` | none | Environment variable → vault key, injected into **every** build. See [build logs](./build-logs.md#giving-a-build-a-secret), and [Projects](./projects.md#environment-variables-scoped-to-a-project) for the per-repository form. |
| `cache_dir` | `<data_dir>/cache` | Package downloads, one directory per pull request, under the docker driver. |

### Choosing a driver

**`docker`** runs the build in a container with a memory and CPU cap and none of the host's environment. Use it when
more people can open a pull request than you would give a shell to.

**`local`** runs `package.json` scripts written by whoever opened the pull request, as the daemon's user, on the host
that holds the GitHub App key and every project's tokens. It's fast, because the npm cache stays warm between builds.
It's fine for repositories whose contributors already have access to the host. You have to turn it on with
`allow_local_driver: true`. A missing config file, a missing docker or a project row can't turn it on for you.

Fork pull requests are refused at the webhook under either driver, so only people with push access to a branch can
run a build. Whether that's a meaningful limit depends on your repository.

On Windows, Docker Desktop runs containers in a WSL2 VM, and a container can't use more than that VM was given.
`docker info` shows what is really available. Raise it in `.wslconfig`, not here.

### What the docker driver mounts

The workspace is **bind-mounted** at `/workspace`, not copied in. The container is created with `docker create`, run,
and removed with `docker rm -fv`, so nothing survives except what is on a mount. The install and build commands run as
one `sh -lc` line inside the build directory.

:::warning The workspace must be on a lettered drive

On Windows the host path is translated for the docker daemon's view of the disk: `D:\...\workspace` becomes
`/mnt/d/.../workspace`. Nothing else is translated. A UNC path, a mapped share or anything without a drive letter fails
the build before the container is created:

```text
cannot express \\nas\builds\ws as a path the docker daemon can mount;
the docker driver needs the workspace on a lettered drive
```

Docker Desktop has used more than one spelling for the host mount root. A wrong guess would give an empty
`/workspace` and a build that fails on a missing `package.json`, which looks like the repository's fault. So it's an
error instead.

Move `data_dir` onto a lettered drive, or use the `local` driver.

:::

`node_modules` gets **its own anonymous docker volume**, mounted over `<build dir>/node_modules` on top of the bind
mount. That's where build time goes. Installing this project's own `www/` took **5m46s** writing `node_modules`
through the bind mount and **14s** writing it to a volume, with the same warm package cache. Writing tens of thousands
of small files through a cross-filesystem mount is about twenty times slower than writing them to a volume.

The volume is anonymous, so `docker rm -fv` removes it with the container. `node_modules` is never cached between
builds and never reaches the host disk.

Before publishing, the **build output is walked and refused if it contains a symlink**:

```text
the build output contains a symlink (assets/leak), which cannot be published
```

The preview file server blocks path traversal but follows symlinks out of its root. Without this check,
`build/leak -> /etc/passwd` would be served to anyone with the preview URL. The check runs on the output directory
only, after the build, under either driver.

### The build cache

A workspace is created per commit and deleted later, so nothing a build downloads survives in it. `cache_dir` is where
downloads go instead, so a second build of the same pull request doesn't fetch the whole dependency tree again.

Don't expect much from it. Two builds of this project's own `www/`, one with an empty cache and one with a full one,
took **4m28s cold and 4m21s warm**. Both reported `added 1325 packages in 2m`. The cache saved seven seconds.

The real speed-up came from moving `node_modules` off the bind mount, described above. The same 1325 packages now
install in 14 seconds while downloading every one. The cache helps on a slow link, a very large tree, or a bad day
for the registry, and it costs almost nothing the rest of the time.

There's one cache per pull request, at `<cache_dir>/<preview-id>/`, holding `npm/`, `yarn/` and `pnpm/`. They're
bind-mounted into the container at `/cache/npm`, `/cache/yarn` and `/cache/pnpm`. Each package manager is pointed at
its own with an environment variable (`npm_config_cache`, `YARN_CACHE_FOLDER`, `npm_config_store_dir`). These are set
before the repository's own `build.env`, so a repository that sets one of them wins.

pnpm hard-links out of its store, and npm may delete files in its cache directory. That's why each gets its own.

**The cache is deleted with the preview**, in the same teardown that removes the workspace, the artifacts, the build
logs and the pull request comment. That's why the key is per preview. A cache shared by every branch never has a
moment when it's safe to remove, so it grows until somebody notices the disk.

The key is the preview ID, which comes from the pull request rather than the branch name. A force-push or a branch
rename keeps the cache the pull request already filled.

This is the biggest and busiest directory NetFoundry Preview writes to, so put it on a disk with room.

#### Clearing it by hand

Do this when a build fails on a package that hasn't changed.

**Every preview's cache**, from the dashboard: **Clear caches** in the top bar, next to the Projects and Secrets
links. It follows the same rule as every other control that changes something. See [Security](./security.md).

**One preview's cache**, over the API:

```powershell
curl.exe -X DELETE http://127.0.0.1:8471/api/cache/9f2a1c4b7e01
```

```bash
curl -X DELETE http://127.0.0.1:8471/api/cache/9f2a1c4b7e01
```

The preview ID is the twelve hex characters shown on its card and in `/status`. Anything else is refused. Both ways
empty `npm/`, `yarn/` and `pnpm/` and recreate them, and neither touches `cache_dir` itself.

If a running build holds a file open in the cache, the clear fails and says so:

```text
could not clear D:\docpreview-cache\9f2a1c4b7e01\npm: ... — a build may be running and
holding a file open in it; try again once the queue is idle
```

## `preview`

| Field | Default | Notes |
|---|---|---|
| `ttl` | `72h` | Idle lifetime. Every rebuild resets it, so an active pull request never expires. |
| `teardown_on_close` | `true` | Remove the preview and its comment when the pull request closes. |
| `keep_builds` | `10` | How many builds of one pull request keep their artifacts, and so stay openable. |
| `title` | `NetFoundry Preview` | The heading of the pull request comment and the name of the Bitbucket build status. Comments are found by a hidden marker, so changing it edits existing comments in place. |

### Builds and `keep_builds`

Artifacts are stored per build, so the dashboard can still open an older commit. That means disk use grows with every
push, and `keep_builds` bounds it. When the limit is reached, the oldest build's artifacts are deleted. The build that
just published is never pruned. Its log is kept separately, under `build.keep_logs`, so you can still read why a build
failed after its site is gone.

An activity entry whose build was cleaned up stops being clickable and says so.

The reaper runs hourly. Preview TTLs are measured in days, so running it more often wastes wakeups, and running it
less often leaves a dead link in a comment for most of a working day.

### One share per preview

Under zrok, each preview has one share for its branch. It is created once, and every rebuild reuses it and swaps in
the new build, so a push costs no zrok calls. The pull request comment links to that share, so the link always shows
the latest build.

Shutting down doesn't delete shares. On the next start, every preview and both tunnels (`webhook-only` and
`dashboard-only`) adopt the share already bound to their name, so a restart creates no shares. A share that nothing
wants any more is deleted at start.

#### The share-creation gate

zrok answers `429` when an account creates shares too fast. So every share creation goes through a gate: **15 per 10
minutes per zrok environment**, shared by the daemon, `webhook-only` and `dashboard-only`. A creation over the limit
waits for the window. Two `429`s in a row mean 10 minutes with no creations at all.

The dashboard footer shows `zrok shares N/15`, and `/status` has the same numbers under `zrok_shares`.

#### Names have a quota

Each URL needs a **reserved name**, which zrok counts against your account separately from shares. NetFoundry
Preview reserves the name before it creates the share, and **releases it when the preview is torn down**. It
de-reserves the name first and then deletes it, so a crash halfway through is cleaned up at the next start.

A name is not released on a rebuild, a supersede or a shutdown. That's what keeps the link in the pull request
comment working across every push, and why the comment can be written once and edited after that.

```powershell
zrok2 list names
```

Hitting the limit answers `409` with `names limit reached; cannot reserve additional names`.

### Per-build URLs

With `exposer.build_shares: true`, every successful build also gets a share of its own, pinned to its commit. The
dashboard's build picker gets an **Open build ↗** button for it. The pull request comment still links to the branch
URL. It's off by default because each one is another share creation and another reserved name.

The build's name is the branch's name with the short commit added, so `docs-new-guide` becomes
`docs-new-guide-a1b2c3d`:

```text
https://docs-new-guide.shares.zrok.io/          the branch, always the latest build
https://docs-new-guide-a1b2c3d.shares.zrok.io/  one commit, forever
```

It's derived from the rendered branch name rather than templated again. A `name_template` that separates
repositories keeps doing so here, and the two names sort next to each other in a list of shares.

A build's own URL is best effort. If the exposer refuses it, most likely because of a quota on reserved names, the
build still succeeds, the branch URL is unaffected, and the daemon logs a warning:

```text
level=WARN msg="this build has no URL of its own; the branch URL is unaffected"
  pr=acme/docs#42 build=20260729-141207-a1b2c3d name=docs-new-guide-a1b2c3d
  error="quota exceeded: too many reserved names"
```

The comment never mentions it.

:::warning A pruned build's share outlives its artifacts until the next restart

This applies only with `build_shares: true`. `keep_builds` deletes artifact directories. It doesn't withdraw that
build's share or release its name. Those are cleaned up at the next start. Until then, the name counts against your
zrok quota and the URL still resolves, onto a directory that is gone. The dashboard marks the build as no longer
openable, but a link someone saved still points at it. Restart the daemon if that matters to you.

When you turn `build_shares` off, the per-build shares already published are deleted at the next start.

:::

## `vault`

Where the master key that decrypts `<data_dir>/vault.age` comes from.

| Key | Default | Meaning |
|---|---|---|
| `key_source` | unset | Where to read the master key at startup. Unset means nowhere: the daemon starts with a locked vault and you unlock it from the dashboard. |

```yaml
vault:
  # A command, which is the recommended form: the key exists in the daemon
  # process and nowhere else on the machine.
  key_source: "exec:op read op://ops/docpreview/master-key"

  # Or a file. It must not be inside data_dir, which would put the key next to
  # the ciphertext it decrypts, and on Unix it must not be group- or
  # world-readable. Make one with:
  #   docpreview vault keygen -out /etc/docpreview/master.key
  key_source: "file:/etc/docpreview/master.key"
```

Unset is the only setting where the key isn't stored anywhere. The cost is that after a reboot the daemon does
nothing until somebody unlocks it. See [security](./security.md#the-master-key) for the trade-off.

## Environment variables

| Variable | Purpose |
|---|---|
| `DOCPREVIEW_MASTER_KEY` | The vault key, as an age identity or a passphrase. Only read when `vault.key_source` is unset, and **not recommended**: every process running as the same user can read an environment variable, and it ends up in service definitions, process listings and crash dumps. |
| `DOCPREVIEW_CONFIG` | Config file path. |

## Related

- [Repository configuration](./repo-config.md): the `.docpreview.yml` that ships in the repository
- [Projects](./projects.md): per-repository settings and environment variables, which override both of these
- [CLI reference](./cli.md)
- [Security model](./security.md)
