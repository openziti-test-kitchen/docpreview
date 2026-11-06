---
id: cli
title: CLI reference
sidebar_position: 3
---

# CLI reference

NetFoundry Preview ships as one binary, `docpreview`.

```text
docpreview init      [-config FILE] [-force] [-advanced] [-yes]
docpreview configure ziti [-controller URL] [-username U] [-password P] …
docpreview preview   [-build] [-base-url PATH] [-name LABEL] <dir>
docpreview serve     [-config FILE] [-log-level LEVEL]
docpreview doctor    [-config FILE]
docpreview shares    list [-config FILE] [-json] [-log-level LEVEL]
docpreview console   <subcommand>
docpreview settings  <subcommand>
docpreview zrok      <subcommand>
docpreview sim       <subcommand>
docpreview webhook-only    [-zrok-name NAME] [-listen ADDR] [-upstream URL] [-path PATH]
docpreview dashboard-only  [-zrok-name NAME] [-listen ADDR] [-upstream URL]
docpreview webhook-check -url URL [-config FILE] [-timeout D]
docpreview vault     <subcommand>
```

:::note `-config` is not universal

`webhook-only` and `dashboard-only` take **`-zrok-name`, not `-config`**. Neither reads the config file or opens the
vault. A reverse proxy that forwards one path needs neither, and keeping a second copy of the webhook secret in a
second process is what `webhook-only` exists to avoid. Passing `-config` to either one exits `2` with a usage dump.

`vault keygen` has no `-config` either: it mints a key and writes no vault.

:::

Source control is **optional**. `github.app_id: 0` means "not wired up yet". That is where everyone starts, and where
Bitbucket users stay. `init`, `doctor` and `preview` all work in that state, and `serve` starts and says so. Webhooks
are the one thing that needs source control.

## `preview`

Publishes one directory through the configured exposer and holds it open until you interrupt it.

```powershell
docpreview preview -build ./www
```

```text
  https://www.shares.zrok.io/

  serving D:\repo\www\build
  Ctrl-C to stop.
```

**No GitHub App, no webhook, no credentials** beyond what the exposer itself needs, and the `local` exposer needs
nothing. It runs the exposer, the build, the baseUrl check and the static server. That is every part of the pipeline
except the webhook and the comment.

Use it to try NetFoundry Preview before you spend ten minutes creating a GitHub App.

It is also the fastest way to diagnose an unstyled preview. It runs the same
[baseUrl check](../troubleshooting.md#the-baseurl-trap) the daemon does and prints the same error, with no pull
request needed.

| Flag | |
|---|---|
| `-build` | Run the site build first. Without it, the directory is served as-is. |
| `-base-url PATH` | Serve under a prefix. Default: from `.docpreview.yml`, else `/`. |
| `-name LABEL` | Public hostname label. Default: the directory name, sanitized. |
| `-config FILE` | |
| `-log-level LEVEL` | |

The preview ID comes from the label, so running it again against the same directory replaces the previous share
instead of piling up orphans.

## `init`

Writes a config file. **One question.**

```text
docpreview init [-config FILE] [-force] [-advanced] [-yes]
                [-exposer KIND] [-app-id N]
```

```text
$ docpreview init

Writing to /home/you/.docpreview/config.yml
One question. Everything else takes a default (run with -advanced to see them all).

How should previews become reachable?
  zrok2      OpenZiti overlay. Binds no local port. Start here.
  frontdoor  NetFoundry Frontdoor. Adds a WAF and IdP-enforced access.
  local      Loopback only. Try the pipeline without an account.
Exposer (zrok2/frontdoor/local) [zrok2]:
```

The exposer is the only setting where a reasonable person would pick something other than the default and can
answer right now.

The GitHub App ID is **not** asked. Source control is the last thing you wire up. At `init` you have not created the
App yet, and on Bitbucket you never will. It defaults to `0`, and the closing checklist reminds you to come back to
it. Set it with `-app-id` if you already have one.

Everything you did not answer is printed:

```text
Settings
--------
  exposer      zrok2
  preview URL  {{.Name}}.<your zrok namespace>
  visibility   anyone with the link
  github app   not set yet
  listen       127.0.0.1:8471
  data dir     /home/you/.docpreview
  builds       docker driver, 15m0s timeout, 2 at a time
  preview ttl  72h0m0s
```

Then two checklists, one to try it now and one for pull requests later:

```text
Try it now
----------
 1. Enable a zrok environment if you have not:  zrok2 enable <account-token>
 2. Check it:                docpreview doctor
 3. Publish something:       docpreview preview -build ./www

When you want previews on pull requests
---------------------------------------
 1. Create a GitHub App and set app_id in /home/you/.docpreview/config.yml
 ...
```

Both follow from your answers. Choose the `local` exposer and it does not tell you to enable zrok.

### Flags

| Flag | |
|---|---|
| `-config FILE` | Where to write. Default `~/.docpreview/config.yml`. |
| `-force` | Overwrite without asking. |
| `-advanced` | Ask about every setting, not only the exposer. |
| `-yes` | Ask nothing. Take every default. |
| `-exposer KIND` | Set the exposer and skip that question. |
| `-app-id N` | Set the App ID. |

Fully unattended, for a provisioning script:

```bash
docpreview init -yes -exposer zrok2 -app-id 1234567
```

### With `-advanced`

Every setting is asked, and each answer is checked at the prompt:

```text
Listen address [127.0.0.1:8471]: localhost
  not a host:port address (try 127.0.0.1:8471)
Listen address [127.0.0.1:8471]:
```

The name template is checked by *rendering* it, so you see the result before you accept it:

```text
Name template [{{.Name}}]: {{.Repo.Name}}-{{.Name}}
  -> a branch named feature/example would be published as "docs-feature-example"
```

### The output

**Commented YAML**, so the file still explains itself when you copy it to another machine months later. `init` reads
it back through the real config loader before it reports success, so it cannot write a file that `serve` refuses.

`init` writes only the config. It never touches the vault, so it never handles a secret.

## `configure ziti`

Provisions an OpenZiti network and points NetFoundry Preview at it. From nothing to a working private preview takes
four commands:

```bash
# 1. get the ziti CLI: https://openziti.io/docs/downloads
ziti edge quickstart          # leave it running
docpreview configure ziti
docpreview serve
```

It creates, on the controller:

1. an `intercept.v1` config covering `docpreview.ziti` and `*.docpreview.ziti`
2. `docpreview-svc`, the one wildcard service carrying every preview
3. `docpreview-admin`, a second service for the dashboard and webhook endpoint
4. a **Bind** policy per service, so the daemon may host them
5. a **Dial** policy per service, keyed on the `docpreview-reader` role attribute, so adding a reviewer later means
   one attribute on a new identity, not an edit to a live policy
6. a service-edge-router policy per service
7. `docpreview-host`, the daemon's own identity, **enrolled** into `<data_dir>/ziti/docpreview-host.json`
8. `reviewer-alice`, a sample reviewer identity, with a `.jwt` left beside it

Then it writes the config file, so `docpreview serve` works right away.

**Running it again is safe.** It checks for every object before creating it, and a second run reports everything as
already present and changes nothing.

```text
On the controller
-----------------
  created  config docpreview-intercept
  created  service docpreview-svc
  created  service-policy docpreview-bind
  ...

Wrote /home/you/.docpreview/config.yml

Files
-----
  hosting identity  /home/you/.docpreview/ziti/docpreview-host.json
  reviewer token    /home/you/.docpreview/ziti/reviewer-alice.jwt

Next
----
 1. Import reviewer-alice.jwt into Ziti Desktop Edge:
      the + button on the identity list, "Ziti JWT", pick that file,
      then switch the new identity on. …
 2. Check it:         docpreview doctor …
```

The reviewer token is not a URL and not a password. You import the file into a tunneler:
[Ziti Desktop Edge](https://netfoundry.io/docs/openziti/how-to-guides/tunnelers/windows/) on Windows, or
`ziti-edge-tunnel add --jwt "$(cat reviewer-alice.jwt)"` elsewhere. After that `http://<branch>.docpreview.ziti/`
resolves, and it resolves nowhere else.

### Flags

| Flag | Default | |
|---|---|---|
| `-controller` | `https://localhost:1280` | Edge management API. What `ziti edge quickstart` advertises. |
| `-username` | `admin` | |
| `-password` | `admin` | |
| `-domain` | `docpreview.ziti` | DNS suffix previews appear under. Written into the intercept config *and* the docpreview config, so the two cannot disagree. |
| `-service` | `docpreview-svc` | The wildcard preview service. |
| `-admin-service` | `docpreview-admin` | Service for the dashboard and webhook endpoint. Empty leaves the ingress on TCP only. |
| `-host-identity` | `docpreview-host` | The daemon's own identity. |
| `-reviewer` | `reviewer-alice` | Sample reviewer identity. Empty creates none. |
| `-reader-role` | `docpreview-reader` | Role attribute the Dial policies are keyed on. |
| `-prefix` | `docpreview-` | Name prefix for the config and the policies. |
| `-out-dir` | `<data_dir>/ziti` | Where the identity file and the token land. |
| `-no-config` | | Provision the network, leave the config file alone, print the YAML instead. |
| `-config FILE` | `~/.docpreview/config.yml` | |

The loopback listener is **kept** when an admin service is configured, so the written config has both:

```yaml
listeners:
  - tcp: "127.0.0.1:8471"
  - ziti:
      identity_file: "…/docpreview-host.json"
      service: "docpreview-admin"
```

Without the TCP address you could not reach the dashboard until your own tunneler is enrolled and running. Delete the
`tcp` entry once the overlay works.

### Two identities that cannot be recreated the same way

An enrollment token works once, so "already exists" means different things for the two identities.

**The hosting identity** is recreated if it exists on the controller but its file is missing. Once the token is
spent, that file is the only way to authenticate as the identity. This is the one destructive step in the command,
and it only touches the daemon's own identity.

**A reviewer identity** is never touched. A missing `.jwt` is normal once somebody has imported the token, and
deleting the identity would cut off a reviewer whose setup works. If the token was already used, the command says so
and suggests a different `-reviewer` name.

## `serve`

Runs the daemon: webhook ingress, build workers, and the reaper.

With no source control configured, no webhook can arrive. The daemon starts anyway, because you store the GitHub
private key and webhook secret on the [`/secrets`](#endpoints) page, which needs a running daemon. It says on the way
up that nothing can arrive yet:

```text
level=WARN msg="no source control is configured, so no webhooks can arrive"
  fix="add github.app_id, or set local.enabled for the git simulator" config=/home/you/.docpreview/config.yml
```

A locked vault works the same way. With `github.app_id` set and no master key, there is no GitHub client, because
building one reads the App private key and the webhook secret from the vault. `serve` starts, `/webhook/github`
answers `501`, and the client is built when the vault is unlocked from the dashboard. Rotating either credential
rebuilds the client and drops the cached installation token, since the old key minted it.

```powershell
docpreview serve
docpreview serve -config ./dev.yml -log-level debug
```

| Flag | Default | |
|---|---|---|
| `-config` | `~/.docpreview/config.yml` | Also `$DOCPREVIEW_CONFIG`. |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error`. |

Startup checks every component **before binding the port**. If the zrok environment is not enabled, you find out now,
not when the first pull request gets no comment.

Every configured [listener](./configuration.md#listeners) is bound, or none is. One HTTP server serves them all, and
a failure on any one is fatal. Otherwise the process would look healthy while the address people use is gone.

```text
level=INFO msg=listening listeners="tcp 127.0.0.1:8471, ziti service docpreview-admin (identity …)" exposer=ziti
```

Then it reconciles with the exposer. Shares survive a restart, so under zrok it lists what the environment already
holds, adopts every share the database still claims, and deletes the ones nothing claims. Each recorded preview is
then served again from the artifacts already on disk. Nothing is re-cloned and nothing runs `npm install`.

Per-build shares are off by default (`exposer.build_shares: false`). With them off, any per-build shares left from an
earlier run are deleted at start. With them on, each retained build is restored too.

:::warning Preview URLs do not answer until that finishes

An adopted share still needs its listener opened, and a share that has to be created costs a controller round trip of
10 to 15 seconds. Restores run at least four at a time. Share creations also go through the
[share gate](#the-zrok-share-gate), so a first start with many previews can queue.

Until recovery finishes, `/status` answers `200` with an empty event list, because the activity feed is loaded from
the database afterwards. It looks like data loss. Wait for:

```text
level=INFO msg=recovered previews_restored=3 build_shares_restored=0 jobs_pending=0
```

`build_shares_restored` is above zero only with `exposer.build_shares: true`.

See [troubleshooting](../troubleshooting.md#every-preview-404s-for-the-first-minute-after-a-restart).

:::

A share counts as "owned" when it belongs to this host's zrok environment and carries the daemon's target tag. So
**two daemons sharing one zrok environment delete each other's previews** on every restart. Give a second instance its
own environment.

`SIGINT` or `SIGTERM` drains in-flight work and stops serving every publication. Under zrok the shares stay on the
controller, and the next start adopts them, so a restart creates no shares.

### The zrok share gate

zrok limits how fast an account can create shares. Every share creation goes through one gate: **15 per 10 minutes
per zrok environment**, shared by `serve`, `webhook-only` and `dashboard-only` on the same environment. A creation
past the limit waits for the next slot, and the build log says so. Two `429` answers in a row from zrok mean 10
minutes with no creations at all.

A push to a branch that already has a preview costs no zrok calls. The new build is served on the branch's existing
share.

The dashboard footer shows the budget as `zrok shares N/15`, and `/status` carries it as `zrok_shares`.

### The dashboard's three pages

One embedded HTML document, served at three paths and switched on the path, so the styles and the fetch helper exist
once.

| Path | |
|---|---|
| `/` | Previews, the activity feed, and the build log viewer. Live over an `EventSource`. |
| `/secrets` | The credential surface, and nothing else. No stream, because nothing on it changes by itself. |
| `/projects` | [Projects](./projects.md) and their environment variables. No stream either. |

**The links to the last two appear only for a local request.** The page asks `/api/admin`, which runs the same
locality check the write endpoints run, and draws **Projects**, **Settings** and **Clear caches** only on a clear
yes. The server decides, not the page. A `Host` check in the browser would prove nothing, because `Host` is whatever
the client typed. You can still type the paths. Those pages load read-only, with a banner saying why.

:::note A tab left open across a restart runs the code it loaded

The document has no cache busting, so an open tab keeps the old page after the daemon is rebuilt. The page notices
the daemon instance changed and offers a reload. **If the page contradicts the code, reload before you debug.**

:::

### Endpoints

| Method | Path | |
|---|---|---|
| `POST` | `/webhook/github` | HMAC-verified. Returns `202` before doing the work. |
| `POST` | `/webhook/local` | The local platform. No signature. See [Local platform](../local-platform.md). |
| `POST` | `/webhook/bitbucket` | HMAC-verified, with the signature in `X-Hub-Signature`. Bitbucket uses SHA-256 there, although GitHub uses that header name for its legacy SHA-1. Returns `501` only when no Bitbucket credential is stored. |
| `GET` | `/` | The dashboard. |
| `GET` | `/secrets` | Credential management, on its own page so it can be bookmarked, named in a runbook, and gated by a proxy. Registered only when a credential surface is wired, so a daemon without one answers `404`. |
| `GET` | `/projects` | [Projects](./projects.md) and their environment variables. |
| `GET` | `/api/admin` | Whether this request may write to the admin pages. The dashboard asks before drawing **Projects**, **Settings** and **Clear caches**, so it never offers an action that would `403`. |
| `GET` | `/api/secrets` | What `/secrets` reads: names, whether the vault is unlocked, and whether this daemon may be written to at all. Never a value. |
| `PUT` `DELETE` `POST` | `/api/secrets/…` | Store, delete, unlock, generate. Refused unless every listener is loopback *and* the request came from this machine with no forwarding header. Loopback is not the same as local: through a tunnel the listener is loopback while the caller is on the internet. That is why [`webhook-only`](#webhook-only) exists. |
| `GET` `PUT` `DELETE` | `/api/projects/…` | Project rows and their environment variables. Same two gates as `/api/secrets`, because a project row decides what command runs on the build host. |
| `POST` | `/api/projects/…/branch` | Build a project's default branch, or the branch named in the body. This is the permanent preview described in [Projects](./projects.md). Same gates. |
| `POST` | `/api/projects/…/link` | Build one pull request by number, and stop skipping it if it was unlinked. Same gates. |
| `POST` | `/api/builds/{preview}/unlink` | Remove a preview and record that its pull request must not be built again. The ignore is written before the teardown, so a failure partway leaves the pull request unbuilt instead of rebuilding on the next push. Same gates. |
| `POST` | `/api/builds/{preview}/rebuild` | Build again: the recorded commit for a pull request, the branch's current tip for a branch preview. Same gates. |
| `POST` | `/api/builds/{preview}/cancel` | Abandon the build running for a preview. Same gates. |
| `DELETE` | `/api/cache` | Empty every preview's package cache. Same gates. |
| `DELETE` | `/api/cache/{preview}` | Empty one preview's package cache. Same gates. |
| `GET` | `/events` | Server-sent events: the whole status payload on every change. |
| `GET` | `/logs/{preview}/stream` | Server-sent events: one build log, live or replayed. |
| `GET` | `/logs/{preview}` | The builds recorded for one preview. |
| `GET` | `/logs/{preview}/download` | The log as an attachment. |
| `GET` | `/pr` | The stand-in pull request page, for the local platform. |
| `GET` | `/preview/{name}/` | Previews, under the `local` exposer only. |
| `GET` | `/healthz` | `ok`. |
| `GET` | `/readyz` | JSON: whether recovery has finished, and how busy the daemon is. |
| `GET` | `/api/zrok` | JSON: both zrok environments, which is in use, and whether one is enrolled. |
| `POST` | `/api/zrok/use` | Record which environment the daemon adopts. Takes effect at the next restart. |
| `POST` | `/api/zrok/invite` | Ask zrok to email a registration link. Refused if one is already enrolled. |
| `POST` | `/api/zrok/register` | Turn that link into an account and enrol this host. |
| `POST` | `/api/zrok/enable` | Enrol with an account token, from the request or from the vault. |
| `POST` | `/api/zrok/disable` | Remove this host from the account. Takes every preview URL down until republish. |
| `GET` | `/status` | JSON: exposer, queue depth, live previews, and under zrok the share gate as `zrok_shares`. |

```json
{
  "exposer": "zrok2",
  "pending": 0,
  "previews": [
    {
      "preview_id": "9f2a1c4b7e01",
      "repo": "github:acme/docs",
      "number": 42,
      "branch": "feature/new-guide",
      "name": "feature-new-guide",
      "url": "https://feature-new-guide.shares.zrok.io/",
      "state": "ready",
      "updated_at": "2026-07-27T14:22:07Z"
    }
  ]
}
```

`/status` carries no secrets, but it lists every open pull request that has a preview. Keep that in mind before you
publish the ingress next to the webhook endpoint. Once a console password is set, `/status` is behind the login with
the rest of the dashboard, including from loopback.

Poll `/readyz` instead from a script or a supervisor. It answers without a login, and it says how busy the daemon is
without saying what it is busy with: counts and a stage name, never a repository, branch or URL.

```json
{
  "starting": false,
  "pending": 0,
  "running": 0,
  "ready": 8,
  "instance": "20260731-192655.448"
}
```

While recovery runs it also carries `startup` with `stage`, `note`, `done` and `total`, which the restart script
prints while it waits. `instance` is the process start stamp, so a caller can tell a fresh restart from a daemon that
was already running.

## `console`

Sets the dashboard login. The dashboard is open until a password is set. After that, everything except the previews,
the webhooks and the health endpoints asks for a login, including from loopback. See
[The login](./security.md#the-login).

```text
docpreview console password [-role admin|viewer] [-clear] [-config FILE]
docpreview console oauth-domains [D] [-clear] [-config FILE]
docpreview console status [-config FILE]
```

| Subcommand | |
|---|---|
| `password` | Read a password from stdin and store its hash. `-role viewer` sets the read-only password. `-clear` removes the role's password. |
| `oauth-domains` | Let a Google sign-in from these email domains read the dashboard. It grants viewer, never admin. With no argument it prints the list. |
| `status` | What is set, and what this daemon listens on. |

The password is read from stdin so it never lands in shell history or `ps` output. The hash lives in the database,
not the vault, because the page that unlocks the vault is behind this login. A running daemon picks up a change
without a restart.

## `settings`

Settings the daemon keeps in its database. Each has a dashboard field too. The command is for scripts, and for a host
whose dashboard is not reachable yet.

```text
docpreview settings prefix          print the installation's hostname prefix
docpreview settings prefix P        publish previews as P-<name>
docpreview settings prefix -clear   back to bare preview names
```

A running daemon reads the prefix once at startup, so set it and then start the daemon.

## `webhook-only`

A reverse proxy that publishes `POST` on one or more paths, forwards those to the daemon, and answers `404` to
everything else.

```text
docpreview webhook-only [-zrok-name NAME] [-zrok-namespace NS] [-listen ADDR]
                        [-upstream URL] [-path PATH] [-log-level LEVEL]
```

```powershell
docpreview webhook-only -zrok-name docpreview
```

```text
level=INFO msg="webhook-only serving over zrok" url=https://docpreview.shares.zrok.io forwards="POST /webhook/github" to=http://127.0.0.1:8471
level=INFO msg="this is the webhook URL to configure" platform=github webhook=https://docpreview.shares.zrok.io/webhook/github
```

**Never tunnel the daemon itself.** zrok's proxy backend shares an *origin*, not a path, so
`zrok2 share public http://127.0.0.1:8471` publishes every route the daemon serves, `/api/secrets` included. That
surface's write gate asks whether the daemon's listeners are loopback. They are, so the gate says yes while the API
is on the internet. Against an unlocked vault, anyone with the share URL can store, delete and generate secrets.

No single check inside the daemon closes that. Behind a proxy, the connection comes from the local tunnel process, so
`RemoteAddr` is loopback too and `Host` is whatever the client sent. The credential API also refuses any request with
a forwarding header, and this proxy sets `X-Forwarded-For`. That is a second line of defence, and it fails if
something proxies and strips those headers. Tunnel this command instead, and the dashboard, the previews and the
credential API stay reachable only from the machine itself.

It does **not** verify the webhook signature. The daemon does that with the secret from the vault, and checking here
would need a second copy of the secret in a second process. This is a router. The daemon one hop in is the guard, and
a forged payload still gets `401`.

### Flags

| Flag | Default | |
|---|---|---|
| `-zrok-name` | | Serve over a named public zrok share and bind no local port. |
| `-zrok-namespace` | the environment's default | Namespace for `-zrok-name`. Required if the environment has none. |
| `-zrok-home` | `~/.zrok2` | The zrok environment directory. Pass `<data_dir>/zrok2` if the daemon uses its own. This process reads no config, so it cannot know. A share created from the wrong account reserves a name the previews cannot use. |
| `-listen` | `127.0.0.1:8481` | Where to accept tunnelled requests. Unused with `-zrok-name`. |
| `-upstream` | `http://127.0.0.1:8471` | The daemon to forward to. |
| `-path` | `/webhook/github` | A path to forward. Repeat it for more than one. |
| `-log-level` | `info` | |

A non-loopback `-upstream` is **refused**. This process publishes a daemon that is otherwise unreachable, and pointed
at a remote one it would be an open relay for that daemon's webhook endpoint.

Without `-zrok-name` it binds `-listen` and prints the `zrok2 share public` command to point at it, for a tunnel you
run yourself.

### With `-zrok-name`

**No local TCP port is bound at all**, so there is nothing on this machine for anything else to find. The URL is
stable, because the name is reserved.

The name must already exist: `zrok2 create name docpreview`. A reserved name counts against an account quota, so the
command never creates one for you.

A share from a previous run that still holds the name is **adopted**, so a restart creates no share. Shutdown leaves
the share on the controller and only closes the listener. If the old share cannot be adopted and a new one cannot be
created, the old share is deleted and created again. That is safe because the search is scoped to this zrok
environment and matches the leftmost DNS label of a share's frontend endpoints. The only share it can delete is this
account's own share of this name.

A new share goes through the [share gate](#the-zrok-share-gate), so a process stuck in a restart loop waits instead
of creating a share every few seconds.

This path uses the zrok SDK because the CLI cannot do it. `zrok2 share public` takes the target as its one positional
argument and its `-n` flag selects the namespace, so the command line has no way to claim a reserved name for a
public share.

### What the outside sees

| Request | |
|---|---|
| `POST` a forwarded path | Forwarded. `502` if the daemon is down, so GitHub retries the delivery. |
| `GET` a forwarded path | `405`, with a sentence saying there is nothing here to open in a browser. |
| Anything else, including `/` | A bare `404`. |

People paste a webhook URL into a browser first, and a browser sends `GET`, so the `405` tells them it is the right
URL with the wrong method. It reveals nothing new: a `POST` to the real path answers `401` and a `POST` anywhere else
answers `404`, so anyone probing can find the path anyway.

## `dashboard-only`

The same idea as `webhook-only`, for the other direction: it publishes the **read-only** dashboard and forwards
nothing else.

```text
docpreview dashboard-only [-zrok-name NAME] [-zrok-namespace NS] [-listen ADDR]
                          [-upstream URL] [-oauth google|github -oauth-domain D] [-log-level LEVEL]
```

```powershell
docpreview dashboard-only -zrok-name docpreview-dash
```

It is a separate command so the two can use different names and stop independently. You can stop showing someone
the dashboard without taking the webhook down.

### What it forwards

An allowlist, `GET` only, compiled in. There is no `-path`:

```text
/                                   the dashboard
/status                             the JSON the page renders
/events                             the live status stream
/pr  /pr/                           the local platform's pull request page
/logs/{preview}                     the build log index for one preview
/logs/{preview}/stream              a live build log
/logs/{preview}/download            the whole log
/logs/{preview}/download/{build}    one build's log
```

It is an allowlist so a route added to the daemon later stays private until someone adds it here.

`/secrets`, `/projects`, `/api/secrets`, `/api/projects`, `/api/cache` and `/api/admin` are not on the list, so they
answer `404` through this proxy. A second layer backs that up: this proxy sets `X-Forwarded-For`, and the daemon
refuses every write from a forwarded request. The dashboard's **Projects** and **Settings** links come from
`/api/admin`, so through this proxy that fetch fails and the links are not drawn.

:::danger Put a login in front of it

This publishes every open pull request that has a preview, across every repository the App is installed on: branch
names, commit SHAs, build durations, and the full build log of each. On a public repository all of that is public
already. On a private one it is not.

Set a [console password](#console), or pass `-oauth google -oauth-domain example.com` (or `-oauth github`) so zrok's
frontend asks for a sign-in before anything reaches the daemon.

:::

### Flags

| Flag | Default | |
|---|---|---|
| `-zrok-name` | | Serve over a named public zrok share and bind no local port. |
| `-zrok-namespace` | the environment's default | Namespace for `-zrok-name`. Required if the environment has none. |
| `-zrok-home` | `~/.zrok2` | The zrok environment directory. Pass `<data_dir>/zrok2` if the daemon uses its own. This process reads no config, so it cannot know. A share created from the wrong account reserves a name the previews cannot use. |
| `-oauth` | | Gate the share at the zrok frontend: `google` or `github`. Needs `-oauth-domain`. |
| `-oauth-domain` | | Comma-separated email domains allowed through `-oauth`, like `example.com`. |
| `-listen` | `127.0.0.1:8482` | Where to accept tunnelled requests. Unused with `-zrok-name`. A different port from `webhook-only`'s `8481`, so both can run at once. |
| `-upstream` | `http://127.0.0.1:8471` | The daemon to forward to. Non-loopback is refused, for the same reason as `webhook-only`. |
| `-log-level` | `info` | |

There is no `-config`. The name must already exist (`zrok2 create name docpreview-dash`), and a share from a previous
run that holds it is adopted, as in [`webhook-only`](#webhook-only).

## `webhook-check`

Sends a correctly signed `ping` to a webhook URL and reports whether it was accepted.

```text
docpreview webhook-check -url URL [-config FILE] [-timeout D]
```

```powershell
docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github
```

```text
POST https://docpreview.shares.zrok.io/webhook/github
  event     ping
  delivery  check-1f0c9a4b2e77d310
  status    202 Accepted in 412ms

PASS — a signed delivery is accepted end to end.
```

From outside you can check every part of the delivery path except whether a *signed* request is accepted. An
unsigned `curl` gets `401`, which proves the request reached the daemon and nothing more. Without this command you
would configure the App first and read GitHub's Recent Deliveries page to find a broken tunnel or a wrong secret.

A `ping` is enough. The signature is checked before the event type, so a signed ping that returns `2xx` has passed
the signature check. A fake `pull_request` would also queue a build for a repository that does not exist.

The webhook secret is read from the vault into this process and used for one HMAC. It is never printed, never passed
as an argument, and never lands in shell history. That is why this is a command and not a `curl` recipe with the
secret pasted in. The vault opens the way it does for every other CLI command, so if your daemon is unlocked only
from the dashboard, this command needs its own route to the [master key](./security.md#the-master-key).

| Flag | Default | |
|---|---|---|
| `-url` | required | The webhook URL to test, exactly as GitHub would be given it. |
| `-config` | `~/.docpreview/config.yml` | Which vault the secret is read from. |
| `-timeout` | `30s` | |

Each failure gets a diagnosis, because each has a different fix:

| | |
|---|---|
| unreachable | The tunnel is not running. |
| `401` | The request arrived, so the tunnel is fine. This vault's secret is not the one the receiving daemon uses: `-config` names a different vault, or the secret was rotated after that daemon read it. |
| `404` | The tunnel is up and the path is wrong, or `webhook-only` was given a different `-path`. |
| `501` | The daemon has no GitHub client. `github.app_id` is unset, or the vault was locked at startup and nothing has unlocked it since. |
| `502` | zrok holds the share and cannot reach the backend. `webhook-only` is not running, or is still attaching its listener, which takes a few seconds. |

## `doctor`

Checks configuration and credentials, then exits.

```powershell
docpreview doctor
```

```text
config:  C:\Users\you\.docpreview\config.yml
data:    C:\Users\you\.docpreview
exposer: zrok2
driver:  local
listen:  tcp 127.0.0.1:8471
vault:   C:\Users\you\.docpreview\vault.age (2 secrets)
key:     file:C:\ProgramData\docpreview\master.key
scm:     github (app 1234567)

all checks passed
```

The `key` line tells you whether a restart needs a person. With no `vault.key_source` it says so. With
`github.app_id` set and the vault locked, the `scm` line says `not wired up: the vault is locked` instead of claiming
no source control is configured, because the two have different fixes.

Every listener is listed, and each `ziti` one is **bound and released** to prove it works:

```text
listen:  tcp 127.0.0.1:8471
         ziti service docpreview-admin (identity …\docpreview-host.json)
bind:    ziti service docpreview-admin (identity …\docpreview-host.json) ok
```

That catches an identity that will not authenticate and a service with no Bind policy naming it. Neither shows in the
config file, and both look like a dashboard you cannot reach. TCP listeners are *not* bound: when the daemon is
running, the port is supposed to be in use.

It makes real calls. It asks GitHub who the App is and asks the zrok controller whether the environment is valid.
Both can fail while the local state looks fine, and it is better to find out before a reviewer is waiting on a
comment.

With nothing configured beyond the `local` exposer and the `local` build driver, it still passes, and says what is
missing:

```text
exposer: local
driver:  local
key:     none — the daemon starts locked and is unlocked from the dashboard
scm:     none — set local.enabled or github.app_id to receive webhooks
```

There is no vault line there because nothing needed the vault. It opens only when needed, so a setup with the `local`
exposer and no source control never asks for a master key.

## `shares list`

What the exposer's account holds, compared with what the database claims. It answers two questions with opposite
fixes: is the account paying for something nothing uses, and is something advertising a URL that no longer resolves.

```powershell
docpreview shares list -config .docpreview\config.yml
```

```text
STATE    PUBLICATION                           PULL REQUEST                                     SHARE         URL
missing  81379294374a/20260730-184200-cf9f37d  bitbucket:netfoundry/customer-connect-docs#19    -             https://…-cf9f37d.shares.zrok.io/
ok       10e1d73c7eea                          github:openziti-test-kitchen/docpreview@main     g83f5pgt410k  https://docpreview-main.shares.zrok.io/
never    58a4a5f461f7                          github:openziti-test-kitchen/docpreview#6        -             -

23 shares held: 23 matched the database, 0 orphaned. 1 recorded publication is no longer on the account;
2 previews have never published.
```

| State | |
|---|---|
| `ok` | The account holds it and the database claims it. |
| `orphan` | The account holds it and the database does not. It costs a share and a reserved name against quota, and serves a URL nothing links to. The daemon's reaper deletes these at its next sweep. |
| `missing` | The database claims it and the account does not hold it. A comment or the dashboard offers a URL that now 404s. Rebuild the preview. |
| `never` | A recorded preview that has not published yet. Counted apart so a queued build is not reported as a problem. |

A `@branch` in the pull request column is a [branch preview](./projects.md#the-default-branch-is-always-previewed),
not a pull request.

**It deletes nothing**, so it is safe to run against a live daemon. `-json` gives the same data for a script.

:::note Two things it cannot see

A **leaked reserved name** is what the quota counts, and it is invisible once its share is gone, because the command
lists shares. Only the `zrok2` exposer can answer at all. The others say so and point at `doctor`.

:::

## `zrok`

Signs up for zrok, enrols this host, and picks which of the two zrok environments the daemon uses. These are the same
operations as the panel on `/secrets`, for a host with no browser.

```powershell
docpreview zrok status
docpreview zrok use system|project
docpreview zrok invite <email> [-api-endpoint URL] [-invite-token T]
docpreview zrok register <link-or-token> [-api-endpoint URL] [-description D] [-no-enable]
docpreview zrok enable [-token-stdin] [-description D]
docpreview zrok disable -yes
```

### `zrok status`

Both environment directories, what is enrolled in each, and which one this installation uses.

```text
in use: system
  this installation  D:\docpreview\.docpreview\zrok2
    nothing here yet
* this machine       C:\Users\you\.zrok2
    enabled against https://api-v2.zrok.io/, default namespace public
```

### `zrok use system|project`

Records which environment the daemon adopts. **Takes effect at the next restart**, because zrok reads its root
directory once per process.

| | |
|---|---|
| `system` | `~/.zrok2`, what the `zrok2` CLI uses |
| `project` | `<data_dir>/zrok2`, the daemon's own, beside the vault |

With nothing recorded, a daemon adopts whichever one is enabled and writes that down. With **both** enabled it uses
`project`, warns, and records nothing, so the dashboard keeps asking. See
[Publish previews with zrok v2](../guides/zrok2.md) for why it does not guess.

### `zrok invite <email>`

Asks the zrok service to email a registration link. Refused if an environment is already enrolled in either
directory.

| Flag | |
|---|---|
| `-api-endpoint` | A self-hosted zrok. Default `https://api-v2.zrok.io`. |
| `-invite-token` | For a zrok service that is itself invitation-only (`tokenStrategy: store`). |

### `zrok register <link-or-token>`

Creates the account and enrols this host. Takes the whole emailed link or only the token at the end of it.

The **zrok account password** is read from stdin, never from an argument. It is not stored here, and you need it to
reset the account later, so keep it somewhere safe.

The account token goes into the vault as `zrok.account_token`. If the vault is locked, the enrolment still happens
and the command tells you how to store the token afterwards. `-no-enable` stops after creating the account.

### `zrok enable`

Enrols this host with an account token you already have. Reads it from the vault, or from stdin with `-token-stdin`,
which also stores it.

### `zrok disable -yes`

Removes this host's environment from the account. `-yes` is required because every share published through it is
deleted, and preview URLs stop answering until the daemon publishes them again. The reserved names belong to the
account and survive, so the URLs come back unchanged.

## `sim`

Bare git repositories that behave like pull requests, so the whole loop runs with no GitHub account, no network, and
no tunnel. See [Local platform](../local-platform.md) for how it works.

```bash
docpreview sim init <name> [-base main] [-seed ./path/to/a/repo]
docpreview sim list
```

### `sim init <name>`

Creates `<local.repos_dir>/<name>.git`, a bare repository with a generated `post-receive` hook.

| Flag | |
|---|---|
| `-base` | The branch treated as the base. Pushing it is not a pull request. Default `local.default_base`. |
| `-seed` | Push the current branch of an existing repository in as the base, so there is something to build. |
| `-config` | Config file. |

It needs a TCP listener, because the hook is a `curl` to an address on this machine. An overlay-only configuration is
refused with a message saying so, instead of writing a hook that fails on every push.

### `sim list`

Every simulated repository, with its branch count.

There is no `sim rm`. Delete the `.git` directory under `local.repos_dir`, and its previews expire on `preview.ttl`.
To tear them down right away, delete the branch and push.

## `vault`

Credentials, encrypted at rest in a single file with [age](https://age-encryption.org/). See
[age, and why the vault uses it](../background/age.md) if that name is new.

The master key comes from `vault.key_source` in the config (a file, or a command that prints it), then
`$DOCPREVIEW_MASTER_KEY`, then a prompt on a terminal. With none of the three, the daemon starts locked and you unlock
it from the dashboard. See [security](./security.md#the-master-key) for which to pick.

The vault is `<data_dir>/vault.age`, by default `~/.docpreview/vault.age`. It holds a JSON map of names to values,
encrypted as one blob, so the file reveals neither the values nor which secrets exist.

### `vault keygen`

```text
docpreview vault keygen [-out FILE] [-shell SHELL] [-quiet]
```

Generates a new [age X25519 identity](../background/age.md#how-it-works). There are three output shapes, and all three
keep the key out of your shell history. `-out` and `-shell` together are refused.

#### Write it to a file

```powershell
docpreview vault keygen -out C:\ProgramData\docpreview\master.key
```

```bash
docpreview vault keygen -out /etc/docpreview/master.key
```

The key is written at mode 0600 and never printed. This pairs with `vault.key_source`, and it is the shape that lets
the daemon come back from a reboot with nobody there:

```yaml
vault:
  key_source: "file:/etc/docpreview/master.key"
```

An existing file is **not** overwritten. It may be the only key to a vault full of credentials. Move it aside first if
you mean to replace it.

The daemon enforces two placement rules when it starts: the file may not live inside `data_dir`, and on Unix it may
not be readable by group or other.

Prefer a secret manager where you have one. The key then exists in the daemon process and nowhere else on the
machine:

```yaml
vault:
  key_source: "exec:op read op://ops/docpreview/master-key"
```

#### Set it directly

```powershell
docpreview vault keygen -shell | Invoke-Expression
```

```bash
eval "$(docpreview vault keygen -shell)"
```

```fish
docpreview vault keygen -shell | source
```

With `-shell`, stdout is one line, an assignment in that shell's syntax, so you can evaluate it straight into the
session. Everything else goes to stderr, where you can read it and the pipe does not pick it up.

This keeps the key out of your shell history. Pasting `$env:DOCPREVIEW_MASTER_KEY = 'AGE-SECRET-KEY-1...'` records
the key. Piping records only the pipeline.

It still sets an environment variable, the least preferred of the three sources. Every process under the same user
can read it, and it shows up in service definitions, process listings and crash dumps. That is fine for a shell
session you are about to close. For a daemon, use `-out` or `exec:`.

| `-shell` value | Emits |
|---|---|
| omitted or `auto` | detected from the process tree |
| `powershell`, `pwsh` | `$env:NAME = 'value'` |
| `sh`, `bash`, `zsh` | `export NAME='value'` |
| `fish` | `set -gx NAME 'value'` |
| `cmd` | `set NAME=value` |

Detection reads the **process tree**, not environment variables. PowerShell exports `PSModulePath` and Git Bash sets
`SHELL`, and each inherits the other's variable when nested. A guess from the environment would emit `export` at a
PowerShell prompt. Pass `-shell` yourself if a wrapper confuses the walk.

`-quiet` drops the stderr commentary, for scripts.

#### See the key

```powershell
docpreview vault keygen
```

Bare, the key goes to stdout and the guidance to stderr, so it pipes cleanly into a password manager. Save it there
as well. The `-shell` form sets the variable for one session only, and losing the key means losing the vault.

#### Passphrases

A passphrase works in place of a generated key. Any `$DOCPREVIEW_MASTER_KEY` that does not start with
`AGE-SECRET-KEY-1` is treated as a passphrase and stretched with scrypt.

### `vault rekey`

```powershell
docpreview vault rekey -generate -yes
```

Re-encrypts the vault under a new master key and keeps every secret. Use it when the key is weaker than it should be,
such as a short passphrase typed during setup, or when someone may have seen the key.

```text
docpreview vault rekey [-config FILE] [-generate] -yes
```

| Flag | |
|---|---|
| `-generate` | Mint the new key. Without it, the new key is read from stdin, so you can reuse an existing one. |
| `-yes` | Required. Without it nothing happens and the command says why. |

:::danger Stop the daemon first

A running daemon holds the old key in memory. Its next write to the vault re-encrypts under the old key and undoes
the rekey without a word. Nothing here can detect that, which is why `-yes` is required.

:::

The order of steps keeps it recoverable:

1. Open the vault under the **current** key. If that fails, nothing is written. A rekey that could not read the vault
   would otherwise replace it with an empty one.
2. Copy `vault.age` to `vault.age.bak-<timestamp>`, which the old key still opens.
3. Re-encrypt in place, atomically. A crash here leaves the vault readable by the old key.
4. Install the new key at `vault.key_source`, **last**. Between steps 3 and 4 the key and the vault disagree, and the
   fix is to put the old key back, which works only while the old key still exists.

```text
Re-encrypted 12 secrets under a new master key.

  key    /etc/docpreview/master.key
  vault  /var/lib/docpreview/vault.age
  backup /var/lib/docpreview/vault.age.bak-20260814-124819 (opens with the previous key)
```

Start the daemon and confirm it unlocks before you delete the backup.

An `exec:` key source cannot be written to, because the key lives in a secret manager. The command prints the new key
once, on stdout, and tells you to store it. Until you do, the daemon starts locked.

:::note This changes the lock, not the credentials

Anyone who read a secret under the old key still knows it. A rekey fixes a weak key. A leaked credential has to be
rotated at the platform that issued it.

:::

### `vault set <key>`

From a file:

```powershell
docpreview vault set github.private_key -file .\downloaded-key.pem
```

From stdin:

```powershell
"my-webhook-secret" | docpreview vault set github.webhook_secret
```

```bash
echo -n "my-webhook-secret" | docpreview vault set github.webhook_secret
```

Trailing newlines are stripped, since a shell adds them. A PEM key keeps its internal newlines. Empty values are
refused.

### `vault list`

```powershell
docpreview vault list
```

```text
github.private_key
github.webhook_secret
```

Names only, sorted. No command prints a value.

### `vault delete <key>`

```powershell
docpreview vault delete frontdoor.api_token
```

### Known keys

| Key | Used by |
|---|---|
| `github.private_key` | GitHub App: the `.pem` GitHub generated |
| `github.webhook_secret` | GitHub App: webhook HMAC |
| `bitbucket.email` | Bitbucket: Atlassian account email |
| `bitbucket.api_token` | Bitbucket: API token (app passwords no longer work) |
| `bitbucket.webhook_secret` | Bitbucket: webhook HMAC |
| `frontdoor.api_token` | Frontdoor exposer |
| `zrok.account_token` | zrok: the account token `zrok register` and `zrok enable` store |

## Exit codes

| Code | |
|---|---|
| `0` | Success. Also what `-h` on a subcommand exits with, after printing that subcommand's flags. |
| `1` | The command ran and failed, with the reason on stderr. |
| `2` | The command line itself was wrong, such as an unknown flag or a flag a subcommand does not have. Go's flag package prints the usage and exits before the command runs. |

`unknown command` exits `1`, not `2`, because the binary parsed that far and answered.
