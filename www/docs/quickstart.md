---
id: quickstart
title: Quickstart
sidebar_position: 2
---

# Quickstart

Two parts. Part 1 builds a documentation site and publishes it at a URL — about five minutes, and it needs no
GitHub App and no source-control integration at all. Part 2 wires it to pull requests, and that one costs ten
minutes in GitHub's App form.

Do part 1 before deciding whether you want part 2.

## Part 1 needs

| | |
|---|---|
| **Go 1.26+** | To build docpreview. |
| **git** | To clone it. |
| **Node 20+** | To run the documentation build. Not needed if you set the Docker build driver. |
| **A zrok v2 account** | Only for a *public* URL. Skip it and choose the `local` exposer to get a loopback URL and nothing else to sign up for. See the [zrok guide](./guides/zrok2.md). |

## Part 2 additionally needs

| | |
|---|---|
| **A GitHub repository** | Containing a Docusaurus site, with admin rights so you can install an App on it. |
| **A public webhook URL** | Which means the `local` exposer will not do — GitHub has to reach you. |

Bitbucket is not wired up yet. See [Status](https://github.com/openziti-test-kitchen/docpreview#status).

---

## Part 1 — see it work

### 1. Build

```powershell
git clone https://github.com/openziti-test-kitchen/docpreview
cd docpreview
go build -o bin/ ./cmd/docpreview
```

### 2. Write a config

```powershell
docpreview init
```

One question: which exposer. Take `zrok2`, or `local` if you have not enabled a zrok environment and want to
see the pipeline run anyway.

Everything else takes a default, printed in a summary at the end so nothing is hidden. `-advanced` asks about
all of them. `-yes` asks about none.

It does **not** ask for a GitHub App ID. Source control is the last thing you wire up, and on Bitbucket you
never will — see part 2.

<details>
<summary>The parts of it that matter, if you would rather hand-author it</summary>

Abridged — the real file carries a comment above nearly every key, and sections for the exposers you did not
choose so that switching later is an edit rather than research.

```yaml
listen: "127.0.0.1:8471"
data_dir: "/home/you/.docpreview"
workers: 2

exposer:
  kind: "zrok2"
  zrok2:
    namespace: ""
    name_template: "{{.Repo.Name}}-{{.Name}}"
    open: true

github:
  app_id: 0            # part 2 fills this in
  api_base: "https://api.github.com"

build:
  driver: "local"
  timeout: 15m0s
  keep_logs: 168h0m0s

preview:
  ttl: 72h0m0s
  teardown_on_close: true
```

Every field has a default and a missing file is not an error, so anything absent here is still set. `init` does
not currently write `preview.keep_builds`, `build.cache_dir` or `dashboard_url`. Add them by hand from
[the server configuration reference](./reference/configuration.md) if you want them stated.

</details>

### 3. Check it

```powershell
docpreview doctor
```

```text
config:  C:\Users\you\.docpreview\config.yml
data:    C:\Users\you\.docpreview
exposer: zrok2
driver:  local
scm:     none — set github.app_id to receive webhooks

all checks passed
```

`scm: none` is expected at this stage — you have not wired up GitHub, and part 1 does not need it.

With the `zrok2` exposer this makes a real call to the controller, so a pass means the environment works
rather than merely existing on disk. With `local` there is nothing remote to check and it always passes.

No master key was asked for. The vault is opened lazily, and nothing here has a secret to store.

### 4. Publish something

You are still in the docpreview clone from step 1, which contains this documentation site in `www/`. Publish
it:

```powershell
docpreview preview -build -name my-first-preview ./www
```

```text
  https://my-first-preview.shares.zrok.io/

  serving D:\repo\www\build
  Ctrl-C to stop.
```

`-build` runs `npm install` and `npm run build` first, so the first run takes a couple of minutes. Without it,
the directory is served exactly as it is.

`-name` sets the hostname label. Without it the label is the directory's own name, which for `./www` would be
`www` — fine on a private zrok instance, and likely already taken on a shared one.

Open the URL. That is this documentation site, built from source and published through your exposer — the same
build, the same [baseUrl verification](./troubleshooting.md#the-baseurl-trap), and the same static server the
daemon uses for a real pull request. Everything except the webhook and the comment.

Now point it at your own docs:

```powershell
docpreview preview -build -name my-docs C:\path\to\your\docs
```

Ctrl-C withdraws the URL.

**If it refuses to publish**, complaining that the site was built for a different base URL — that is the one
trap worth knowing about, caught before it could waste your afternoon. See
[If your baseUrl is not `/`](#if-your-baseurl-is-not-) below, or pass `-base-url /whatever/` to see it
work now.

---

## Part 1½ — previews nobody else can reach

Optional, and about two minutes on top of part 1.

Everything above produces a **public** URL. zrok can gate it behind OAuth, but the address still exists on the
internet and something has to decide whether to let each visitor through. The alternative is a preview
reachable only from a machine running an OpenZiti tunneler with an enrolled identity: the hostname does not
resolve, the address is not routable, and the service cannot be dialed without a right granted on the
controller. For unreleased documentation that is a materially different posture from "public URL,
unguessable name".

Four commands, from nothing:

```bash
# 1. get the ziti CLI: https://openziti.io/docs/downloads
ziti edge quickstart          # a whole OpenZiti network; leave it running
docpreview configure ziti
docpreview serve
```

`configure ziti` creates every controller object docpreview needs — the intercept config, the services, the
Bind and Dial policies, docpreview's own enrolled identity — and then writes your config file, so `serve`
works immediately. Re-running it changes nothing. See
[`configure ziti`](./reference/cli.md#configure-ziti) for what each flag moves.

It leaves two files. One is docpreview's identity, already in the config. The other is a reviewer's enrollment
token:

```text
Files
-----
  hosting identity  /home/you/.docpreview/ziti/docpreview-host.json
  reviewer token    /home/you/.docpreview/ziti/reviewer-alice.jwt
```

Import that `.jwt` into a tunneler — the `+` button in
[Ziti Desktop Edge](https://netfoundry.io/docs/openziti/how-to-guides/tunnelers/windows/), "Ziti JWT", pick
the file, switch the identity on. On Linux or macOS:

```bash
ziti-edge-tunnel add --jwt "$(cat ~/.docpreview/ziti/reviewer-alice.jwt)" --identity reviewer
```

Then publish something and open it:

```powershell
docpreview preview -build -name my-first-preview ./www
```

```text
  http://my-first-preview.docpreview.ziti/
```

Turn the tunneler off and the hostname stops resolving. That is the whole point.

The dashboard gets the same treatment: `configure ziti` writes both a loopback listener and an overlay one,
so `/status` — which enumerates every open documentation pull request — is reachable through the tunneler as
well as from this machine. Delete the `tcp` entry from `listeners` once you trust the overlay.

---

## Part 2 — wire it to pull requests

Only now, and only for GitHub. Everything above already worked without it.

### 5. Make a vault key

The **vault** is one file — `~/.docpreview/vault.age` — holding every credential docpreview needs, encrypted
with [age](./background/age.md). It is not a service and there is nothing to install.

Mint one and set it in a single step. `-shell` emits an assignment in your shell's own syntax. The pipe
evaluates it:

```powershell
docpreview vault keygen -shell | Invoke-Expression
```

```bash
eval "$(docpreview vault keygen -shell)"
```

The shell is detected from the process tree, so `-shell` on its own is right almost always. Force it with
`-shell powershell`, `-shell sh`, `-shell fish`, or `-shell cmd`.

This is not only less typing. Pasting the assignment yourself puts **the key** into your shell history. Piping
puts only the pipeline there.

This command **stores nothing and creates nothing** — not the key, not the vault file. The vault appears in the
next step, on the first `vault set`.

To see the key so you can save it, run it bare:

```powershell
docpreview vault keygen
```

Either way, put the key in a password manager before you close the terminal. The `-shell` form sets it for
**this session only** — it is gone when the shell exits, and the vault is unrecoverable without it. That is
what "encrypted at rest" has to mean to be worth anything.

:::tip Just trying it out?

A passphrase works instead of a generated key — anything that does not start with `AGE-SECRET-KEY-1` is
stretched with scrypt and used directly. Slower to open, by design, and fine for a trial. Use a real key for
anything long-running: a passphrase a human can remember is one a wordlist can too.

:::

### 6. Create the GitHub App

Follow the [GitHub App guide](./guides/github-app.md) end to end. You will come back with:

- `github.app_id` — set it in `~/.docpreview/config.yml`, replacing the `0`
- `github.private_key` in the vault
- `github.webhook_secret` in the vault

### 7. Check everything again

```powershell
docpreview doctor
```

```text
config:  C:\Users\you\.docpreview\config.yml
data:    C:\Users\you\.docpreview
exposer: zrok2
driver:  local
vault:   C:\Users\you\.docpreview\vault.age (2 secrets)
scm:     github (app 1234567)

all checks passed
```

Two lines are new since step 3: the vault, now that something needs it, and `scm`. `doctor` asks GitHub who
the App is, so a pass means the credentials work rather than merely being present.

### 8. Run it

```powershell
docpreview serve
```

If this reports that no source control is configured, `app_id` is still `0` in the config file.

### 9. Try it

In your documentation repository, on a branch:

```bash
git checkout -b docs/quickstart-test
echo "" >> docs/intro.md
echo "A line added to test the preview." >> docs/intro.md
git commit -am "docs: test the preview pipeline"
git push -u origin docs/quickstart-test
gh pr create --fill
```

Within a minute the pull request gets a comment:

> **Documentation preview**
>
> | | |
> |---|---|
> | **Status** | ✅ Ready |
> | **Preview** | https://docs-quickstart-test.shares.zrok.io/ |
> | **Name** | `docs-quickstart-test` |
> | **Commit** | `a1b2c3d` |
> | **Built in** | 41s |
> | **Updated** | 2026-07-27 14:22:07 UTC |

Push again. The **same comment** updates — the timestamp and commit change, the URL does not.

### 10. Open the dashboard

```text
http://127.0.0.1:8471/
```

Every preview, the activity feed, and each build's log, live. Two more pages — **Settings** and **Projects** — are
linked from it, but only when you open it on the machine running docpreview. From anywhere else the links are absent
because the daemon would refuse the writes they lead to.

The link in the comment is the **branch** URL and always serves the latest build. Every build also has its own URL,
pinned to its commit, behind **Open build ↗** beside the build picker. See
[every build gets its own URL](./reference/configuration.md#every-build-gets-its-own-url).

:::note After a restart, wait for the log line

A restart deletes every share and republishes from disk, at roughly 14 seconds per preview. Until it finishes every
preview URL 404s and `/status` reports no activity. That is expected — see
[troubleshooting](./troubleshooting.md#every-preview-404s-for-the-first-minute-after-a-restart).

:::

## If your `baseUrl` is not `/`

If `docusaurus.config.ts` hardcodes something like `baseUrl: '/my-project/'` — which it does if you deploy to
GitHub Pages — the build succeeds and docpreview then refuses to publish, telling you the site was built for a
different base URL than the preview would serve.

Fix it in whichever direction suits you. Make the config read the environment:

```ts
baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
```

Or tell docpreview to serve it where the site expects, with `.docpreview.yml` in the repository root:

```yaml
build:
  base_url: /my-project/
```

Either works. The refusal exists because the alternative — serving it anyway — produces an unstyled wall of
text with no explanation anywhere. See [Troubleshooting](./troubleshooting.md).

## Next

- [Repository configuration](./reference/repo-config.md) — tuning detection and the build
- [Architecture](./architecture.md) — what is actually happening
- [Security model](./reference/security.md) — before you point this at anything sensitive
