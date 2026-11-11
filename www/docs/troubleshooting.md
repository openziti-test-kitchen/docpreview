---
id: troubleshooting
title: Troubleshooting
sidebar_position: 9
---

# Troubleshooting

## The baseUrl trap

This one gets its own section because it is the failure that costs people an afternoon.

### What goes wrong

Docusaurus bakes `baseUrl` into every emitted `href` and `src` **at build time**. Build a site for `/my-project/`
and serve it at `/`, and:

- `index.html` loads. Status 200. Everything looks like it worked.
- Every stylesheet 404s.
- Every JavaScript chunk 404s.
- The page renders as unstyled text with no navigation.
- The build log says nothing. It was a successful build.

The reviewer sees a broken page and reports "the preview is broken". Nobody looks at asset URLs.

### What NetFoundry Preview does

Before publishing, it reads the built `index.html`, works out what base URL the site was built for, and compares it
to where the preview is about to be mounted. If they disagree, it refuses:

```text
the site was built for a different base URL than the preview will serve.
  preview base URL:    /
  built-in base URL:   /my-project/
  asset in index.html: /my-project/assets/css/styles.a1b2c3.css

Serving it anyway would load index.html and 404 every stylesheet and script,
which looks like a broken preview rather than a configuration mismatch.

Fix it either way round:
  - set build.base_url to "/my-project/" in .nfpreview.yml, or
  - make docusaurus.config read the environment:
        baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
```

That message is in the build log and in the daemon's own log. It is **not** in the pull request comment (see
[the comment says only that the build failed](#the-comment-says-only-that-the-build-failed)). Send whoever reports
"the preview is broken" to the log, or have them run `nfpreview preview -build` against the same directory, which
prints the same error locally.

### How the inference works

The obvious approach, the longest common prefix of the absolute references, is wrong in both directions. One
hand-written `href="/"` in a footer collapses the prefix to `/` and hides a real mismatch. A site whose every asset
lives under `/assets/` reports its base URL as `/assets/`.

Instead, the check counts the **first path segment** of each absolute reference. A root-mounted Docusaurus site
scatters across `/assets`, `/img`, `/docs`, `/blog`, and no segment dominates. A site built for `/my-project/` puts
every reference under one segment. If one segment accounts for 60% or more, that is the base URL. Otherwise it is
`/`. Stray links in either direction do not confuse it.

### The two fixes

**Make the config read the environment.** Preferred: one line, and existing deployments are unaffected.

```ts
baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
```

**Or tell NetFoundry Preview where to serve it**, when you cannot change the config:

```yaml
# .nfpreview.yml
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
| `401` | Signature mismatch: the secret in the App and the one in the vault differ. Regenerate and store it again. |
| `202` | The daemon accepted it. Keep going. |

Every delivery has a **Redeliver** button, so you can iterate without pushing more commits.

### 2. What did the daemon do with it?

```powershell
nfpreview serve -log-level debug
```

| Log line | Meaning |
|---|---|
| `ignoring a webhook delivery for a repository with no project` | The App is installed on it and you have not added it. Add the row at `/projects`. See [Projects](./reference/projects.md). |
| `refusing to build a fork pull request` | Working as intended. Forks are never built. |
| `ignoring pull_request action` | The action was `labeled`, `assigned`, or similar. Only `opened`, `synchronize`, `reopened`, and `ready_for_review` build. |
| `documentation change detected` | It is building. |
| `skipped` | Detection said no. See [below](#it-says-skipped). |

### 3. Is the comment there but you missed it?

On a busy pull request, look for the `nfpreview` check in the checks list. Its link goes to the same information.

---

## The webhook URL looks broken

The fastest test is not a browser:

```powershell
nfpreview webhook-check -url https://nfpreview.shares.zrok.io/webhook/github
```

It sends a correctly signed ping using the secret from the vault, and names the cause of whatever comes back. The
secret is never printed and never reaches your shell history.

### Opening the webhook URL in a browser gives an error

Nothing is wrong. A browser sends `GET`. GitHub sends `POST`, and only `POST` is served. A `GET` on the webhook path
answers **405** with a sentence saying so. Any other path answers **404** and says nothing. Use
`nfpreview webhook-check`.

### `502` from the public URL

zrok has the share, but nothing is attached behind it. Either `webhook-only` is not running, or it is still attaching
its listener, which takes a few seconds after startup. Wait, then retry.

### `401` from `nfpreview webhook-check`

The request arrived and verification ran, so the tunnel is fine. The webhook secret in the vault you read is not the
one the running daemon holds. Two causes:

- `-config` points at a different config, and so a different vault, than the daemon is using.
- The secret was rotated after that daemon read it.

Restart the daemon, or store the secret again:

```powershell
nfpreview vault set github.webhook_secret
```

### `501` from the webhook

The daemon has no GitHub client. Either `github.app_id` is unset, or the vault was locked when the daemon started and
nothing has unlocked it since.

```powershell
nfpreview doctor
```

`doctor` prints the vault path, the key source, and whether GitHub is wired up. A locked vault shows as
`github (app 12345, not wired up: the vault is locked)`.

### `404` from `nfpreview webhook-check`

The tunnel is up but the path is wrong. It must be exactly the path `webhook-only` was given: `/webhook/github`
unless you passed `-path`.

### `webhook-only` cannot create its share

`webhook-only` keeps its share across restarts. Shutdown leaves the share on the controller, and the next start
adopts the share bound to its name, so a restart normally creates nothing. When there is nothing to adopt it creates
a share through the [share gate](#a-build-waits-on-zrok-share-queued). If the name is still held by an abandoned
share, it reclaims the name and tries once more.

If that fails too, the name is held by something it will not touch. Find it and remove it by hand:

```powershell
zrok2 list shares
zrok2 delete share <token>
```

---

## The daemon will not start, or the vault is locked

A locked vault is not an error. It means the master key was not available at startup, so anything that reads a
secret (the GitHub client, a build secret, the Frontdoor token) is not wired up yet.

Two ways to fix it. Give the daemon the key so a restart needs nobody:

```yaml
vault:
  key_source: "file:/etc/nfpreview/master.key"   # or "exec:op read op://ops/nfpreview/master-key"
```

Or unlock it by hand from the dashboard's **Settings** page. The daemon rewires itself when the vault opens, with no
restart.

`nfpreview doctor` reports which of these applies. With no key source it prints
`key: none — the daemon starts locked and is unlocked from the dashboard`.

### There are no Settings or Projects links on the dashboard

They are drawn only for an admin session, and the daemon decides, not the page. Sign in as `admin` over a private
path: `http://127.0.0.1:8471/` on the daemon's machine or through `ssh -L`, a `zrok_private` listener, or a `ziti`
listener. Anywhere else an admin session counts as a viewer, and the login form refuses the admin password. See
[Admin needs a private path](reference/security.md#admin-needs-a-private-path).

Through [`dashboard-only`](reference/cli.md#dashboard-only) they never appear. That proxy does not forward the
endpoint the page asks, so the links stay hidden, and `/secrets` and `/projects` answer 404. That is deliberate.

### The Settings or Projects page shows what is stored but no input fields

It is read-only, and the banner at the top says why: this session is not an admin session. Writing a credential,
changing a project or clearing a cache needs the admin password, signed in over a private path as above. Or set the
value on the host with `nfpreview vault set <key>`.

### The dashboard asks for a login and no password works

With no password set, the dashboard is closed to everyone. Set one on the host:

```sh
nfpreview console password -role admin
```

The projects page is gated the same way for a stronger reason: a project row decides what command runs on the build
host. See [Projects](reference/projects.md) and [the security model](reference/security.md#the-mutating-surfaces).

---

## The preview of `main` is out of date

The permanent branch preview is refreshed when the default branch moves, and that needs a **push** delivery. Push is
not subscribed by default, so a fresh installation gets pull request previews and a `main` preview that updates only
when somebody presses **Rebuild**. A restart builds it only when it is missing or its last build failed.

| Platform | Where |
|---|---|
| GitHub | The App's **Subscribe to events** → tick **Push**. See the [App guide](./guides/github-app.md). |
| Bitbucket | The repository's **Webhooks** → tick **Repository → Push** on the existing hook. |

Subscribing does not mean building constantly. The daemon ignores these on purpose:

- **A push to any other branch.** A branch with an open pull request already arrives as its own event, so building
  here too would build it twice. A branch without one is somebody's work in progress, and previewing every pushed
  branch would publish a URL per branch per contributor.
- **A push to a repository with no branch preview.** Pushes refresh a preview and never create one. Otherwise
  installing the App would publish a `main` preview for every repository it can see the first time anyone pushed.
  Create one from the project's card, or by adding the project.
- **A tag, and a deletion of the default branch.** Neither is a branch that can be built.

Check the delivery arrived at all. The log line reads:

```text
the default branch moved; rebuilding its preview if there is one
```

An ignored delivery says which of the above applied, at debug level.

## It says "Skipped"

Detection matched nothing, so nothing was built. The comment shows ⏭️ **Skipped** and the reason below the table,
for example `None of the 4 changed files matched a documentation path.` The debug log names the patterns.

What counts as a change:

- **No `build.dir`:** the defaults match a Docusaurus layout anywhere in the repository. That covers `docs/`,
  `blog/`, `src/` and `static/` directories, any `.md` or `.mdx` file, the Docusaurus config and sidebars, and the
  package files.
- **`build.dir` set:** only changes under that directory, plus `.nfpreview.yml`. A pull request that touches a README
  elsewhere in the repository is skipped.
- **A `nfpreview-detect` script** in the build directory overrides both. See
  [detect script](./reference/repo-config.md#script).

If your site lives somewhere the defaults miss, list the paths:

```yaml
# .nfpreview.yml
detect:
  paths:
    - "content/**"
    - "**/*.md"
```

---

## The build fails

### The comment says only that the build failed

That is deliberate. The comment's row reads ❌ **Failed**. With
[`dashboard_url`](reference/configuration.md#dashboard_url) set, the word links to that preview on the dashboard,
where the build log is. Without it, the comment adds one line:

```text
The build failed. See the build log on the nfpreview dashboard.
```

and links nowhere, which is the commonest reason this looks unhelpful.

Neither the error text nor the build output is quoted. The comment is public on any public repository, and neither
was written with that in mind. An error carries host paths, internal hostnames and third-party API detail, and the
log is whatever a build script chose to print. The redactor removes known secret *values*, which is not the same as
deciding a line is fit to publish.

Nothing is lost. The reason is in the daemon's log and the whole output is in the build log, both on the machine that
ran the build.

### `no package.json in "."`

The site is in a subdirectory:

```yaml
build:
  dir: website
```

### `build produced no output at "build"`

Your generator writes somewhere else: `out` for Next.js export, `public` for Hugo, `_site` for Jekyll.

```yaml
build:
  output: out
```

### `npm ci` fails on a missing lockfile

With no `package-lock.json`, the build falls back to `npm install`. So this usually means the lockfile is out of sync
with `package.json`, not absent. Regenerate it and commit.

### `'yarn' is not recognized` / `pnpm: command not found`

The install command is chosen by which lockfile is committed. See
[dependency install](reference/repo-config.md#dependency-install). Under the local driver, that package manager has
to be on the daemon's `PATH`. Under the Docker driver it has to be in the image. The default, `node:24-bookworm`,
ships npm and Yarn 1 but not pnpm. Enable it in the build (`corepack enable`) or set `build.image` to an image that
has it.

### The build installs the wrong dependency tree

Two lockfiles in the same directory. The order is yarn, then pnpm, then npm, and the first one found wins. A stale
`package-lock.json` left behind after a move to yarn is ignored, but a stale `yarn.lock` after a move to npm is not.
Delete the one you no longer use.

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

The Docker driver bind-mounts the workspace. On Windows it translates the host path to the docker daemon's own view,
so `D:\nfpreview\workspaces\...` becomes `/mnt/d/nfpreview/workspaces/...`. A UNC path, a mapped network share, or
anything else without a drive letter has no such translation. Guessing one produces an empty `/workspace` and a build
that fails on a missing `package.json`.

Point `data_dir` at a lettered local drive, or use the `local` driver. See
[what the docker driver mounts](reference/configuration.md#what-the-docker-driver-mounts).

### `the build output contains a symlink (...), which cannot be published`

The site built, and its output contains a symlink. It is refused instead of published, because the preview file
server blocks path traversal but follows symlinks out of its root. A link to `/etc/passwd` in the output would be
served to anyone with the preview URL.

Find it and replace it with the file itself. A Docusaurus site does not normally emit one. The usual source is a
build script that links a shared asset directory in instead of copying it.

### A build fails cloning another private repository

The build log shows a `git clone` of some *other* repository failing on authentication, or falling back to SSH and
failing there. The daemon holds an installation token for the repository the webhook came from and nothing else, and
it has no SSH key.

Give that project the token as an environment variable, on the dashboard's **Projects** page. The build script then
picks it up and clones over HTTPS. See
[environment variables scoped to a project](reference/projects.md#environment-variables-scoped-to-a-project). That
page also has a worked example of a script dispatching on one variable per source repository.

A variable set in the server config's `build.secrets` does the same for every project at once, which is the wrong
shape when the credentials differ per repository.

---

## Every build takes minutes on `npm ci`

Look for a line like this in the build log:

```text
added 1325 packages in 2m
```

A minute or more there means the time goes into **writing** `node_modules`, not downloading it. Under the Docker
driver the workspace is bind-mounted from the host. On Windows that mount crosses into NTFS, where the tens of
thousands of small files in a dependency tree are roughly twenty times slower to write. Measured on this project's
own `www/` with the same warm cache: **5m46s** with `node_modules` on the mount, **14s** with it on a volume.

The driver mounts a volume at `<build dir>/node_modules` to avoid exactly this, so a slow install means it is not in
effect. Check which driver is in use. Under **local** there is no mount and no volume, and the install writes
wherever the daemon's disk is.

Do not reach for [`build.cache_dir`](reference/configuration.md#the-build-cache) to fix this. Measured: the same build
took **4m28s** with an empty cache and **4m21s** with a full one. A cold install of 1325 packages, downloading every
one, finishes in 14 seconds once `node_modules` is on a volume. The cache is not what makes builds fast here.

---

## Rebuilding nfpreview fails with "used by another process"

```text
open build.claude\nfpreview.exe: The process cannot access the file because
it is being used by another process.
```

A running daemon holds its own executable open, and Windows will not let the linker overwrite it. This is the most
common self-inflicted build failure here, and it is not a toolchain problem.

Stop the daemon, rebuild, start it again. If `nfpreview serve` is not in a terminal you can see:

```powershell
Get-Process nfpreview -ErrorAction SilentlyContinue | Stop-Process
```

`webhook-only` and `dashboard-only` run from the same executable, so both hold it open too. Stop all three.

---

## The preview URL does not work

### `nfpreview doctor` fails on zrok

| Error | Fix |
|---|---|
| `no zrok environment is enrolled here` | Enrol from `/secrets`, or `nfpreview zrok invite <email>`. Run `nfpreview zrok status` first. See below. |
| `zrok controller rejected this environment` | Token revoked or environment deleted on the server. `nfpreview zrok disable -yes` then `nfpreview zrok enable`. |
| `no zrok namespace configured` | Set `exposer.zrok2.namespace`, or give the environment a default. |

### `no zrok environment is enrolled here`, but `zrok2 overview` shows one

There are two possible environment directories, and you are looking at the other one:

```powershell
nfpreview zrok status
```

`~/.zrok2` is what the `zrok2` CLI enrols into. `<data_dir>/zrok2` is the daemon's own. `zrok status` marks which one
this installation uses, and `nfpreview zrok use system` switches to the machine's. It takes effect at the next
restart.

:::danger Do not point two daemons at one environment

Startup deletes every share in its zrok environment that its own database does not claim. Two daemons on one
environment delete each other's live previews. Give the second one `nfpreview zrok use project` and its own
enrolment, and a [name prefix](./reference/cli.md) so their names cannot collide either.

:::

### A build waits on `zrok share queued`

Every zrok share creation on the machine goes through a gate: at most 15 per 10 minutes per zrok environment, shared
by the daemon, `webhook-only` and `dashboard-only`. A build that needs a share past that waits for the next slot and
says so in its log:

```text
zrok share queued: 15 of 15 used in this window, next slot at 2026-11-06T14:32:00Z
```

Two 429s in a row from the controller start 10 minutes of silence, because retrying inside a refusal only extends
it. The dashboard footer shows `zrok shares N/15`, and `/status` has the same under `zrok_shares`.

In normal use the gate is hard to reach. A rebuild reuses its branch's share and a restart adopts the shares it
already has, so only a new pull request, a new branch preview or a share that could not be adopted costs a creation.
If it fills up anyway, look at whether `exposer.build_shares` is on (one share per build) or a tunnel unit is stuck
in a restart loop.

### `name already in use`

Something holds that zrok name. The daemon tries once to reclaim a name held by one of its own orphaned shares, then
retries. If that fails, the name belongs to something else. Check `zrok2 list shares`.

The commonest cause is **two previews rendering to the same name**. The default template includes the repository to
prevent it, so this means a `name_template` was set that does not:

```yaml
exposer:
  zrok2:
    name_template: "{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}"
```

### `names limit reached; cannot reserve additional names`

Your zrok account is out of reserved names. Each preview needs one, so by default the count grows with open pull
requests and branch previews. With `exposer.build_shares: true` every build needs one too, and the count grows with
pushes.

```powershell
zrok2 list names
```

Names are released when a preview is torn down. So the usual cause is more open pull requests than the account
allows, or names left behind by something that could not release them. Look for
`could not release an exposer name` in the daemon's log.

Close some pull requests, or delete the stragglers:

```powershell
zrok2 delete name <name>
```

With build shares on, lower [`preview.keep_builds`](reference/configuration.md#preview) too. The build share is then
what fails first. The branch share is created before it, so the comment still gets a working link and the failure
shows only in the log.

### `the name "x" is already serving a different preview`

Two open pull requests render to one label. The error names the other preview's ID. Widen `name_template`. See
[the table](reference/configuration.md#name_template).

Refusing is deliberate. Taking the name would point a URL somebody is already reading at a different site.

### A preview says `ready` but the link is refused

Under the `local` exposer the URL is `/preview/<name>/` on the daemon's own listener. It survives a restart, but it
only answers while `nfpreview serve` is running, and only from a machine that can reach that listener. A loopback
listener refuses everyone else.

`demo\Test-PreviewUrls.ps1` fetches every advertised URL and reports which ones answer.

### The URL loads but everything is unstyled

You are in the baseUrl trap, and the check that should have caught it did not. Most likely the site has fewer than
three absolute references in `index.html`, which is below the threshold for inferring anything. Set `build.base_url`
explicitly.

---

## Previews are briefly unreachable after a restart \{#every-preview-404s-for-the-first-minute-after-a-restart}

**Expected. Wait for the `recovered` line.**

Shutdown stops serving but leaves every zrok share on the controller. On startup, in this order:

1. The daemon lists the shares its zrok environment holds and deletes the ones its database no longer claims.
2. It restores each recorded preview from the artifacts on disk, several at a time. A preview whose share still
   exists adopts it, which takes one listener bind and no share creation. Only a preview with no share gets a new
   one, through the [share gate](#a-build-waits-on-zrok-share-queued).

Until a preview is restored, its URL has nothing behind it. With adoption that is usually seconds. A preview that
needs a new share can wait longer, because creations are rationed.

Two things inside that window look like failures and are not:

- **`share not found`, a 502, or a bare 404, from a preview URL.** The preview has not been restored yet.
- **The dashboard shows an empty activity feed.** It is reloaded from the database *after* recovery finishes, so the
  history is not there yet. Previews fill in as each one comes back, and `/readyz` reports the stage.

The line that says it is over:

```text
level=INFO msg=recovered previews_restored=3 build_shares_restored=0 jobs_pending=0
```

Do not diagnose anything before that line appears.

### Previews that really did vanish

| Log line | Meaning |
|---|---|
| `dropping preview with missing artifacts` | The artifacts directory was deleted. Push again to rebuild. |
| `dropping preview whose artifacts cannot be served` | The directory is there and unusable. Push again. |
| `recovered previews_restored=0` | The database is empty. Different `data_dir`? |

If the URL changed during recovery, the comment is updated to match, because a comment pointing at a dead URL is worse
than no comment.

:::danger One daemon per zrok environment

Startup deletes every share in its zrok environment, carrying the `nfpreview:` target tag, that its own database does
not claim. **Two daemons on one environment delete each other's live previews**, each restart wiping the other. Two
installations on one account with separate environments are safe from that, but names belong to the account, so give
the second its own `exposer.prefix`.

:::

---

## Everything looks fine and nothing happens

```powershell
curl http://127.0.0.1:8471/readyz
```

`pending` above zero with nothing progressing means the workers are stuck. Restart with `-log-level debug`.

`pending` zero and `ready` zero means no event ever arrived. Go back to step 1.

Use `/readyz` instead of `/status` because it answers without a login. With a console password set, `/status` from a
terminal returns `401 {"error":"not logged in"}`. That is the gate working, not a fault.

---

## Known issues

Open defects, with whatever workaround exists. Each one is reproducible and none has a fix yet.

### The log pane does not always start tailing a build that begins while you are watching

**Workaround: reload the page.** The pane then follows the running build normally.

A row is open on a finished build, a new build starts underneath it, and the pane keeps showing the old one. It is
the most-reported defect in the dashboard, and a reload always fixes it. That points at state the render path does
not recompute, not at the daemon. The build itself is unaffected: it runs, it publishes, and the comment updates.

`tools/dashboardtest/newbuild.mjs` covers five shapes of the sequence, and all five behave correctly. That includes
the one where the server replays a finished build and then switches the same connection to the live one. So the
cause is in none of them, and the next step is watching it happen against a live daemon instead of a stubbed
`EventSource`. It is not fixed yet.

If you hit it, report the exact sequence: which page, which row, how it was expanded (by clicking the row or an
activity entry), and whether the build came from a push, a **Rebuild**, or the startup scan.

### Switching exposer rewrites every open pull request comment

**Working as designed. Know it before you press it.** Each exposer publishes at its own addresses. Changing
which one is enabled republishes every preview at a new URL and rewrites the comment on every open pull request to
match, at the next restart. The dashboard asks first and says so.

Switching back restores the previous URLs under zrok, where a preview's name is reserved and its URL is derived from
it. Under Frontdoor the share ID is assigned by the tenant, so those URLs do not come back.
