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
    api_base: https://gateway.production.netfoundry.io/frontdoor
    frontend: public
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
  # cache_dir: D:\docpreview-cache  # default <data_dir>/cache

preview:
  ttl: 72h
  teardown_on_close: true
```

## Top level

| Field | Default | Notes |
|---|---|---|
| `listen` | `127.0.0.1:8471` | Bind loopback and reach it through a zrok share rather than binding `0.0.0.0`. |
| `listeners` | — | The general form of `listen`. See below. |
| `data_dir` | `~/.docpreview` | Created with `0700`. Holds the vault, so treat it accordingly. |
| `workers` | `2` | Concurrent builds. |

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

Both are required; a `ziti` listener missing either is a config error at startup, not a surprise on the first
request. `docpreview doctor` goes further and actually binds each one, which is the only way to catch an
identity that will not authenticate or a service with no Bind policy naming it.

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
| `timeout` | `15m` | Per build. |
| `cache_dir` | `<data_dir>/cache` | Package downloads, one directory per pull request, under the docker driver. |

### Choosing a driver

**`local`** runs `package.json` scripts written by whoever opened the pull request, with this process's
privileges. That is fine for a repository whose contributors you already trust, and it is fast because the npm
cache is warm between builds.

**`docker`** runs the same thing in a container with a memory and CPU cap and no host environment. The correct
choice if the set of people who can open a pull request is larger than the set of people you would give a shell to.

### The build cache

A workspace is created per commit and deleted with its siblings, so nothing a build downloads survives inside it.
`cache_dir` is where the downloads go instead. Without it every push refetches the whole dependency tree, which for
a Docusaurus site is about two minutes before the build starts.

One cache per pull request, at `<cache_dir>/<preview-id>/`, holding `npm/`, `yarn/` and `pnpm/` and mounted into the
container at `/cache/`. **It is deleted when the preview is** — the same teardown that removes the workspace, the
artifacts and the pull request comment. That is the reason for the key: a cache shared by every branch has no moment
at which anything knows it is safe to remove, and grows until somebody notices the disk.

The cost is that the first build of each pull request is cold. Every build after it is warm, which is where the
pushes actually repeat. The branch name is not part of the key, so a force-push or a rename keeps the cache the pull
request already filled.

`node_modules` is **not** cached, and does not touch the mounted workspace at all — it gets its own docker volume for
the life of the build. That is where the build time actually went: installing this project's own `www/` took 5m46s
writing `node_modules` through the bind mount and 14s writing it to a volume, with the same warm package cache.
Downloading was never the slow part.

This is the largest and most frequently rewritten directory docpreview owns, so it is worth pointing at a disk with
room. To clear one pull request's by hand, use **Clear cache** beside its build log — reach for it when a build
fails on a package that has not changed. Only that pull request refetches.

Fork pull requests are refused at the webhook under either driver, so the exposure is limited to people with
push access to a branch. Whether that is a meaningful limit depends on your repository.

## `preview`

| Field | Default | Notes |
|---|---|---|
| `ttl` | `72h` | Idle lifetime. Refreshed on every rebuild, so an active pull request never expires. |
| `teardown_on_close` | `true` | Remove the preview and its comment when the pull request closes. |

The reaper runs hourly. Preview TTLs are measured in days, so finer is wasted wakeups and coarser leaves a
dead link in a comment for most of a working day.

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
- [CLI reference](./cli.md)
