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

That message is in the build log and in the daemon's own log. It is **not** in the pull request comment — see
[the comment says only that the build failed](#the-comment-says-only-that-the-build-failed) — so the person who
reports "the preview is broken" has to be sent to the log, or told to run
`docpreview preview -build` against the same directory, which prints the identical error locally.

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

The preview is then served at `https://<name>.shares.zrok.io/my-project/`, and a request to the bare origin
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

### There are no Secrets or Projects links on the dashboard

They are drawn only for a request the daemon would accept a write from, and the daemon decides, not the page. Open the
dashboard on the machine running docpreview — `http://127.0.0.1:8471/` — rather than through a tunnel.

Through [`dashboard-only`](reference/cli.md#dashboard-only) they will never appear: that proxy does not forward the
endpoint the page asks, so the fetch 404s and the links stay hidden. That is deliberate, not a fault.

The pages themselves still answer if you type `/secrets` or `/projects`, read-only, with a banner saying why.

### The Secrets or Projects page shows what is stored but no input fields

It is read-only, and the banner at the top says why. The request did not originate on the machine running
docpreview. Writing a credential — or changing a project, or clearing a cache — requires **both** of:

- a loopback `RemoteAddr`, and
- no forwarding header — `X-Forwarded-For`, `X-Forwarded-Host`, `X-Real-Ip` or `Forwarded`.

A tunnel satisfies the first and fails the second, which is the point: `zrok2 share public http://127.0.0.1:8471`
would otherwise put the credential API on the internet while the listener is still loopback. Open the page on the
daemon's own machine, tunnel `docpreview webhook-only` rather than the daemon, or set the value on the host with
`docpreview vault set <key>`.

The whole panel is replaced by **Not available on this daemon** instead when a listener is not loopback, or when any
listener is ziti — neither admin surface checks the dialing identity yet, so writes are refused outright.

The projects page is gated identically and for a stronger reason: a project row decides what command runs on the
build host. See [Projects](reference/projects.md) and [the security model](reference/security.md#the-mutating-surfaces).

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

### The comment says only that the build failed

That is deliberate and complete. A failed build's comment is one line:

```text
The build failed. See the build log for details: https://docpreview-dash.shares.zrok.io/logs/9f2a1c4b7e01
```

Neither the error text nor the build output is quoted. The comment is public on any public repository, and neither
was written with that in mind: an error carries host paths, internal hostnames and third-party API detail, and the
log is whatever a build script chose to print. The redactor removes known secret *values*, which is not the same as
deciding a line is fit to publish.

Nothing is lost. The reason is in the daemon's log and the whole output is in the build log, both on the machine
that ran the build.

The URL in that line comes from [`dashboard_url`](reference/configuration.md#dashboard_url). Unset, the comment says
"on the docpreview dashboard" and links nowhere — which is the commonest reason this line looks unhelpful.

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

### `cannot express ... as a path the docker daemon can mount`

```text
cannot express \\nas\builds\ws as a path the docker daemon can mount;
the docker driver needs the workspace on a lettered drive
```

The Docker driver bind-mounts the workspace, and on Windows it translates the host path to the docker daemon's own
view — `D:\docpreview\workspaces\...` becomes `/mnt/d/docpreview/workspaces/...`. A UNC path, a mapped network
share, or anything else without a drive letter has no such translation, and guessing one produces an empty
`/workspace` and a build that fails on a missing `package.json`.

Point `data_dir` at a lettered local drive, or use the `local` driver. See
[what the docker driver mounts](reference/configuration.md#what-the-docker-driver-mounts).

### `the build output contains a symlink (...), which cannot be published`

The site built, and its output contains a symlink. It is refused rather than published, because the preview file
server blocks path traversal but follows symlinks out of its root — so a link to `/etc/passwd` in the output would
be served to anyone with the preview URL.

Find it and replace it with the file itself. A Docusaurus site does not normally emit one; the usual source is a
build script that links a shared asset directory in rather than copying it.

### A build fails cloning another private repository

The symptom is a build log showing a `git clone` of some *other* repository failing on authentication, or falling
back to SSH and failing there. docpreview holds an installation token for the repository the webhook came from and
nothing else, and it has no SSH key.

Give that project the token as an environment variable, on the dashboard's **Projects** page. The build script then
picks it up and clones over HTTPS. See
[environment variables scoped to a project](reference/projects.md#environment-variables-scoped-to-a-project) — that
page also has the worked example of a script dispatching on one variable per source repository.

A variable set in the server config's `build.secrets` does the same thing for every project at once, which is the
wrong shape when the credentials differ per repository.

---

## Every build takes minutes on `npm ci`

Look for a line like this in the build log:

```text
added 1325 packages in 2m
```

A minute or more there means the time is going into **writing** `node_modules`, not into downloading it. Under the
Docker driver the workspace is bind-mounted from the host, and on Windows that mount crosses into NTFS, where the
tens of thousands of small files in a dependency tree are roughly twenty times slower to write. Measured on this
project's own `www/` with the same warm cache: **5m46s** with `node_modules` on the mount, **14s** with it on a
volume.

The driver mounts a volume at `<build dir>/node_modules` to avoid exactly this, so a slow install means it is not in
effect. Check the driver actually in use — under **local** there is no mount and no volume, and the install writes
wherever the daemon's disk is.

Do not reach for [`build.cache_dir`](reference/configuration.md#the-build-cache) to fix this. It was measured: the
same build was **4m28s** with an empty cache and **4m21s** with a full one. A cold install of 1325 packages,
downloading every one, finishes in 14 seconds once `node_modules` is on a volume. The cache is not what makes builds
fast here.

---

## Rebuilding docpreview fails with "used by another process"

```text
open build.claude\docpreview.exe: The process cannot access the file because
it is being used by another process.
```

A running daemon holds its own executable open, and Windows will not let the linker overwrite it. This is the most
common self-inflicted build failure here, and it is not a toolchain problem.

Stop the daemon, rebuild, start it again. If `docpreview serve` is not in a terminal you can see:

```powershell
Get-Process docpreview -ErrorAction SilentlyContinue | Stop-Process
```

`webhook-only` and `dashboard-only` run from the same executable, so both hold it open too. Stop all three.

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

### `names limit reached; cannot reserve additional names`

Your zrok account is out of reserved names. Each preview URL needs one, and since every *build* gets its own URL as
well as every branch, the count grows with pushes rather than with pull requests.

```powershell
zrok2 list names
```

Names are released when a preview is torn down, so the usual cause is either genuinely more open pull requests than
the account allows, or names left behind by something that could not release them — look for
`could not release an exposer name` in the daemon's log.

Lower [`preview.keep_builds`](reference/configuration.md#preview), close some pull requests, or delete the stragglers:

```powershell
zrok2 delete name <name>
```

The build share is what fails first. The branch share is created before it, so the pull request comment still gets a
working link and the failure appears only in the log.

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

## Every preview 404s for the first minute after a restart

**Expected. Wait for it.** This is the single most misleading thing docpreview does, and it has cost an afternoon.

On startup, in this order:

1. Every remote share docpreview owns is deleted. Nothing was serving them, so by definition they are orphans from
   the process that just exited. **From here until step 2 finishes, every preview URL 404s.**
2. Each recorded preview is republished from the artifacts already on disk — the branch share first, then each
   build's own share.

Reap-before-republish is not an ordering that can be relaxed: reversed, it deletes what it just restored.

Under zrok each of those is a round trip to the controller, at roughly **14 seconds each**, and it is serial. Three
previews is about a minute. Thirty open pull requests is about seven minutes.

Two things inside that window look like failures and are not:

- **`share not found`, or a bare 404, from a preview URL.** The share has been deleted and not yet recreated.
- **`/status` answers `200` with an empty event list.** The activity feed is re-hydrated from the database *after*
  recovery finishes, so the history is not there yet. `previews` fills in as each one comes back.

The line that says it is over:

```text
level=INFO msg=recovered previews_restored=3 build_shares_restored=11 jobs_pending=0
```

Do not diagnose anything before that line appears.

### Previews that really did vanish

| Log line | Meaning |
|---|---|
| `dropping preview with missing artifacts` | The artifacts directory was deleted. Push again to rebuild. |
| `dropping preview whose artifacts cannot be served` | The directory is there and unusable. Push again. |
| `forgetting a pruned build's URL` | Normal. A build past `keep_builds` lost its artifacts; its share is not restored. |
| `recovered previews_restored=0` | The database is empty. Different `data_dir`? |

If the URL changed during recovery, the comment is updated to match — a comment pointing at a dead URL is
worse than no comment.

:::danger One daemon per exposer account

Because startup deletes every share it recognises as its own, and "its own" means the zrok environment on this host
plus docpreview's target tag, **two daemons sharing one zrok account delete each other's live previews** — each
restart wiping the other. Give a second instance its own account.

:::

---

## Everything looks fine and nothing happens

```powershell
curl http://127.0.0.1:8471/status
```

`pending` above zero with nothing progressing means the workers are stuck. Restart with `-log-level debug`.

`pending` zero and no previews means no event ever arrived. Go back to step 1.
