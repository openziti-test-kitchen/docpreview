---
id: troubleshooting
title: Troubleshooting
sidebar_position: 9
---

# Troubleshooting

## The baseUrl trap

This deserves its own section because it is the failure that costs people an afternoon.

### What goes wrong

Docusaurus bakes `baseUrl` into every emitted `href` and `src` **at build time**. Build a site for
`/my-project/` and serve it at `/`, and:

- `index.html` loads. Status 200. Everything looks like it worked.
- Every stylesheet 404s.
- Every JavaScript chunk 404s.
- The page renders as unstyled text with no navigation.
- The build log says nothing. It was a successful build.

The reviewer sees a broken page and reports "the preview is broken". Nobody looks at asset URLs.

### What docpreview does

Before publishing, it reads the built `index.html`, works out what base URL the site was actually built for,
and compares it to where the preview is about to be mounted. If they disagree, it refuses:

```text
the site was built for a different base URL than the preview will serve.
  preview base URL:    /
  built-in base URL:   /my-project/
  asset in index.html: /my-project/assets/css/styles.a1b2c3.css

Serving it anyway would load index.html and 404 every stylesheet and script,
which looks like a broken preview rather than a configuration mismatch.

Fix it either way round:
  - set build.base_url to "/my-project/" in .docpreview.yml, or
  - make docusaurus.config read the environment:
        baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
```

That message lands in the pull request comment, so the person who can fix it is the person who reads it.

### How the inference works

The obvious approach — longest common prefix of the absolute references — is wrong in both directions. A
single hand-written `href="/"` in a footer collapses the prefix to `/` and hides a real mismatch. A site whose
every asset lives under `/assets/` reports its base URL as `/assets/`.

Instead, docpreview counts the **first path segment** of each absolute reference. A root-mounted Docusaurus
site scatters across `/assets`, `/img`, `/docs`, `/blog` — no segment dominates. A site built for
`/my-project/` puts every single reference under one segment. If one segment accounts for 60% or more, that is
the base URL; otherwise it is `/`. Robust to stray links in both directions.

### The two fixes

**Make the config configurable** (preferred — one line, and existing deployments are unaffected):

```ts
baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
```

**Or tell docpreview where to serve it** (when you cannot change the config):

```yaml
# .docpreview.yml
build:
  base_url: /my-project/
```

The preview is then served at `https://<name>.share.zrok.io/my-project/`, and a request to the bare origin
redirects there.

---

## No comment appears on the pull request

Work down the chain. Each step tells you whether to keep going.

### 1. Did GitHub send the webhook?

App settings → **Advanced** → **Recent Deliveries**.

| | |
|---|---|
| Nothing listed | The event never fired. Is the App installed on **this** repository? Is **Pull request** ticked under events? |
| Red ❌, no response | Your URL is unreachable. Is the zrok share still running? |
| `401` | Signature mismatch — the secret in the App and the one in the vault differ. Regenerate and re-store. |
| `202` | docpreview accepted it. Keep going. |

Every delivery has a **Redeliver** button, so you can iterate without pushing more commits.

### 2. What did docpreview do with it?

```powershell
docpreview serve -log-level debug
```

| Log line | Meaning |
|---|---|
| `refusing to build a fork pull request` | Working as intended. Forks are never built. |
| `ignoring pull_request action` | The action was `labeled`, `assigned`, or similar. Only `opened`, `synchronize`, `reopened`, and `ready_for_review` build. |
| `documentation change detected` | It is building. |
| `skipped` | Detection said no. See below. |

### 3. Is the comment there but you missed it?

On a busy pull request, look for the `docpreview` check in the checks list — that link goes to the same
information.

---

## The webhook URL looks broken

The fastest way to test it is not a browser:

```powershell
docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github
```

It sends a correctly signed ping using the secret from the vault, and names the cause of whatever comes back. The
secret is never printed and never reaches your shell history.

### Opening the webhook URL in a browser gives an error

Nothing is wrong. A browser sends `GET`; GitHub sends `POST`, and only `POST` is served. A `GET` on the webhook
path answers **405** with a sentence saying so; any other path answers **404** and says nothing. Use
`docpreview webhook-check`.

### `502` from the public URL

zrok has the share, but nothing is attached behind it. Either `webhook-only` is not running, or it is still
attaching its listener — that takes a few seconds after startup. Wait, then retry.

### `401` from `docpreview webhook-check`

The request arrived and verification ran, so the tunnel is fine. The webhook secret in the vault you just read is
not the one the running daemon holds. Two causes:

- `-config` points at a different config, and therefore a different vault, than the daemon is using.
- The secret was rotated after that daemon read it.

Restart the daemon, or re-store the secret:

```powershell
docpreview vault set github.webhook_secret
```

### `501` from the webhook

The daemon has no GitHub client. Either `github.app_id` is unset, or the vault was locked when the daemon started
and nothing has unlocked it since.

```powershell
docpreview doctor
```

`doctor` prints the vault path, the key source, and whether GitHub is wired up — a locked vault shows as
`github (app 12345, not wired up: the vault is locked)`.

### `404` from `docpreview webhook-check`

The tunnel is up but the path is wrong. It must be exactly the path `webhook-only` was given — `/webhook/github`
unless you passed `-path`.

### `name is already in use by another share` when starting `webhook-only`

An abandoned share from a killed process still holds the reserved name. A tunnel process is usually ended by a
kill, which runs no cleanup, so this is the ordinary case rather than an exceptional one.

`webhook-only` reclaims it automatically and retries. If it still fails, the name is held by something it will not
touch — find and remove it by hand:

```powershell
zrok2 list shares
zrok2 delete share <token>
```

---

## The daemon will not start, or the vault is locked

A locked vault is not an error. It means the master key was not available at startup, so anything that reads a
secret — the GitHub client, a build secret, the Frontdoor token — is not wired up yet.

Two ways to fix it. Give the daemon the key so a restart needs nobody:

```yaml
vault:
  key_source: "file:/etc/docpreview/master.key"   # or "exec:op read op://ops/docpreview/master-key"
```

Or unlock it by hand from the dashboard's **Secrets** page. The daemon rewires itself when the vault opens; no
restart is needed.

`docpreview doctor` reports which of these applies. With no key source it prints
`key: none — the daemon starts locked and is unlocked from the dashboard`.

### The Secrets page shows the credentials but no input fields

It is read-only, and the banner at the top says why. The request did not originate on the machine running
docpreview. Writing a credential requires **both** of:

- a loopback `RemoteAddr`, and
- no forwarding header — `X-Forwarded-For`, `X-Forwarded-Host`, `X-Real-Ip` or `Forwarded`.

A tunnel satisfies the first and fails the second, which is the point: `zrok2 share public http://127.0.0.1:8471`
would otherwise put the credential API on the internet while the listener is still loopback. Open the page on the
daemon's own machine, tunnel `docpreview webhook-only` rather than the daemon, or set the value on the host with
`docpreview vault set <key>`.

The whole panel is replaced by **Not available on this daemon** instead when a listener is not loopback, or when any
listener is ziti — the credential surface does not check the dialing identity yet, so writes are refused outright.

---

## It says "Skipped — no documentation changes"

Detection matched nothing. The comment names the count of changed files; the debug log names the patterns.

If your documentation is not under `docs/` or `blog/`, tell it where:

```yaml
# .docpreview.yml
detect:
  paths:
    - "content/**"
    - "**/*.md"
```

For anything more nuanced, use a [detect script](./reference/repo-config.md#script).

---

## The build fails

The tail of the build output is in the comment, in a collapsed `<details>` block.

### `no package.json in "."`

The site is in a subdirectory:

```yaml
build:
  dir: website
```

### `build produced no output at "build"`

Your generator emits somewhere else — `out` for Next.js export, `public` for Hugo, `_site` for Jekyll:

```yaml
build:
  output: out
```

### `npm ci` fails on a missing lockfile

docpreview falls back to `npm install` when there is no `package-lock.json`, so this usually means the
lockfile is out of sync with `package.json` rather than absent. Regenerate it and commit.

### `'yarn' is not recognized` / `pnpm: command not found`

The install command is chosen by which lockfile is committed — see
[dependency install](reference/repo-config.md#dependency-install). Under the local driver, that package
manager has to be on the daemon's `PATH`; under the Docker driver it has to be in the image, and
`node:24-bookworm-slim` ships only npm. Either install it (`corepack enable`) or set `build.image` to
something that has it.

### The build installs the wrong dependency tree

Two lockfiles in the same directory. The order is yarn, then pnpm, then npm — the first one found wins, so a
stale `package-lock.json` left behind after a move to yarn is ignored, but a stale `yarn.lock` after a move
to npm is not. Delete the one you no longer use.

### `build timed out after 15m`

```yaml
build:
  timeout: 30m
```

Or switch to the Docker driver, which gets a clean, predictable environment each time.

---

## Every build takes minutes on `npm ci`

Look for a line like this in the build log:

```text
added 1325 packages in 2m
```

If that number does not fall on the second build of the same pull request, the time is going into writing
`node_modules`, not into downloading. Two things fix it, and both are already on by default under the Docker driver
— so seeing this means one of them is not in effect.

**`node_modules` must be on a volume, not on the mounted workspace.** The workspace is bind-mounted from the host,
and on Windows that mount crosses into NTFS, where writing the tens of thousands of small files in a dependency tree
is roughly twenty times slower. Measured on this project's own `www/`, with the same warm cache: **5m46s** with
`node_modules` on the mount, **14s** with it on a volume. The driver mounts one at `<build dir>/node_modules` for
you. Under the **local** driver there is no mount and no volume, so this does not apply.

**The package cache has to survive between builds.** See [`build.cache_dir`](reference/configuration.md#the-build-cache).
It is per pull request and deleted when the preview is, so the *first* build of each pull request pays for its
downloads and every later one does not.

If the second build is still slow, check that `cache_dir` is on a disk with room and that nothing is clearing it
between builds.

---

## The preview URL does not work

### `docpreview doctor` fails on zrok

| Error | Fix |
|---|---|
| `zrok environment is not enabled` | `zrok2 enable <account-token>` |
| `zrok controller rejected this environment` | Token revoked or environment deleted server-side. `zrok2 disable` then `zrok2 enable`. |
| `no zrok namespace configured` | Set `exposer.zrok2.namespace`, or give the environment a default. |

### `name already in use`

Something holds that zrok name. docpreview tries once to reclaim a name held by one of its own orphaned shares
and retries; if that fails, it belongs to something else. Check `zrok2 list shares`.

The commonest cause is **two previews rendering to the same name**. The default template includes the
repository to prevent it, so this means a `name_template` was set that does not:

```yaml
exposer:
  zrok2:
    name_template: "{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}"
```

### `the name "x" is already serving a different preview`

Two open pull requests render to one label. The error names the other preview's ID. Widen `name_template` — see
[the table](reference/configuration.md#name_template).

Refusing is deliberate. Taking the name would point a URL somebody is already reading at a different site.

### A preview says `ready` but the link is refused

Under the `local` exposer the URL is an ephemeral port that does not survive a daemon restart, and the row is
only rewritten when the preview is republished. Push again, or use an exposer with a stable name.

`demo\Test-PreviewUrls.ps1` fetches every advertised URL and reports which ones answer.

### The URL loads but everything is unstyled

You are in the baseUrl trap, and the check that should have caught it did not — most likely the site has fewer
than three absolute references in `index.html`, which is below the threshold for inferring anything. Set
`build.base_url` explicitly.

---

## Previews vanish after a restart

They should not. On startup docpreview republishes every recorded preview from the artifacts already on disk,
which takes seconds.

| Log line | Meaning |
|---|---|
| `dropping preview with missing artifacts` | The artifacts directory was deleted. Push again to rebuild. |
| `recovered previews_restored=0` | The database is empty. Different `data_dir`? |

If the URL changed during recovery, the comment is updated to match — a comment pointing at a dead URL is
worse than no comment.

---

## Everything looks fine and nothing happens

```powershell
curl http://127.0.0.1:8471/status
```

`pending` above zero with nothing progressing means the workers are stuck. Restart with `-log-level debug`.

`pending` zero and no previews means no event ever arrived. Go back to step 1.
