---
id: cli
title: CLI reference
sidebar_position: 3
---

# CLI reference

```text
docpreview init      [-config FILE] [-force] [-advanced] [-yes]
docpreview configure ziti [-controller URL] [-username U] [-password P] …
docpreview preview   [-build] [-base-url PATH] [-name LABEL] <dir>
docpreview serve     [-config FILE] [-log-level LEVEL]
docpreview doctor    [-config FILE]
docpreview shares    list [-config FILE] [-json] [-log-level LEVEL]
docpreview sim       <subcommand>
docpreview webhook-only    [-zrok-name NAME] [-listen ADDR] [-upstream URL] [-path PATH]
docpreview dashboard-only  [-zrok-name NAME] [-listen ADDR] [-upstream URL]
docpreview webhook-check -url URL [-config FILE] [-timeout D]
docpreview vault     <subcommand>
```

:::note `-config` is not universal

`webhook-only` and `dashboard-only` take **`-zrok-name`, not `-config`**. Neither reads the config file or opens
the vault — a reverse proxy that forwarded one path needs neither, and holding a second copy of the webhook secret
in a second process is the thing `webhook-only` exists to avoid. Passing `-config` to either one exits `2` with a
usage dump, which reads as a broken command rather than as the design it is.

`vault keygen` has no `-config` either: it mints a key and writes no vault.

:::

Source control is **optional**. `github.app_id: 0` means "not wired up yet", which is the state everyone is in
for their first ten minutes and the permanent state of anyone using Bitbucket. `init`, `doctor`, and `preview`
all work in it, and `serve` starts in it and says so — webhooks are the one thing that cannot happen without it.

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

**No GitHub App, no webhook, no credentials** beyond whatever the exposer itself needs — nothing at all for the
`local` exposer. This exercises the exposer, the build, the baseUrl verification, and the static server: every
part of the pipeline except the webhook and the comment.

It exists because creating a GitHub App is a ten-minute detour through a web form, and nobody should have to
take it on the strength of a README. Try the thing, then decide.

It is also the fastest way to diagnose an unstyled preview, because it runs the same
[baseUrl check](../troubleshooting.md#the-baseurl-trap) the daemon does and prints the same error, without
needing a pull request to trigger it.

| Flag | |
|---|---|
| `-build` | Run the site build first. Without it, the directory is served as-is. |
| `-base-url PATH` | Serve under a prefix. Default: from `.docpreview.yml`, else `/`. |
| `-name LABEL` | Public hostname label. Default: the directory name, sanitized. |
| `-config FILE` | |
| `-log-level LEVEL` | |

The preview ID is derived from the label, so re-running against the same directory replaces the previous share
rather than accumulating orphans.

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

A question earns its place only if a reasonable person would answer differently from the default, *and* they
can answer it now. Only the exposer clears that bar.

The GitHub App ID is deliberately **not** asked. Source control is the last thing you wire up, not the first:
at `init` you have not created the App yet, and on Bitbucket you never will. It defaults to `0`, everything
except `serve` works in that state, and the closing checklist reminds you where to come back to. Set it with
`-app-id` if you already have one.

Nothing is hidden. Whatever you did not answer is printed:

```text
Settings
--------
  exposer      zrok2
  preview URL  {{.Name}}.<your zrok namespace>
  visibility   anyone with the link
  github app   not set yet
  listen       127.0.0.1:8471
  data dir     /home/you/.docpreview
  builds       local driver, 15m0s timeout, 2 at a time
  preview ttl  72h0m0s
```

Then two checklists, because they are two different commitments:

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

Both are computed from your answers — choose the `local` exposer and it does not tell you to go enable zrok.

### Flags

| Flag | |
|---|---|
| `-config FILE` | Where to write. Default `~/.docpreview/config.yml`. |
| `-force` | Overwrite without asking. |
| `-advanced` | Ask about every setting, not only the two. |
| `-yes` | Ask nothing. Take every default. |
| `-exposer KIND` | Set the exposer and skip that question. |
| `-app-id N` | Set the App ID and skip that question. |

Fully unattended, for a provisioning script:

```bash
docpreview init -yes -exposer zrok2 -app-id 1234567
```

### With `-advanced`

Every setting, each validated at the prompt rather than at the end:

```text
Listen address [127.0.0.1:8471]: localhost
  not a host:port address (try 127.0.0.1:8471)
Listen address [127.0.0.1:8471]:
```

The name template is validated by *rendering* it, so you see the consequence before you commit to it:

```text
Name template [{{.Name}}]: {{.Repo.Name}}-{{.Name}}
  -> a branch named feature/example would be published as "docs-feature-example"
```

### The output

**Commented YAML**, not a marshalled struct — the comments are the part that survives being copied to another
machine six months later. It is read back through the real config loader before `init` reports success, so it
cannot hand you a file that `serve` will refuse.

`init` writes only the config. It never touches the vault, so it never handles a secret.

## `configure ziti`

Provisions an OpenZiti network and points docpreview at it. The whole path from nothing to a working private
preview is four commands:

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
4. a **Bind** policy per service, so docpreview may host them
5. a **Dial** policy per service, keyed on the `docpreview-reader` role attribute — so adding a reviewer later
   is one attribute on a new identity rather than an edit to a live policy
6. a service-edge-router policy per service
7. `docpreview-host`, docpreview's own identity, **enrolled** into `<data_dir>/ziti/docpreview-host.json`
8. `reviewer-alice`, a sample reviewer identity, leaving a `.jwt` beside it

Then it writes the config file, so `docpreview serve` works immediately afterwards.

**Re-running is safe.** Every object is checked for before it is created, and a second run reports everything
as already present and changes nothing. People run a setup command more than once — after reading the output,
after changing one flag, after a failure halfway through — and a command that errors on the second run teaches
them to tear the network down before every attempt.

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

The reviewer token is the part nobody guesses. It is not a URL and not a password — the only thing to do with
it is import the file into a tunneler — [Ziti Desktop Edge](https://netfoundry.io/docs/openziti/how-to-guides/tunnelers/windows/)
on Windows, `ziti-edge-tunnel add --jwt "$(cat reviewer-alice.jwt)"` elsewhere. Then
`http://<branch>.docpreview.ziti/` resolves, and resolves nowhere else.

### Flags

| Flag | Default | |
|---|---|---|
| `-controller` | `https://localhost:1280` | Edge management API. What `ziti edge quickstart` advertises. |
| `-username` | `admin` | |
| `-password` | `admin` | |
| `-domain` | `docpreview.ziti` | DNS suffix previews appear under. Goes into the intercept config *and* the docpreview config, so the two cannot disagree. |
| `-service` | `docpreview-svc` | The wildcard preview service. |
| `-admin-service` | `docpreview-admin` | Service for the dashboard and webhook endpoint. Empty leaves the ingress on TCP only. |
| `-host-identity` | `docpreview-host` | docpreview's own identity. |
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

Removing the TCP address would lock you out of the thing you set up until your own tunneler is enrolled
and running, which is the wrong order in which to discover a mistake. Delete the `tcp` entry once the overlay
is proven.

### Two identities that cannot be recreated the same way

An enrollment token is one-time, which makes "already exists" mean different things for the two identities.

**The hosting identity** is recreated if it exists on the controller but its file is missing beside it — once
the token is spent, that file is the only proof of the identity, and without it docpreview can never
authenticate as it again. This is the one destructive act in the command, and it is confined to docpreview's
own identity.

**A reviewer identity** is never touched. No local `.jwt` is the normal state after somebody imported the
token into their tunneler, and deleting the identity would revoke a reviewer who is working fine. If the token
was already consumed, the command says so and suggests a different `-reviewer` name.

## `serve`

Runs the daemon: webhook ingress, build workers, and the reaper.

With no source control configured no webhook can ever arrive, which makes for a daemon that looks healthy and is
inert. It starts anyway. Configuring GitHub means storing a private key and a webhook secret, and
[`/secrets`](#endpoints) is where you do that — so refusing to boot until it is done would leave a terminal as the
only route back. Instead it starts, and says plainly on the way up that nothing can arrive yet:

```text
level=WARN msg="no source control is configured, so no webhooks can arrive"
  fix="add github.app_id, or set local.enabled for the git simulator" config=/home/you/.docpreview/config.yml
```

A locked vault is treated the same way. With `github.app_id` set and no master key available there is no GitHub
client, because building one reads the App private key and the webhook secret out of the vault — so `serve`
starts, `/webhook/github` answers `501`, and the client is built the moment the vault is unlocked from the
dashboard. Rotating either credential rebuilds it, which drops the cached installation token: that token was
minted by the key being replaced.

```powershell
docpreview serve
docpreview serve -config ./dev.yml -log-level debug
```

| Flag | Default | |
|---|---|---|
| `-config` | `~/.docpreview/config.yml` | Also `$DOCPREVIEW_CONFIG`. |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error`. |

Startup validates every component **before binding the port**. Discovering that the zrok environment is not
enabled after the first webhook arrives means a comment that never appears and no obvious place to look.

Every configured [listener](./configuration.md#listeners) is bound, or none of them is. One HTTP server serves
them all, and a failure on any one is fatal — the survivors would keep the process looking healthy while the
address people are actually pointed at has gone away.

```text
level=INFO msg=listening listeners="tcp 127.0.0.1:8471, ziti service docpreview-admin (identity …)" exposer=ziti
```

Then it reconciles: reaps every remote share it owns (nothing was serving them) and republishes each recorded
preview — and each retained build — from the artifacts already on disk. No re-cloning and no `npm install`.

:::warning Every preview URL 404s until that finishes

Reap has to come first, or it deletes what it restored. So there is a window with the shares gone and not yet
recreated, and under zrok each publication is a round trip to the controller at roughly **14 seconds**, run
serially. Three previews is about a minute. Thirty open pull requests is about seven.

Inside that window `/status` answers `200` with an empty event list, because the activity feed is re-hydrated from
the database after recovery finishes. It looks exactly like data loss. Wait for:

```text
level=INFO msg=recovered previews_restored=3 build_shares_restored=11 jobs_pending=0
```

See [troubleshooting](../troubleshooting.md#every-preview-404s-for-the-first-minute-after-a-restart).

:::

Because "every share it owns" is identified by this host's zrok environment plus docpreview's target tag, **two
daemons sharing one exposer account delete each other's live previews** on every restart. Give a second instance its
own account.

`SIGINT` or `SIGTERM` drains in-flight work and withdraws every publication.

### The dashboard's three pages

One embedded HTML document, served at three paths and switched on the path. Splitting it would mean two copies of the
styles and the fetch helper to keep in step.

| Path | |
|---|---|
| `/` | Previews, the activity feed, and the build log viewer. Live over an `EventSource`. |
| `/secrets` | The credential surface, and nothing else. No stream — nothing on it changes on its own, and an open stream per idle tab is a connection held for nothing. |
| `/projects` | [Projects](./projects.md) and their environment variables. No stream either. |

**The links to the last two appear only for a local request.** The page asks `/api/admin`, which runs the same
locality check the write endpoints run, and draws **Projects**, **Settings** and **Clear caches** only on an outright
yes. The server decides, not the page: a `Host`-header test in the browser would be worthless, because `Host` is
whatever the client typed. The pages themselves are still reachable if you type the path — read-only, with a banner
saying why.

:::note A tab left open across a restart is running the code it loaded

There is no cache busting on that document, so a rebuilt daemon does not reach an open tab. Three false bug reports
came out of one afternoon of this. The page now notices the daemon instance changed and offers a reload rather than
reloading under you. **If something on the page contradicts the code, reload before diagnosing.**

:::

### Endpoints

| Method | Path | |
|---|---|---|
| `POST` | `/webhook/github` | HMAC-verified. Returns `202` before doing the work. |
| `POST` | `/webhook/local` | The local platform. No signature — see [Local platform](../local-platform.md). |
| `POST` | `/webhook/bitbucket` | HMAC-verified, with the signature in `X-Hub-Signature` — which is SHA-256 here, despite being the name GitHub uses for its legacy SHA-1. Returns `501` only when no Bitbucket credential is stored. |
| `GET` | `/` | The dashboard. |
| `GET` | `/secrets` | Credential management. Its own page rather than a panel on the dashboard: a URL can be bookmarked and named in a runbook, and a distinct path is something a proxy or a later authentication layer can gate. Registered only when a credential surface is wired, so a daemon without one answers `404` rather than serving an empty page. |
| `GET` | `/projects` | [Projects](./projects.md) and their environment variables. The third page, same embedded document, switched on the path. |
| `GET` | `/api/admin` | Which of the two admin pages this request would be allowed to write to. The dashboard asks before drawing the **Projects**, **Settings** and **Clear caches** controls, so the server decides and the page never offers an action that would `403`. |
| `GET` | `/api/secrets` | What that page reads: names, whether the vault is unlocked, and whether this daemon may be written to at all. Never a value. |
| `PUT` `DELETE` `POST` | `/api/secrets/…` | Store, delete, unlock, generate. Refused unless every listener is loopback *and* the request arrived from this machine carrying no forwarding header. Loopback is not local: a tunnel makes the first true while the caller is on the internet, which is what [`webhook-only`](#webhook-only) exists for. |
| `GET` `PUT` `DELETE` | `/api/projects/…` | Project rows and their environment variables. Same two gates as `/api/secrets`, for a stronger reason: a project row decides what command runs on the build host. |
| `POST` | `/api/projects/…/branch` | Build a project's default branch, or the branch named in the body. The permanent preview described in [Projects](./projects.md). Same gates. |
| `POST` | `/api/projects/…/link` | Build one pull request by number, and stop skipping it if it was unlinked. Same gates. |
| `POST` | `/api/builds/{preview}/unlink` | Remove a preview and record that its pull request must not be built again. The ignore is written before the teardown, so a failure partway leaves the pull request unbuilt rather than rebuilding itself on the next push. Same gates. |
| `POST` | `/api/builds/{preview}/rebuild` | Build again: the recorded commit for a pull request, the branch's current tip for a branch preview. Same gates. |
| `POST` | `/api/builds/{preview}/cancel` | Abandon the build running for a preview. Same gates. |
| `DELETE` | `/api/cache` | Empty every preview's package cache. Same gates. |
| `DELETE` | `/api/cache/{preview}` | Empty one preview's. Keyed on a preview rather than a project, but served by the projects admin so there is one gate rather than two. |
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
| `GET` | `/status` | JSON: exposer, queue depth, live previews. |

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

`/status` carries no secrets, but it does enumerate every open documentation pull request. Think about that
before sharing the ingress publicly alongside the webhook endpoint. Once a console password is set it is behind
the login with the rest of the dashboard, including from loopback.

`/readyz` is the endpoint to poll instead, from a script or a supervisor. It answers without a login, and it says
how busy the daemon is without saying what it is busy with — counts and a stage name, never a repository, branch or
URL:

```json
{
  "starting": false,
  "pending": 0,
  "running": 0,
  "ready": 8,
  "instance": "20260731-192655.448"
}
```

While recovery is running it also carries `startup` with `stage`, `note`, `done` and `total`, which is what the
restart script prints as it waits. `instance` is the process start stamp, so a caller can tell the daemon it just
restarted from one that was already running.

## `webhook-only`

A reverse proxy that publishes exactly one method and one path, forwards it to the daemon, and answers `404` to
everything else.

```text
docpreview webhook-only [-zrok-name NAME] [-zrok-namespace NS] [-listen ADDR]
                        [-upstream URL] [-path PATH] [-log-level LEVEL]
```

```powershell
docpreview webhook-only -zrok-name docpreview
```

```text
level=INFO msg="webhook-only serving over zrok" url=https://docpreview.shares.zrok.io forwards="POST /webhook/github"
level=INFO msg="this is the webhook URL to give GitHub" webhook=https://docpreview.shares.zrok.io/webhook/github
```

**Never tunnel the daemon itself.** zrok's proxy backend shares an *origin*, not a path, so
`zrok2 share public http://127.0.0.1:8471` publishes every route the daemon serves — `/api/secrets` included.
That surface's write gate asks whether the daemon's listeners are loopback. They are. So the gate says yes while
the API is on the internet, and against an unlocked vault store, delete and generate all succeed for anyone
holding the share URL.

No single check inside the daemon closes that. Under a proxy it sees the connection from the local tunnel
process, so `RemoteAddr` is loopback too and `Host` is whatever the client sent. The distinction does not exist
at that layer. The credential API does additionally refuse any request carrying a forwarding header, and this
proxy sets `X-Forwarded-For` — but that is a second line, and it assumes nothing ever proxies while stripping
those headers. Tunnel this instead and the dashboard, the previews and the credential API are reachable only from
the machine itself, which is what they were designed for.

It deliberately does **not** verify the webhook signature. That is the daemon's job, it already does it with the
secret from the vault, and a copy here would mean a second copy of that secret in a second process. This is a
router, not a guard — the guard is one hop further in, and a forged payload gets the same `401` it always did.

### Flags

| Flag | Default | |
|---|---|---|
| `-zrok-name` | | Serve over a named public zrok share and bind no local port. |
| `-zrok-namespace` | the environment's default | Namespace for `-zrok-name`. Required if the environment has none. |
| `-zrok-home` | `~/.zrok2` | The zrok environment directory. Pass `<data_dir>/zrok2` if the daemon uses its own — this process reads no config and cannot know. A share created from the wrong account reserves a name the previews cannot use. |
| `-listen` | `127.0.0.1:8481` | Where to accept tunnelled requests. Unused with `-zrok-name`. |
| `-upstream` | `http://127.0.0.1:8471` | The daemon to forward to. |
| `-path` | `/webhook/github` | The one path forwarded. |
| `-log-level` | `info` | |

A non-loopback `-upstream` is **refused**. This process exists to publish a daemon that is otherwise unreachable.
Pointing it at a remote one would make it an open relay for that daemon's webhook endpoint.

Without `-zrok-name` it binds `-listen` and prints the `zrok2 share public` command to point at it, for a tunnel
you run yourself.

### With `-zrok-name`

**No local TCP port is bound at all** — there is nothing on this machine for anything else to find — and the URL
is stable, because the name is reserved rather than minted per share.

The name must already exist: `zrok2 create name docpreview`. Reserving is an account-level act with a quota
behind it, and a process that created names silently would leak one per typo.

A name still held by a share from a previous run is **reclaimed** and the share recreated. This is the ordinary
case rather than the exceptional one: the share is deleted on graceful shutdown, and a process serving a tunnel
is normally ended by a kill, which runs no deferred cleanup — without this, every restart would need a manual
`zrok2 delete share`. Reclaiming is safe precisely because the name is reserved: the search is scoped to this
zrok environment and matched on the leftmost DNS label of a share's own frontend endpoints, so the only thing it
can delete is this account's own abandoned share of this name.

This path uses the zrok SDK rather than the CLI because the CLI cannot do it. `zrok2 share public` takes the
target as its one positional and its `-n` flag selects the namespace, so there is no way to claim a reserved name
for a public share from the command line.

### What the outside sees

| Request | |
|---|---|
| `POST` the forwarded path | Forwarded. `502` if the daemon is down, so GitHub retries the delivery. |
| `GET` the forwarded path | `405`, with a sentence saying there is nothing here to open in a browser. |
| Anything else, including `/` | A bare `404`. |

The `405` earns its place because the first thing anyone does with a webhook URL is paste it into a browser, and
a browser sends `GET` — so the useful answer is "right URL, wrong method". It leaks nothing already hidden: a
`POST` to the real path answers `401` and a `POST` anywhere else answers `404`, so the path is distinguishable to
anyone probing properly regardless.

## `dashboard-only`

The same shape as `webhook-only`, for the other direction: it publishes the **read-only** dashboard and forwards
nothing else.

```text
docpreview dashboard-only [-zrok-name NAME] [-zrok-namespace NS] [-listen ADDR]
                          [-upstream URL] [-log-level LEVEL]
```

```powershell
docpreview dashboard-only -zrok-name docpreview-dash
```

Two commands rather than one with a mode flag, because they are published at different names and stopped
independently. Sharing one process would mean taking the webhook down to stop showing somebody the dashboard.

### What it forwards

An allowlist, `GET` only, compiled in — there is no `-path`:

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

An allowlist rather than a denylist, because the failure mode of forgetting to add a route to a denylist is that
the new route is public.

`/secrets`, `/projects`, `/api/secrets`, `/api/projects`, `/api/cache` and `/api/admin` are absent and therefore
`404` through this proxy. That is the first of two layers: this proxy sets `X-Forwarded-For`, and the daemon refuses
every write from a forwarded request regardless. The dashboard's own **Projects** and **Settings** links come from
`/api/admin`, so through this proxy the fetch 404s and the links are not drawn.

:::danger There is no authentication here

What this publishes is every open documentation pull request across every repository the App is installed on:
branch names, commit SHAs, build durations, and the full build log of each. On a public repository all of that is
public already. On a private one it is not. Put `--basic-auth` on the zrok share — which works in a browser, in a
way it cannot for a webhook.

:::

### Flags

| Flag | Default | |
|---|---|---|
| `-zrok-name` | | Serve over a named public zrok share and bind no local port. |
| `-zrok-namespace` | the environment's default | Namespace for `-zrok-name`. Required if the environment has none. |
| `-zrok-home` | `~/.zrok2` | The zrok environment directory. Pass `<data_dir>/zrok2` if the daemon uses its own — this process reads no config and cannot know. A share created from the wrong account reserves a name the previews cannot use. |
| `-listen` | `127.0.0.1:8482` | Where to accept tunnelled requests. Unused with `-zrok-name`. Note the port differs from `webhook-only`'s `8481`, so both can run at once. |
| `-upstream` | `http://127.0.0.1:8471` | The daemon to forward to. Non-loopback is refused, for the same reason as `webhook-only`. |
| `-log-level` | `info` | |

There is no `-config`. The name must already exist — `zrok2 create name docpreview-dash` — and an abandoned share
still holding it is reclaimed and recreated, exactly as in [`webhook-only`](#webhook-only).

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

Every part of the delivery path can be checked from outside except the part that matters: whether a *signed*
request is accepted. An unsigned `curl` gets `401`, which proves the request reached the daemon and proves
nothing about whether it would ever pass. The alternative is to configure the App first and read GitHub's Recent
Deliveries page — finding a broken tunnel or a mismatched secret from the far side, after the fact.

A `ping` is the right probe. Signature verification happens before the event type is examined, so a signed ping
returning `2xx` has already proven the signature passed. A fabricated `pull_request` would additionally queue a
build for a repository that does not exist, leaving junk behind to prove something this already proves.

The webhook secret is read from the vault into this process and used to compute one HMAC. It is never printed,
never passed as an argument, and never reaches a shell history — which is why this is a command rather than a
documented `curl` with the secret pasted into it. The vault is opened the way every other CLI command opens it,
so on a daemon unlocked only from the dashboard this needs its own route to the
[master key](./security.md#the-master-key).

| Flag | Default | |
|---|---|---|
| `-url` | required | The webhook URL to test, exactly as GitHub would be given it. |
| `-config` | `~/.docpreview/config.yml` | Which vault the secret is read from. |
| `-timeout` | `30s` | |

Each failure is diagnosed rather than reported as a number, because the fix differs every time:

| | |
|---|---|
| unreachable | The tunnel is not running. |
| `401` | The request arrived, so the tunnel is fine. This vault's secret is not the one the receiving daemon is using: `-config` names a different vault, or the secret was rotated after that daemon read it. |
| `404` | The tunnel is up and the path is wrong, or `webhook-only` was given a different `-path`. |
| `501` | The daemon has no GitHub client — `github.app_id` is unset, or the vault was locked at startup and nothing has unlocked it since. |
| `502` | zrok holds the share and cannot reach the backend: `webhook-only` is not running, or is still attaching its listener, which takes a few seconds. |

## `doctor`

Validates configuration and credentials, then exits.

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

The `key` line answers the question an operator cannot see from anywhere else: whether a restart needs a person.
With no `vault.key_source` it says so, and with `github.app_id` set and the vault locked the `scm` line says
`not wired up: the vault is locked` rather than claiming no source control is configured — the fix for those two
is not the same.

Every listener is listed, and each `ziti` one is **bound and released** to prove it can be:

```text
listen:  tcp 127.0.0.1:8471
         ziti service docpreview-admin (identity …\docpreview-host.json)
bind:    ziti service docpreview-admin (identity …\docpreview-host.json) ok
```

The two ways that fails — an identity that will not authenticate, and a service with no Bind policy naming it
— are both invisible in the config file and both present at runtime as a dashboard that cannot be reached.
TCP listeners are deliberately *not* bound: a port already in use is the expected state when the daemon is
running, and failing a health check for that would be actively misleading.

It makes real calls — asks GitHub who the App is, asks the zrok controller whether the environment is valid.
Both can fail while the local state looks perfect, which is exactly the situation worth catching before a
reviewer is waiting on a comment.

With nothing configured it still passes, and says what is missing rather than failing:

```text
exposer: local
driver:  local
key:     none — the daemon starts locked and is unlocked from the dashboard
scm:     none — set local.enabled or github.app_id to receive webhooks

all checks passed
```

The vault line is absent there because nothing needed it. The vault is opened lazily, so a setup with the
`local` exposer and no source control never asks for a master key — demanding a passphrase to serve a
directory on loopback would be pure ceremony.

## `shares list`

What the exposer's account actually holds, compared against what the database claims. It answers two questions that
have opposite fixes and were both invisible before it existed: is this account paying for something nothing uses,
and is something advertising a URL that no longer resolves.

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
| `ok` | The account holds it and the database claims it |
| `orphan` | The account holds it, the database does not. Costs a share and a reserved name against quota, and serves a URL nothing links to. `Reap` deletes these at its next sweep |
| `missing` | The database claims it, the account does not hold it. A comment or the dashboard is offering a URL that now 404s — rebuild the preview |
| `never` | A recorded preview that has not published yet. Counted apart so a queued build is not reported as a problem |

A `@branch` in the pull request column is a [branch preview](./projects.md#the-default-branch-is-always-previewed)
rather than a pull request.

**It deletes nothing.** `Reap` is what deletes, and an audit command that also deleted is one nobody would run.
`-json` gives the same data for a script.

:::note Two things it cannot see

A **leaked reserved name** — the object the quota actually counts — is invisible once its share is gone, because
the listing is of shares. Only the `zrok2` exposer can answer at all — the others say so and point at `doctor`.

:::

## `zrok`

Signing up for zrok, enrolling this host, and choosing which of the two possible zrok environments the daemon
uses. The same operations as the panel on `/secrets`, for a host with no browser on it.

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

Records which environment the daemon adopts. **Takes effect at the next restart** — zrok's root directory is a
process-wide setting read once at startup.

| | |
|---|---|
| `system` | `~/.zrok2`, what the `zrok2` CLI uses |
| `project` | `<data_dir>/zrok2`, docpreview's own, beside the vault |

With nothing recorded, a daemon adopts whichever is enabled and writes that down. With **both** enabled it uses
`project`, warns, and records nothing — so the dashboard keeps asking. See
[Runbook — zrok v2](../runbooks/zrok2.md) for why that is not a default worth guessing.

### `zrok invite <email>`

Asks the zrok service to email a registration link. Refused if an environment is already enrolled in either
directory.

| Flag | |
|---|---|
| `-api-endpoint` | A self-hosted zrok. Default `https://api-v2.zrok.io`. |
| `-invite-token` | For a zrok service that is itself invitation-only (`tokenStrategy: store`). |

### `zrok register <link-or-token>`

Creates the account and enrols this host. Takes the whole emailed link or only the token at the end of it.

The **zrok account password** is read from stdin, never an argument. It is not stored here — it is how you reset
that account later, so keep it somewhere.

The account token goes into the vault as `zrok.account_token`. A locked vault is not fatal: the enrolment still
happens and the command says how to store the token afterwards. `-no-enable` stops after creating the account.

### `zrok enable`

Enrols this host against an account token you already have. Reads it from the vault, or from stdin with
`-token-stdin` — which also stores it.

### `zrok disable -yes`

Removes this host's environment from the account. `-yes` is required because every share published through it is
deleted: preview URLs stop answering until the daemon republishes. The reserved names belong to the account and
survive, so the URLs come back unchanged.

## `sim`

Bare git repositories that behave like pull requests, so the whole loop runs with no GitHub account, no
network, and no tunnel. See [Local platform](../local-platform.md) for what it does and how it works.

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

Requires a TCP listener: the hook is a `curl`, so the ingress needs an address on this machine. An
overlay-only configuration is refused with a message saying so, rather than writing a hook that fails on every
push with a bare curl error.

### `sim list`

Every simulated repository, with its branch count.

There is no `sim rm`. Delete the `.git` directory under `local.repos_dir` — previews built from it expire on
`preview.ttl` — or delete the branch and push to tear them down immediately.

## `vault`

Credentials, encrypted at rest in a single file with [age](https://age-encryption.org/) — see
[age, and why the vault uses it](../background/age.md) if that name is new.

The master key comes from `vault.key_source` in the config — a file, or a command that prints it — then
`$DOCPREVIEW_MASTER_KEY`, then a prompt on a terminal. With none of the three the daemon starts locked and is
unlocked from the dashboard. See [security](./security.md#the-master-key) for which to pick.

The vault is `<data_dir>/vault.age`, by default `~/.docpreview/vault.age`. It holds a JSON map of names to
values, encrypted as one blob — so the file leaks neither the values nor which secrets exist.

### `vault keygen`

```text
docpreview vault keygen [-out FILE] [-shell SHELL] [-quiet]
```

Generates a new [age X25519 identity](../background/age.md#how-it-works). Three output shapes, all three keep
the key out of your shell history. `-out` and `-shell` together are refused — they are the same job done two
ways.

#### Write it to a file

```powershell
docpreview vault keygen -out C:\ProgramData\docpreview\master.key
```

```bash
docpreview vault keygen -out /etc/docpreview/master.key
```

The key is written at mode 0600 and never printed. This is the shape that pairs with `vault.key_source`, and
therefore the shape that lets the daemon come back from a reboot without a person:

```yaml
vault:
  key_source: "file:/etc/docpreview/master.key"
```

An existing file is **not** overwritten. It may be the only key to a vault full of credentials, and clobbering
it would make every secret in that vault unreadable with nothing to restore from. Move it aside first if you
mean to replace it.

Two placement rules are enforced when the daemon starts, not here: the file may not live inside `data_dir`, and
on Unix it may not be readable by group or other.

Prefer a secret manager to a file where you have one — the key then exists in the daemon process and nowhere
else on the machine:

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

With `-shell`, stdout is exactly one line — an assignment in that shell's syntax — so it can be evaluated
straight into the session. Everything explanatory goes to stderr, where the pipe cannot reach it and you can
still read it.

This keeps the key out of your shell history. Pasting `$env:DOCPREVIEW_MASTER_KEY = 'AGE-SECRET-KEY-1...'`
records the key. Piping records only the pipeline.

It still sets an environment variable, which is the least preferred of the three sources — readable by every
process under the same user, and present in service definitions, process listings and crash dumps. Fine for a
shell session you are about to close. Prefer `-out` or `exec:` for a daemon.

| `-shell` value | Emits |
|---|---|
| omitted or `auto` | detected from the process tree |
| `powershell`, `pwsh` | `$env:NAME = 'value'` |
| `sh`, `bash`, `zsh` | `export NAME='value'` |
| `fish` | `set -gx NAME 'value'` |
| `cmd` | `set NAME=value` |

Detection reads the **process tree**, not environment variables. PowerShell exports `PSModulePath` and Git
Bash sets `SHELL`, and each inherits the other's variable when nested — so any env-based guess is wrong in
some ordinary arrangement, and it fails silently by emitting `export` at a PowerShell prompt. Pass `-shell`
explicitly if you are invoking through a wrapper that confuses the walk.

`-quiet` suppresses the stderr commentary, for scripts.

#### See the key

```powershell
docpreview vault keygen
```

Bare, the key goes to stdout and the guidance to stderr, so it pipes cleanly into a password manager. Do this
too — the `-shell` form sets the variable for one session only, and losing the key means losing the vault.

#### Passphrases

A passphrase works instead of a generated key: any `$DOCPREVIEW_MASTER_KEY` that does not begin with
`AGE-SECRET-KEY-1` is treated as one and stretched with scrypt.

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

Trailing newlines are stripped, because that is what a shell adds rather than part of the credential. A PEM
key keeps its internal newlines. Empty values are refused.

### `vault list`

```powershell
docpreview vault list
```

```text
github.private_key
github.webhook_secret
```

Names only, sorted. There is no command that prints a value, deliberately.

### `vault delete <key>`

```powershell
docpreview vault delete frontdoor.api_token
```

### Known keys

| Key | Used by |
|---|---|
| `github.private_key` | GitHub App — the `.pem` GitHub generated |
| `github.webhook_secret` | GitHub App — webhook HMAC |
| `bitbucket.email` | Bitbucket — Atlassian account email |
| `bitbucket.api_token` | Bitbucket — API token (app passwords are dead) |
| `bitbucket.webhook_secret` | Bitbucket — webhook HMAC |
| `frontdoor.api_token` | Frontdoor exposer |

## Exit codes

| Code | |
|---|---|
| `0` | Success. Also what `-h` on a subcommand exits with, after printing that subcommand's flags. |
| `1` | The command ran and failed, with the reason on stderr. |
| `2` | The command line itself was wrong — an unknown flag, or a flag a subcommand does not have. Go's flag package prints the usage and exits. docpreview never sees it. |

`unknown command` is `1`, not `2`: the binary got that far and answered.
