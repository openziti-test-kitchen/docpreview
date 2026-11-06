---
id: quickstart
title: Quickstart
sidebar_position: 2
---

# Quickstart

Two parts. Part 1 builds a site and publishes it at a URL in about five minutes, with no GitHub App and no
source-control integration at all. Part 2 wires it to pull requests, which costs ten minutes in GitHub's App form.

Do part 1 before deciding whether you want part 2.

## Part 1 needs

| | |
|---|---|
| **Go 1.26+** | To build `docpreview`. |
| **git** | To clone it. |
| **Docker** | To run the site build. Builds run in a throwaway container by default. Node 20+ on the host replaces it if you choose the `local` build driver. |
| **A zrok v2 account** | For a public URL. See the [zrok guide](./guides/zrok2.md). |

## Part 2 additionally needs

| | |
|---|---|
| **A GitHub repository** | Containing a site that builds to static files, with admin rights so you can install an App on it. |
| **A public webhook URL** | GitHub has to reach you, so the `local` exposer will not do. |

On Bitbucket instead, follow the [Bitbucket guide](./guides/bitbucket.md) for part 2.

---

## Part 1: see it work

### 1. Build

```powershell
git clone https://github.com/openziti-test-kitchen/docpreview
cd docpreview
go build -o bin/ ./cmd/docpreview
```

The binary lands in `bin/`. Put that directory on your `PATH`, or call `./bin/docpreview` wherever this page says
`docpreview`.

### 2. Write a config

```powershell
docpreview init
```

One question: which exposer. Take `zrok2`.

Everything else takes a default, printed in a summary at the end so nothing is hidden. `-advanced` asks about all of
them. `-yes` asks about none.

It does **not** ask for a GitHub App ID. Source control is the last thing you wire up, and on Bitbucket you never
will. See part 2.

<details>
<summary>The parts of it that matter, if you would rather write it by hand</summary>

Abridged. The real file carries a comment above nearly every key, plus sections for the exposers you did not choose,
so switching later is an edit and not research.

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
  driver: "docker"
  image: "node:24-bookworm"
  timeout: 15m0s
  keep_logs: 168h0m0s

preview:
  ttl: 72h0m0s
  teardown_on_close: true
```

Every field has a default, and a missing file is not an error, so anything absent here is still set. `init` does not
write `preview.keep_builds`, `build.cache_dir` or `dashboard_url`. Add them by hand from
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
driver:  docker
listen:  tcp 127.0.0.1:8471
key:     none — the daemon starts locked and is unlocked from the dashboard
login:   none — anyone who can reach this daemon can read it (docpreview console password -role viewer)
scm:     none — set local.enabled or github.app_id to receive webhooks

all checks passed
```

`scm: none` is expected at this stage. You have not wired up GitHub, and part 1 does not need it.

With the `zrok2` exposer this makes a real call to the controller, so a pass means the environment works, not merely
that it exists on disk.

No master key was asked for. The vault opens lazily, and nothing here has a secret to store.

### 4. Publish something

You are still in the clone from step 1, which holds this documentation site in `www/`. Publish it:

```powershell
docpreview preview -build -name my-first-preview ./www
```

```text
  https://my-first-preview.shares.zrok.io/

  serving D:\repo\www\build
  Ctrl-C to stop.
```

`-build` installs dependencies first (`npm ci`, or yarn or pnpm, by lockfile) and runs the build, with the build
driver from your config. The first run takes a couple of minutes. Without `-build`, the directory is served as it is.

`-name` sets the hostname label. Without it the label is the directory's own name, which for `./www` would be `www`.
That is fine on a private zrok instance and likely taken on a shared one.

Open the URL. That is this documentation site, built from source and published through your exposer. It went through
the same build, the same [baseUrl check](./troubleshooting.md#the-baseurl-trap), and the same static server the
daemon uses for a real pull request. Only the webhook and the comment are missing.

Now point it at your own site:

```powershell
docpreview preview -build -name my-site C:\path\to\your\site
```

Ctrl-C withdraws the URL.

**If it refuses to publish** because the site was built for a different base URL, you have met the one trap worth
knowing about, caught before it could waste your afternoon. See [If your baseUrl is not `/`](#if-your-baseurl-is-not-)
below, or pass `-base-url /whatever/` to see it work now.

:::note No zrok account?

Set `exposer.kind: local` and `docpreview preview` serves the site at `http://127.0.0.1:<port>/preview/<name>/`, on a
port it picks for itself. Only this machine can open that URL. To see the whole pipeline with no account anywhere,
use the [local platform](./local-platform.md) with `docpreview serve`.

:::

---

## Part 1½: previews nobody else can reach

Optional, and about two minutes on top of part 1.

Everything above produces a **public** URL. zrok can gate it behind OAuth, but the address still exists on the
internet, and something has to decide whether to let each visitor through. The alternative is a preview reachable
only from a machine running an OpenZiti tunneler with an enrolled identity. The hostname does not resolve, the
address is not routable, and the service cannot be dialed without a right granted on the controller. For unreleased
content that is a very different posture from "public URL, unguessable name".

Four commands, from nothing:

```bash
# get the ziti CLI first: https://openziti.io/docs/downloads
ziti edge quickstart          # a whole OpenZiti network; leave it running
docpreview configure ziti
docpreview preview -build -name my-first-preview ./www
docpreview serve              # after stopping the preview
```

`configure ziti` creates every controller object the daemon needs: the intercept config, the services, the Bind and
Dial policies, and the daemon's own enrolled identity. Then it writes your config file, so `preview` and `serve` work
immediately. Running it again changes nothing. See [`configure ziti`](./reference/cli.md#configure-ziti) for what
each flag moves.

It leaves two files. One is the daemon's identity, already in the config. The other is a reviewer's enrollment token:

```text
Files
-----
  hosting identity  /home/you/.docpreview/ziti/docpreview-host.json
  reviewer token    /home/you/.docpreview/ziti/reviewer-alice.jwt
```

Import that `.jwt` into a tunneler. In
[Ziti Desktop Edge](https://netfoundry.io/docs/openziti/how-to-guides/tunnelers/windows/),
press `+`, choose "Ziti JWT", pick the file, and switch the identity on. On Linux or macOS:

```bash
ziti-edge-tunnel add --jwt "$(cat ~/.docpreview/ziti/reviewer-alice.jwt)" --identity reviewer
```

Then the preview opens at:

```text
  http://my-first-preview.docpreview.ziti/
```

Turn the tunneler off and the hostname stops resolving. That is the whole point.

Stop the preview before you start `serve`. Both bind the same ziti service, and two bindings split the traffic
between them, so every preview would work half the time.

The dashboard gets the same treatment. `configure ziti` writes both a loopback listener and an overlay one, so
`/status`, which lists every open preview, is reachable through the tunneler as well as from this machine. Delete the
`tcp` entry from `listeners` once you trust the overlay.

---

## Part 2: wire it to pull requests

Only now. Everything above already worked without it. These steps are for GitHub. For Bitbucket, follow the
[Bitbucket guide](./guides/bitbucket.md).

### 5. Make a vault key

The **vault** is one file, `~/.docpreview/vault.age`, holding every credential the daemon needs, encrypted with
[age](./background/age.md). It is not a service and there is nothing to install.

Mint a key and set it in one step. `-shell` emits an assignment in your shell's own syntax, and the pipe evaluates it:

```powershell
docpreview vault keygen -shell | Invoke-Expression
```

```bash
eval "$(docpreview vault keygen -shell)"
```

The shell is detected from the process tree, so `-shell` on its own is almost always right. Force it with
`-shell powershell`, `-shell sh`, `-shell fish`, or `-shell cmd`.

This saves more than typing. Pasting the assignment yourself puts **the key** into your shell history. Piping puts
only the pipeline there.

This command **stores nothing and creates nothing**: not the key, not the vault file. The vault appears in the next
step, on the first `vault set`.

To see the key so you can save it, run it bare:

```powershell
docpreview vault keygen
```

Either way, put the key in a password manager before you close the terminal. The `-shell` form sets it for **this
session only**. It is gone when the shell exits, and the vault cannot be recovered without it. That is what
"encrypted at rest" has to mean to be worth anything.

:::tip Just trying it out?

A passphrase works instead of a generated key. Anything that does not start with `AGE-SECRET-KEY-1` is stretched with
scrypt and used directly. It is slower to open, by design, and fine for a trial. Use a real key for anything
long-running: a passphrase a human can remember is one a wordlist can too.

:::

### 6. Create the GitHub App

Follow the [GitHub App guide](./guides/github-app.md) end to end. You will come back with:

- `github.app_id`: set it in `~/.docpreview/config.yml`, replacing the `0`
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
driver:  docker
listen:  tcp 127.0.0.1:8471
vault:   C:\Users\you\.docpreview\vault.age (2 secrets)
key:     none — the daemon starts locked and is unlocked from the dashboard
login:   none — anyone who can reach this daemon can read it (docpreview console password -role viewer)
scm:     github (app 1234567)

all checks passed
```

The `vault` line is new since step 3, now that something needs it, and `scm` names the App. `doctor` asks GitHub who
the App is, so a pass means the credentials work, not merely that they are present.

### 8. Run it

```powershell
docpreview serve
```

If this reports that no source control is configured, `app_id` is still `0` in the config file.

### 9. Try it

In your site's repository, on a branch:

```bash
git checkout -b docs/quickstart-test
echo "" >> docs/intro.md
echo "A line added to test the preview." >> docs/intro.md
git commit -am "docs: test the preview pipeline"
git push -u origin docs/quickstart-test
gh pr create --fill
```

Within a minute the pull request gets a comment:

> **NetFoundry Preview**
>
> | Project | Status | Preview | Commit | Updated (UTC) |
> |---|---|---|---|---|
> | acme/docs | ✅ Ready · 41s | [Preview](https://docs-docs-quickstart-test.shares.zrok.io/) | `a1b2c3d` | Jul 27, 2026 2:22pm |

The heading comes from `preview.title`. Push again, and the **same comment** updates: the timestamp and commit
change, the URL does not.

While the build runs, the status reads `🔨 Building`. With
[`dashboard_url`](./reference/configuration.md#dashboard_url) set, the project links to the dashboard and the status
to that build's log, which tails as it runs. Without it, both are plain text.

### 10. Open the dashboard

```text
http://127.0.0.1:8471/
```

Every preview, the activity feed, and each build's log, live. Two more pages, **Settings** and **Projects**, are
linked from it, but only when you open it on the machine running the daemon. From anywhere else the links are absent,
because the daemon would refuse the writes they lead to.

The link in the comment is the **branch** URL and always serves the latest build. A rebuild serves the new build on
the same share, so a push costs no zrok calls. To give every build its own URL as well, pinned to its commit, set
`exposer.build_shares: true`. See [exposers](./exposers.md#one-share-per-branch-unless-you-ask-for-more).

:::note After a restart, wait for the log line

Shutdown leaves every zrok share in place, and the next start adopts them instead of creating new ones. Until a
preview is adopted its URL has nothing behind it, and `/status` reports no activity until recovery finishes. Wait for
the `recovered` log line. See
[troubleshooting](./troubleshooting.md#every-preview-404s-for-the-first-minute-after-a-restart).

:::

## If your `baseUrl` is not `/`

Say `docusaurus.config.ts` hardcodes something like `baseUrl: '/my-project/'`, as it does if you deploy to GitHub
Pages. The build succeeds, and the daemon then refuses to publish, telling you the site was built for a different
base URL than the preview would serve.

Fix it in whichever direction suits you. Make the config read the environment:

```ts
baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
```

Or tell the daemon to serve it where the site expects, with `.docpreview.yml` in the repository root:

```yaml
build:
  base_url: /my-project/
```

Either works. The refusal exists because serving it anyway produces an unstyled wall of text with no explanation
anywhere. See [Troubleshooting](./troubleshooting.md).

## Next

- [Repository configuration](./reference/repo-config.md): tuning detection and the build
- [Architecture](./architecture.md): what is actually happening
- [Security model](./reference/security.md): before you point this at anything sensitive
