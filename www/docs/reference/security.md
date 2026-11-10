---
id: security
title: Security model
sidebar_position: 4
---

# Security model

A preview builder clones repositories and runs code from them. It holds credentials that can write to those
repositories, and it publishes the result on the internet. This page says what NetFoundry Preview defends against
and what it doesn't.

## Threat model

**Defended against:**

- Someone who gets the data directory: a stolen laptop, a leaked backup, a VM snapshot.
- Someone who finds the webhook URL and posts forged events.
- Someone who reads the logs or the pull request comments looking for credentials.
- An outsider opening a fork pull request to get code run.
- A path in a repository config pointing somewhere it shouldn't.
- Someone who finds the tunnel URL and tries to write a credential through it.
- Someone who finds the dashboard URL. With a password set, every page except the previews, the webhooks and the
  health endpoints asks for a login.

**Not defended against:**

- Someone already running code as the daemon's user. They've won, and no file format changes that.
- Anything that proxies to the daemon and strips forwarding headers. The locality route below would treat those
  requests as local. Nothing NetFoundry Preview ships does this, but a proxy you put in front of it might.
- A guessed or shared password. `/login` has no rate limit and no lockout, so the password is the whole defence. The
  comparison is constant-time and the hash is argon2id, which slows an offline attack on a stolen database but not
  an online one against the form.
- Anyone at all, until a viewer password is set. Nothing is gated before then, on purpose: an upgrade must not lock
  people out of their own dashboard.
- A contributor with push access to a branch, under the `local` build driver. They can run any code on the build host.
  That's what `npm run build` is. Use the `docker` driver if that isn't acceptable.
- A malicious dependency in the repository's `package.json`. Same category.

## Credentials at rest

Everything sensitive is in one [age](../background/age.md)-encrypted file: `$DATA_DIR/vault.age`.

The vault uses [age](https://age-encryption.org) instead of a hand-rolled AEAD. Hand-rolled key derivation and nonce
handling fail in ways that quietly ruin this kind of file, and both failures still encrypt, still decrypt and still
pass every test you'd think to write. See [age, and why the vault uses it](../background/age.md) for the reasoning
and the alternatives.

Writes go to a temporary file in the same directory, which is then renamed over the target. A crash mid-write can't
leave a truncated vault behind.

**Three things are kept out of the vault on purpose.** The page that unlocks the vault is served by the daemon, so
anything the daemon needs to serve that page can't be inside it.

| | Where | Why |
|---|---|---|
| The console password hashes | `settings` table, argon2id | A password inside the vault couldn't be checked to reach the form that opens the vault |
| The session signing key | Memory, generated per process | Nothing persists it, which is why a restart signs everybody out |
| The zrok account token's other copy | `<data_dir>/zrok2/environment.json`, plaintext | zrok writes that file and controls its format. The vault holds a copy so re-enrolling needs no new signup. This file is why that directory sits next to the vault instead of in a home directory |

### The master key

The vault is a secrets manager, so its own key has nowhere inside the system to live. This is secret zero. No
configuration makes it go away. You only choose who or what holds it. [OWASP's guidance][owasp] is to move the
problem rather than solve it: "you will often have to secure the primary secret of that secrets management solution
in a secondary secrets management solution."

[owasp]: https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html

`vault.key_source` is where that second mechanism plugs in:

| Source | Spelling | The trade |
|---|---|---|
| A command | `exec:op read op://ops/nfpreview` | Recommended. The key exists in the daemon process and nowhere else on the machine. Survives a reboot if the helper can run unattended. |
| A file | `file:/etc/nfpreview/master.key` | Survives a reboot. Anyone who can read that path can read every secret in the vault. |
| The environment | `$NFPREVIEW_MASTER_KEY` | Works, but not recommended. Every process running as the same user can read it, and it ends up in service definitions, process listings and crash dumps. |
| A person | nothing configured | **Default.** The only option with no key stored anywhere. The daemon starts locked and serves the dashboard until somebody unlocks it, so after a reboot it does nothing until then. |

Write a key straight into a file, without it passing through a clipboard or a shell history:

```bash
nfpreview vault keygen -out /etc/nfpreview/master.key
```

Two rules are enforced at startup:

- **A key file can't live inside `data_dir`.** That directory holds `vault.age`, so a key next to it means one
  directory read gets both halves. Config validation refuses it.
- **On Unix, a key file readable by group or other is refused.** Its permissions and location are all that protect
  it. Windows isn't checked, because its permissions are ACLs that a mode bit doesn't describe.

The `exec:` form runs argv directly, never through a shell, so a config value can't turn into a different command.

The encrypted file leaks nothing, not even which secrets exist:

```bash
$ grep -c "github" ~/.nfpreview/vault.age
0
```

#### Changing it

```bash
nfpreview vault rekey -generate -yes    # with the daemon stopped
```

Every secret is kept, so you don't need the plaintexts. That matters, because the vault exists so nobody keeps a copy
of them. Without rekey, changing the key means storing each credential again by hand, so a key picked in a hurry
during setup would be permanent.

Rekey doesn't change the secrets. Anybody who read one under the old key still knows it, so rotate that credential
where it was issued. It also can't help a running daemon: the old key is in memory, and the next write would undo the
rekey. See [`vault rekey`](./cli.md#vault-rekey) for the order of steps and what the backup is for.

Change a weak key even if the file permissions look fine. On the host, the permissions protect the secrets, since
anything that can read `vault.age` can read a key file next to it. But a short passphrase protects nothing once the
vault travels without its key: a snapshot, a backup, or a copy left behind by
[moving an installation](../guides/move-an-installation.md#afterwards). Four digits is ten thousand guesses.

## Credentials in memory

Every credential is wrapped in a `Secret` type that implements `Stringer`, `Formatter`, `GoStringer` and
`json.Marshaler` to return `***REDACTED***`.

That covers every path a value could accidentally take to an output:

```go
fmt.Sprintf("%v", s)     // ***REDACTED***
fmt.Sprintf("%s", s)     // ***REDACTED***
fmt.Sprintf("%q", s)     // "***REDACTED***"
fmt.Sprintf("%x", s)     // ***REDACTED***   <- without Formatter, a hex dump
fmt.Sprintf("%+v", cfg)  // ***REDACTED***   <- the realistic case
json.Marshal(cfg)        // ***REDACTED***
slog.Info("x", "key", s) // ***REDACTED***
```

`%x` is the one to watch. Without `Formatter`, a `[]byte`-backed type would hex-dump the credential. There's no
`UnmarshalJSON`, so no code outside the vault package can round-trip a secret through JSON and get the value back.

Getting a value out means calling `Reveal()`, which is awkward to type and easy to grep for.

## The login

**Once a viewer password is set, the whole dashboard is behind a login**, including from loopback. There are two
roles, and they're different things rather than two levels of one:

| Username | What it can do |
|---|---|
| `admin` | Everything, from anywhere the daemon is reachable |
| `viewer` | Read everything and rebuild a preview. Nothing else |

Set them with [`nfpreview console password`](./cli.md). Until a **viewer** password exists nothing is gated, so an
upgrade can't lock an installation with no password out of its own dashboard.

These are never gated:

| Path | Why |
|---|---|
| `/webhook/*` | Already authenticated by an HMAC over the body, and its callers have no cookie. A cookie gate here would break every delivery, and the failure would look like a signature problem |
| `/healthz`, `/readyz` | What a supervisor and the restart script poll. `/readyz` carries counts and a stage name and nothing that identifies a repository. See [the CLI reference](./cli.md) |
| The previews | A preview URL goes in a pull request for a reviewer who has no login and shouldn't need one. Under `local` that's `/preview/…` on this listener. The other exposers never reach this mux |
| `/login`, `/logout`, `/auth/*` | The form can't be behind the form, and the OAuth callback comes from Google with no cookie of ours |

:::danger Loopback is not exempt

Letting a request from this machine in as admin with no session sounds reasonable, since anyone who can reach
`127.0.0.1` can already run the binary. It's still the wrong rule, because the daemon can't tell a request is really
from this machine. A tunnel in proxy mode connects from a local process, so `RemoteAddr` is loopback for a request
that came from the internet. Trusting that would give every visitor an admin session as soon as something proxies
without setting a forwarding header.

:::

Sessions are a signed cookie with no server-side store, and the signing key is generated per process. So **every
restart signs everybody out**. Otherwise a session lasts twelve hours.

### Google sign-in grants viewer, never admin

Google sign-in is optional. An address at a domain on the allow-list gets **viewer**. Admin is password-only, because
"somebody at this company" is not the same claim as "the person who runs this installation". Configure it with
`nfpreview console oauth-domains acme.com`. The check is an exact match on the part after the last `@`, not a suffix
test, so a list naming `acme.com` doesn't let in `evil-acme.com`.

The client id and secret live in the vault. When the vault is locked, the login page shows only the password field,
not a button that can't work.

## The mutating surfaces

Apart from the webhook and the login routes, three surfaces change anything:

| Surface | Endpoints | What it can do |
|---|---|---|
| Credentials | `/api/secrets/…` | Create the vault, unlock it, store a credential, delete one, generate a webhook secret |
| [Projects](./projects.md) | `/api/projects/…`, `/api/builds/…`, `/api/images/…`, `/api/cache/…`, `/api/settings/…` | Decide which repositories build and how, hold a per-project environment variable, cancel, rebuild or unlink a build, clear a build cache, set the hostname prefix |
| Exposer | `/api/zrok/…` | Sign up for zrok, enrol or un-enrol this host, store the Frontdoor token, enrol a ziti identity, switch which exposer publishes |

All three share the gates below. **The projects surface is the most dangerous.** A project decides what command runs
on the build host, and its environment variables are credentials handed to the process that runs it. That's a more
direct route to running code on this machine than reading the vault. The exposer surface is next: it spends an
account's quota and can take every published preview URL down.

The one exception is **Rebuild**. Any signed-in user can press it, viewers too. A rebuild runs the command the project
or the repository already set, at a commit already recorded, so it can't choose what runs.

All of them are on the same listener as the dashboard, the previews and the webhook, so how reachable they are
depends on your deployment, not on the code. **One of these must be true** before any of them will act
(`internal/daemon/secrets.go`):

- **The request has an admin session.** This is authentication. Apart from the two overlay routes below, it's the only one
  that can be true from another machine, which is the point of having a password and the only way to manage credentials remotely.
- **The request came over an OpenZiti listener from an identity in `admin_identities`.** See below.
- **The request came over a [`zrok_private` listener](./configuration.md#a-zrok_private-listener).** The share is in
  closed permission mode, so zrok already limited who can connect to the owning account and its access grants.
- **The request came from this machine**: a loopback `RemoteAddr` **and** no `X-Forwarded-For`, `X-Forwarded-Host`,
  `X-Real-Ip` or `Forwarded` header. On top of that, every configured listener must be loopback, a ziti listener
  naming `admin_identities`, or a `zrok_private` listener. A daemon bound to `0.0.0.0` refuses on this route however
  local the request looks.

A **viewer** session doesn't fall through to the locality test. Signing in as a viewer says this caller is not an
admin. Treating a viewer on loopback as admin would make signing in as a viewer give you more than not signing in.
The request is refused with a reason the page shows.

### Why the locality route needs both halves

Checking where the daemon listens isn't enough, because of tunnels. `zrok2 share public http://127.0.0.1:8471`
publishes every route the daemon serves while the listener is still loopback. A listener test alone would say yes
with `/api/secrets` on the internet. With the vault unlocked, `PUT`, `DELETE` and generate would all work for anyone
holding the share URL.

`RemoteAddr` alone can't tell the difference either. In zrok's proxy mode the daemon sees the connection from the
local tunnel process, so the address is loopback too, and `Host` is whatever the client sent. The forwarding-header
test catches it. Anything that proxies to the daemon sets one of those headers, including NetFoundry Preview's own
`webhook-only` proxy. A caller that adds a header itself only makes the check refuse more, which is the safe way to be
wrong.

### The locality route is a boundary, not authentication

The locality test doesn't check a credential. It decides whether a request started on this machine from what the
network setup tells it, not from anything the caller proves. It assumes nothing proxies to the daemon while stripping
forwarding headers.

So it's one of several routes, not the only one. **Set an admin password and the surface has authentication.** The
locality route is there so a fresh installation with no password can be set up from its own machine. The boundary
there is the same one that already protects `nfpreview vault set`: anyone who can reach `127.0.0.1` can run the
binary, and the API gives them nothing a shell doesn't.

To manage credentials from another machine, set the admin password.

### Over an OpenZiti listener, the gate is an identity

A request on a [`ziti` listener](./configuration.md#a-ziti-listener) is judged on **who dialed it**, not where it came
from. That's stronger than the locality test, since the overlay authenticated the identity and there's no address to
spoof. It's only as good as the grant, though, so the grant is an explicit list of identity ids on the listener:
`admin_identities`.

| | |
|---|---|
| An id in `admin_identities` | Can write |
| A listener naming nobody | Read-only, and the default. Being enrolled on the network doesn't mean you may write a credential |
| An enrolled id not on the list | Read-only. The refusal names the id, so adding it is a copy and paste |
| No id at all | Refused. That's what a router that never sent the header produces, and "we can't tell who this is" is not a grant |

The identity is recorded when the connection is accepted, not read from the request. `http.Server.ConnContext` is the
only hook that sees the connection, and the grant belongs to the listener. It's stored under a context key only this
package can write, so a value forged elsewhere in the process grants nothing.

A mixed configuration is judged on its weakest listener. One `http.Server` serves them all, so allowing writes
because *a* listener is loopback would allow them through the one that isn't.

:::note Tested against a live overlay

`TestOverlayAdminGrantFollowsTheRealDialer` binds a real service and grants one identity by the id the controller
reports for it. The router names the dialer on every circuit, and the id it sends is the one the controller lists. The
listed identity may write. An unlisted identity is refused, and the refusal names its id. Every overlay client has an
identity, so the "no id at all" row is tested offline only. Point the test at your own network with the variables in
`internal/daemon/listen_integration_test.go`.

:::

### Point the tunnel at `webhook-only`, not the daemon

This matters more than the header check. `nfpreview webhook-only` forwards only `POST` to its webhook paths:
`/webhook/github` by default, or the paths you give it with `-path`, which you can repeat (`-path /webhook/bitbucket`).
A `GET` on one of those paths gets a `405`, and everything else gets a `404`. The credential API isn't reachable
through the tunnel at all, so the locality gate is a second line of defence instead of the only one. With `-zrok-name`
it binds no local TCP port either. See the [webhook tunnel guide](../guides/webhook-tunnel.md).

### Reading is open to anyone past the login

`GET /api/secrets`, `GET /api/projects` and `GET /api/zrok` have no gate of their own. They return names and
set/unset flags, never a value. Each one reports `can_write` and `read_only_why`, so a viewer's dashboard shows the
panel read-only with the reason on it, instead of hiding it or offering buttons that 403.

None of the three ever returns a credential: not in a list, not in an error, not after storing one. That includes the
zrok account token, which signup creates and puts straight into the vault without it appearing in any response.

A refused write is a `403` with the reason in it, and the daemon logs `refused a remote credential request` or
`refused a remote project change`, with the remote address, method and path.

The dashboard's **Projects**, **Settings** and **Clear caches** controls come from `GET /api/admin`, which runs the
same check as the write endpoints. The server decides, not the page. A `Host`-header test in the browser would be
worthless, since `Host` is whatever the client typed. Anything short of a clear yes leaves the links out. That's what
a viewer session sees, and what [`dashboard-only`](./cli.md#dashboard-only) shows, since that endpoint isn't on its
allowlist.

### Per-build URLs are one more public surface

This applies only with `exposer.build_shares: true`, which is off by default. Then every retained build has its own
URL as well as the branch. The names include a commit, so they can't be guessed, but they are as public as the branch
URL. They are `noindex`, and they keep serving until the preview is torn down or `preview.keep_builds` evicts them.
If a commit had something in it that should never have been published, tearing down the pull request's preview
removes every build's URL, not just the latest. When `build_shares` is off, any per-build shares left from before are
deleted at the next start.

## Credentials in git output

GitHub App clone URLs carry an installation token:

```text
https://x-access-token:ghs_...@github.com/acme/docs.git
```

Git echoes the remote URL in several of its error messages, and build output goes into the pull request comment. Left
alone, a clone failure would publish a live GitHub credential **to the pull request**.

So every byte of git output is scrubbed: any `scheme://userinfo@host` becomes `scheme://***REDACTED***@host`. The
tests include one proving the scrubber finishes. Rescanning from the start after each replacement would loop forever
on a line it had already redacted.

### Userinfo ends at the last `@`, not the first

Splitting on the first `@` leaks the token it was meant to hide. A Bitbucket credential uses an email address as the
username, so on

```text
https://someone@example.com:TOKEN@bitbucket.org/ws/docs.git
```

that rule redacts `someone` and prints `:TOKEN@bitbucket.org/ws/docs.git` as is, into the build log and from there
into the pull request comment.

The rule is RFC 3986's (`scrubLine` in `internal/pipeline/clone.go`). After `://`, the authority ends at the first
`/`, `?`, `#` or whitespace, and userinfo is everything before the **last** `@` inside it. Whitespace counts because
these are lines of prose with URLs in them, not bare URLs. An unescaped `@` in userinfo isn't legal, but git accepts
it and people write it, so the scrubber has to handle input that is wrong. `internal/pipeline/clone_test.go` covers
the email-username case and two credentialed URLs on one line, and checks that the token is **absent** rather than
that the output matches a string.

## Webhook authentication

HMAC-SHA256, in `X-Hub-Signature-256` from GitHub and `X-Hub-Signature` from Bitbucket, verified with `hmac.Equal`
**before the body is parsed**.

Both parts matter. The endpoint is on the internet by design, so the JSON parser must never see bytes that haven't
been authenticated. And the comparison must be constant-time, or it leaks the expected digest one byte at a time.

A rejected delivery gets a bare `401 unauthorized`. Telling a prober why their signature failed only helps the prober.

## Fork pull requests

Refused at the webhook, before the payload is queued.

```text
WARN refusing to build a fork pull request pr=acme/docs#7 head_repo=mallory/docs
```

Building a fork means cloning an outsider's repository and running its `package.json` scripts with an installation
token that can write to yours. There's no setting to allow it, and there shouldn't be.

## Repository config

On any repository where anyone can open a pull request, an attacker can write `.nfpreview.yml`. So:

- Paths are refused if absolute, drive-qualified or reaching outside the root.
- `build.command` is honored under **both** drivers, so under `local` it's a shell command on the build host. That
  only restates what the driver already allows: `npm run build` runs `package.json` scripts from the same branch.
  Under `local`, "who can open a pull request here" and "who would I give a shell to" are the same question. If the
  answers differ, run `docker`, or set the command yourself on a [project](./projects.md).
- `build.env` can't set reserved variables. A pull request that could set `DOCUSAURUS_BASE_URL` could break its own
  preview in a way that looks like a NetFoundry Preview bug.
- `detect.script` is resolved through symlinks and checked again against the workspace root, because a symlink in an
  attacker's tree can point anywhere.
- The detect script gets a minimal environment. Under `docker` it runs in a container with no network, the checkout
  mounted read-only, and only `NFPREVIEW=1` and `HOME=/tmp` set. Under `local` it gets `PATH`, `HOME` pointed at the
  workspace and `NFPREVIEW=1`, and only a script the project names runs. A build server's environment holds things a
  pull request author shouldn't have.
- **A build whose output contains a symlink is refused, not published**, under either driver. The preview file server
  blocks path traversal but follows symlinks out of its root, so `build/leak -> /etc/passwd` would be served to anyone
  with the preview URL.

A [project](./projects.md) overrides every build field above. That's what projects are for: `.nfpreview.yml` comes
in with the pull request, and the project is yours. A project doesn't relax any of these rules. Its `command` still
runs, but you wrote it.

## The preview surface

Previews serve HTML written by anyone who can open a pull request. They're served with:

| Header | Why |
|---|---|
| `Cache-Control: no-store, must-revalidate` | A cached preview means a reviewer refreshes and sees the version they already rejected. |
| `X-Robots-Tag: noindex, nofollow` | An unreleased site doesn't belong in a search index. |
| `X-Content-Type-Options: nosniff` | |
| `Referrer-Policy: no-referrer` | |

Only `GET` and `HEAD` are allowed. Everything else gets `405`. Path traversal is handled by `http.Dir`, which also
rejects the Windows device names and alternate separators a hand-crafted request might try.

These headers harden the previews. They aren't a WAF. For content you really don't trust, put
[Frontdoor](../guides/frontdoor.md) in front.

## GitHub App permissions

Grant these and nothing else:

| Permission | Access |
|---|---|
| Contents | Read-only |
| Pull requests | Read and write |
| Checks | Read and write |
| Metadata | Read-only |

Install it on **selected repositories**, not all of them.

Installation tokens last an hour and are cached in memory, refreshed five minutes before they expire. The App JWT
lasts nine minutes, under GitHub's limit of ten, and is backdated sixty seconds for clock skew. Without that, a host
running a minute fast fails every API call with a 401 that says nothing about clocks.

## Operational advice

**Set both passwords first.** Run `nfpreview console password -role admin` and `-role viewer`. Until the viewer
password exists nothing is gated. `nfpreview console status` shows what is set and what this daemon listens on.

**Bind loopback, and don't share the daemon.** The default `listen` is `127.0.0.1:8471`. Run `nfpreview
webhook-only` and point the tunnel at that. A share of the daemon itself publishes the dashboard, every preview and
`/api/secrets` along with the webhook. See the [webhook tunnel guide](../guides/webhook-tunnel.md). The login is a
second line here, not a replacement. `dashboard-only` publishes an allowlist of read paths, plus login, logout and
rebuild, and `/api/admin` isn't on it.

**Rotate the private key.** See the [GitHub App guide](../guides/github-app.md). There's no downtime. The step people
forget is deleting the old key afterwards.

**Keep the master key out of the config file.** `vault.key_source` names where to read it from, a command or a file,
so the key itself is never a config value. `exec:` keeps it out of every file on the machine. See
[the master key](#the-master-key).

**Close pull requests.** With `teardown_on_close: true`, closing one withdraws the URL. Otherwise the TTL removes it
after 72 hours.
