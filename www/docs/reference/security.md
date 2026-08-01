---
id: security
title: Security model
sidebar_position: 4
---

# Security model

A preview builder is a machine that clones repositories and executes code from them, holds credentials that
can write to those repositories, and publishes the result on the internet. It is worth being explicit about
what is defended and what is not.

## Threat model

**Defended against:**

- Someone who obtains the data directory — a stolen laptop, a leaked backup, a VM snapshot.
- Someone who finds the webhook URL and posts forged events.
- Someone who reads the logs, or the pull request comments, looking for credentials.
- An outsider opening a fork pull request to get code executed.
- A path in a repository config pointing somewhere it should not.
- Someone who finds the tunnel URL and tries to write a credential through it.
- Someone who finds the dashboard URL. With a password set, every page but the previews, the webhooks and the
  health endpoints asks for a login.

**Not defended against:**

- Someone already running code as the docpreview user. They have won; no file format changes that.
- Anything that proxies to the daemon while stripping forwarding headers. The locality route below would read
  those requests as local. Nothing docpreview ships does this; a proxy you put in front of it might.
- A guessed or shared password. There is no rate limit on `/login` and no lockout, so the password is the whole
  defence — the comparison is constant-time and the hash is argon2id, which slows an offline attack on a stolen
  database rather than an online one against the form.
- Anyone at all, until a viewer password is set. Nothing is gated before then, deliberately: a feature that locks
  people out on upgrade is not shippable.
- A contributor with push access to a branch, under the `local` build driver. They can run arbitrary code on
  the build host, by design — that is what `npm run build` is. Use the `docker` driver if that is not
  acceptable.
- A malicious dependency in the repository's `package.json`. Same category.

## Credentials at rest

Everything sensitive is in one [age](../background/age.md)-encrypted file: `$DATA_DIR/vault.age`.

[age](https://age-encryption.org) rather than a hand-rolled AEAD, because the ways hand-rolled key derivation
and nonce handling go wrong are exactly the ways that quietly ruin this kind of file — and both failures still
encrypt, still decrypt, and still pass every test you would think to write. See
[age, and why the vault uses it](../background/age.md) for the reasoning and the alternatives considered.

Writes go to a temporary file in the same directory and are then renamed over the target, so a crash
mid-write cannot leave a truncated and therefore unrecoverable vault behind.

**Three things are deliberately not in the vault**, and each for the same ordering reason: the page that unlocks
the vault is served by the daemon, so anything the daemon needs in order to serve that page cannot be inside it.

| | Where | Why |
|---|---|---|
| The console password hashes | `settings` table, argon2id | A password inside the vault could not be checked in order to reach the form that opens the vault |
| The session signing key | Memory, generated per process | Nothing persists it, which is why a restart signs everybody out |
| The zrok account token's other copy | `<data_dir>/zrok2/environment.json`, plaintext | zrok writes that file and docpreview does not control its format. The vault holds a copy so re-enrolling needs no new signup; the file is why that directory sits beside the vault rather than in a home directory |

### The master key

The vault is a secrets manager, so its own key has nowhere inside the system to live. This is secret zero, and
there is no configuration that makes it go away — only configurations that decide who or what holds it.
[OWASP's guidance][owasp] is to move the problem rather than solve it: "you will often have to secure the
primary secret of that secrets management solution in a secondary secrets management solution."

[owasp]: https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html

`vault.key_source` is where that secondary mechanism attaches:

| Source | Spelling | The trade |
|---|---|---|
| A command | `exec:op read op://ops/docpreview` | Recommended. The key exists in the daemon process and nowhere else on the machine. Survives a reboot if the helper can run unattended. |
| A file | `file:/etc/docpreview/master.key` | Survives a reboot. Anyone who can read that path can read every secret in the vault. |
| The environment | `$DOCPREVIEW_MASTER_KEY` | Works, not recommended. Readable by every process under the same user; lands in service definitions, process listings and crash dumps. |
| A person | nothing configured | **Default.** The only option with no key at rest. The daemon boots locked and serves the dashboard until somebody unlocks it — so a reboot leaves it inert. |

Mint a key straight into a file, without it passing through a clipboard or a shell history:

```bash
docpreview vault keygen -out /etc/docpreview/master.key
```

Two rules are enforced at startup rather than documented and hoped for:

- **A key file may not live inside `data_dir`.** That directory holds `vault.age`, so a key beside it means one
  directory read yields both halves. Config validation refuses it.
- **On Unix, a key file readable by group or other is refused.** Its permissions and its location are the only
  things protecting it. Not checked on Windows, where permissions are ACLs that a mode bit does not describe.

The `exec:` form runs argv directly and never through a shell, so a config value cannot become command
construction.

The encrypted file leaks nothing, including which secrets exist:

```bash
$ grep -c "github" ~/.docpreview/vault.age
0
```

## Credentials in memory

Every credential is wrapped in a `Secret` type that implements `Stringer`, `Formatter`, `GoStringer`, and
`json.Marshaler` to return `***REDACTED***`.

That covers every path a value could accidentally reach an output:

```go
fmt.Sprintf("%v", s)     // ***REDACTED***
fmt.Sprintf("%s", s)     // ***REDACTED***
fmt.Sprintf("%q", s)     // "***REDACTED***"
fmt.Sprintf("%x", s)     // ***REDACTED***   <- without Formatter, a hex dump
fmt.Sprintf("%+v", cfg)  // ***REDACTED***   <- the realistic case
json.Marshal(cfg)        // ***REDACTED***
slog.Info("x", "key", s) // ***REDACTED***
```

`%x` is the interesting one: without `Formatter`, a `[]byte`-backed type would happily hex-dump the
credential. There is no `UnmarshalJSON`, so no code path outside the vault package can round-trip a secret
through JSON and get the value back.

Getting a value out requires calling `Reveal()`, which is awkward to type and easy to grep for.

## The login

**The whole dashboard is behind a password once one is set**, including from loopback. Two roles, and they are not
two levels of the same thing:

| Username | What it can do |
|---|---|
| `admin` | Everything, from anywhere the daemon is reachable |
| `viewer` | Read everything, change nothing |

Set them with [`docpreview console password`](./cli.md). Until a **viewer** password exists nothing is gated, which
is what every installation had before this existed — a feature that locks people out on upgrade is not shippable.

What is never gated, and each for a load-bearing reason:

| Path | Why |
|---|---|
| `/webhook/*` | Authenticated already, by an HMAC over the body, and its callers hold no cookie. A cookie gate here breaks every delivery and the failure reads as a signature problem |
| `/healthz`, `/readyz` | What a supervisor and the restart script poll. `/readyz` carries counts and a stage name and nothing that identifies a repository — see [the CLI reference](./cli.md) |
| The previews | A preview URL goes in a pull request for a reviewer who has no login and should not need one. Under `local` that is `/preview/…` on this listener; the other exposers never reach this mux |
| `/login`, `/logout`, `/auth/*` | The form cannot be behind the form, and the OAuth callback arrives from Google carrying no cookie of ours |

:::danger Loopback is not exempt

It was, briefly. A request from this machine was admitted as admin with no session, on the reasoning that anyone who
can reach `127.0.0.1` can already run the binary — true, and the wrong rule, because "from this machine" is not
something the daemon can establish. A tunnel in proxy mode connects from a local process, so `RemoteAddr` is
loopback for a request that arrived from the internet. Betting the login on that hands every visitor an admin
session the moment something proxies without setting a forwarding header.

:::

Sessions are a signed cookie with no server-side store, and the signing key is generated per process — so **every
restart signs everybody out**. Twelve hours otherwise.

### Google sign-in grants viewer, never admin

Optional, and worth stating exactly: an address at a domain on the allow-list gets **viewer**. Admin is
password-only, because the two are different claims — "somebody at this company" is not "the person who administers
this installation". Configure with `docpreview console oauth-domains acme.com`; the check is an exact match on the
part after the last `@`, not a suffix test, so a list naming `acme.com` does not admit `evil-acme.com`.

The client id and secret live in the vault, so a locked vault means the login page offers the password field alone
rather than a button that cannot work.

## The mutating surfaces

There are three, and everything else the daemon serves is a `GET`:

| Surface | Endpoints | What it can do |
|---|---|---|
| Credentials | `/api/secrets/…` | Create the vault, unlock it, store a credential, delete one, generate a webhook secret |
| [Projects](./projects.md) | `/api/projects/…`, `/api/cache/…`, `/api/settings/…` | Decide which repositories build and how, hold a per-project environment variable, clear a build cache, set the hostname prefix |
| Exposer | `/api/zrok/…` | Sign up for zrok, enrol or un-enrol this host, store the Frontdoor token, enrol a ziti identity, switch which exposer publishes |

They share the gates below, deliberately. **The projects surface is the most dangerous of the three**: a project row
decides what command runs on the build host, and a project's environment variables are credentials handed to the
process that runs it — a more direct route to executing code on this machine than reading the vault would be. The
exposer surface is next: it spends an account's quota and can take every published preview URL down.

All of them live on the same listener that serves the dashboard, the previews and the webhook, so how reachable they
are is a property of your deployment, not of the code. **One of these must hold** before any of them will act
(`internal/daemon/secrets.go`):

- **The request carries an admin session.** This is authentication, and it is the only one of the three that can be
  true from another machine — which is the point of having a password, and the reason "managing credentials remotely
  is unsupported" is no longer the answer.
- **The request arrived over an OpenZiti listener from an identity in `admin_identities`.** See below.
- **The request arrived locally** — a loopback `RemoteAddr` **and** no `X-Forwarded-For`, `X-Forwarded-Host`,
  `X-Real-Ip` or `Forwarded` header — *and* every configured listener is loopback or a ziti listener naming
  `admin_identities`. A daemon bound to `0.0.0.0` refuses on this route however local the request looks.

A **viewer** session does not fall through to the locality test. It is a positive statement that this caller is not
an admin, so treating a viewer on the loopback interface as one would make signing in as a viewer a way to gain less
than nothing. It is refused with a reason the page shows.

### Why the locality route needs both halves

Checking where the daemon listens is not enough, and the gap is a tunnel. `zrok2 share public
http://127.0.0.1:8471` publishes every route the daemon serves while the listener is still loopback, so the listener
test answered yes with `/api/secrets` on the internet — and with the vault unlocked, `PUT`, `DELETE` and generate
would all have succeeded for anyone holding the share URL.

`RemoteAddr` alone cannot tell the difference. In zrok's proxy mode the daemon sees the connection from the local
tunnel process, so the address is loopback too, and `Host` is whatever the client sent. The forwarding-header test
is what closes it: anything that proxies to the daemon sets one of those headers, including docpreview's own
`webhook-only` proxy. A caller that adds a header itself only makes the check refuse more, which is the safe
direction to be wrong in.

### The locality route is a boundary, not authentication

Taken on its own, the locality test is not a credential check. It answers "did this request originate on this
machine" from evidence the network arrangement supplies rather than from anything the caller proves, and it assumes
nothing proxies to the daemon while stripping forwarding headers.

That is why it is now one of three routes rather than the only one. **Set an admin password and the surface has real
authentication**; the locality route remains so that a fresh installation with no password can be set up from the
machine it is on, where the boundary is the same one that already protects `docpreview vault set` — anyone who can
reach `127.0.0.1` can run the binary, and the API adds no reach a shell did not already have.

Managing credentials from another machine is supported now, with the admin password. Without one it is not.

### Over an OpenZiti listener, the gate is an identity instead

A request that arrives on a [`ziti` listener](./configuration.md#a-ziti-listener) is judged on **who dialed it**
rather than on where it came from. That is a stronger statement than the locality test above — the overlay
authenticated the identity and there is no address to spoof — but it is only as good as the grant, so the grant is
an explicit list of identity ids on the listener, `admin_identities`.

Four properties, and three of them are refusals:

| | |
|---|---|
| An id in `admin_identities` | May write |
| A listener naming nobody | Read-only. Being enrolled on the network is not authorization to write a credential, and this is the default |
| An enrolled id not on the list | Read-only, and the refusal names the id so adding it is a copy and paste |
| No id at all | Refused. That is what a router which never sent the header produces, and "we cannot tell who this is" is not a grant |

The identity is recorded when the connection is accepted, not read from the request — `http.Server.ConnContext` is
the only hook that sees the connection, and the grant belongs to the listener rather than to the request. It is
stored under a context key this package alone can write, so a value forged elsewhere in the process grants nothing.

A mixed configuration is judged on its weakest listener. One `http.Server` serves them all, so allowing writes
because *a* listener is loopback would allow them through the one that is not.

:::note Not yet exercised against a live overlay

The identity plumbing is tested offline, including every refusal. What no test here proves is that the router
actually sends the dialer on a real circuit, or that the id matches what the controller shows. Until that has been
run against a live network, treat the overlay grant as untried rather than as load-bearing.

:::

### Point the tunnel at `webhook-only`, not the daemon

The architectural half of the fix, and the one that matters more than the header check. `docpreview webhook-only`
forwards exactly `POST /webhook/github` and answers 404 to everything else, so the credential API is not
reachable through the tunnel at all and the locality gate becomes a second line rather than the only one. With
`-zrok-name` it binds no local TCP port either. See the [webhook tunnel runbook](../runbooks/webhook-tunnel.md).

### Reading is open to anyone who got past the login

`GET /api/secrets`, `GET /api/projects` and `GET /api/zrok` apply no gate of their own. They return names and
set/unset flags, never a value, and each reports `can_write` plus `read_only_why` so a viewer's dashboard renders
the panel read-only with the reason on it. A panel that vanished instead would read as a broken feature, and one
offering buttons that 403 is worse than one that explains itself.

None of the three ever returns a credential. Not on list, not in an error, not after storing one — including the
zrok account token, which the signup flow creates and puts straight into the vault without it appearing in any
response.

A refused write is a `403` with the reason in it, and the daemon logs it — `refused a remote credential request` or
`refused a remote project change`, with the remote address, method and path.

The **Projects**, **Secrets** and **Clear caches** controls on the dashboard are drawn from `GET /api/admin`, which
runs the same check the write endpoints run. The server decides, not the page: a `Host`-header test in the browser
would be worthless, since `Host` is whatever the client typed. Anything other than an outright yes leaves the links
absent — which is what happens for a viewer session, and through
[`dashboard-only`](./cli.md#dashboard-only), where that endpoint is not in the allowlist at all.

### One preview URL per build is one more public surface

Every retained build has its own URL as well as every branch. Nothing about them is guessable — the name carries a
commit — but they are public in exactly the way the branch URL is, they are `noindex`, and they keep serving until
the preview is torn down or `preview.keep_builds` evicts them. If a commit contained something that should never have
been published, tearing the pull request's preview down removes every build's URL, not only the latest.

## Credentials in git output

GitHub App clone URLs carry an installation token:

```text
https://x-access-token:ghs_...@github.com/acme/docs.git
```

Git echoes the remote URL in several of its error messages. Build output is attached to the pull request
comment. Without intervention, a clone failure would publish a live GitHub credential **to the pull request**.

So every byte of git output is scrubbed: any `scheme://userinfo@host` becomes
`scheme://***REDACTED***@host`. Tested, including that the scrubber terminates — an earlier version rescanned
from the start after each replacement and spun forever on a line it had already redacted.

### It leaked the token it existed to hide

Worth knowing, because it is the failure this section is here to prevent and it shipped anyway. The scrubber found
userinfo by splitting on the **first** `@`. A Bitbucket credential is authenticated with an email address as the
username, so on

```text
https://someone@example.com:TOKEN@bitbucket.org/ws/docs.git
```

it redacted `someone` and emitted `:TOKEN@bitbucket.org/ws/docs.git` verbatim — into the build log, and from there
into the pull request comment.

The rule is now RFC 3986's (`internal/pipeline/clone.go:161`): after `://`, the authority ends at the first `/`,
`?`, `#` or whitespace, and userinfo is everything before the **last** `@` inside it. Whitespace, because these are
prose lines with URLs embedded in them rather than bare URLs. An unescaped `@` in userinfo is not legal, but git
accepts it and people write it, so the scrubber has to be right for input that is wrong.
`internal/pipeline/clone_test.go` covers the email-username case and two credentialed URLs on one line, and
asserts the token is **absent** rather than that the output matches a string.

## Webhook authentication

`X-Hub-Signature-256`, HMAC-SHA256, verified with `hmac.Equal` **before the body is parsed**.

Both halves matter. The endpoint is reachable from the internet by design, so the JSON parser must never see
bytes that have not been authenticated. And the comparison must be constant-time, or it leaks the expected
digest one byte at a time.

A rejected delivery gets a bare `401 unauthorized`. Telling a prober *why* their signature failed helps only
the prober.

## Fork pull requests

Refused, at the webhook, before the payload is queued.

```text
WARN refusing to build a fork pull request pr=acme/docs#7 head_repo=mallory/docs
```

Building a fork means cloning an outsider's repository and executing its `package.json` scripts under an
installation token that can write to yours. There is no configuration flag for this and there should not be.

## Repository config

`.docpreview.yml` is attacker-influenced on any repository where opening a pull request is not a privilege.
Constraints:

- Paths are rejected if absolute, drive-qualified, or traversing outside the root.
- `build.command` is honored under **both** drivers, so under `local` it is a shell command on the build host. This
  is not a hole so much as a restatement of the driver's own posture: `npm run build` already runs `package.json`
  scripts from the same branch. Treat "who can open a pull request here" and "who I would give a shell to" as the
  same question under `local`, or run `docker`. A [project](./projects.md) row is how to state the command yourself.
- `build.env` cannot set reserved variables — a pull request that could set `DOCUSAURUS_BASE_URL` could break
  its own preview in a way that looks like a docpreview bug.
- `detect.script` is resolved through symlinks and re-checked against the workspace root, because a symlink
  inside an attacker-controlled tree can point anywhere.
- The detect script runs with a three-variable environment — `PATH`, `HOME` pointed at the workspace, and
  `DOCPREVIEW=1`. A build server's environment contains things a pull request author should not be handed.
- Under the Docker driver, **a build whose output contains a symlink is refused rather than published.** The preview
  file server blocks path traversal but follows symlinks out of its root, so `build/leak -> /etc/passwd` would
  otherwise be served to anyone holding the preview URL.

A [project row](./projects.md) overrides every one of the build fields above, which is the point of having one:
`.docpreview.yml` arrives in the pull request, and a project row is the operator's. It does not relax any of these
constraints — a project's `command` still runs, but the operator wrote it.

## The preview surface

Previews render HTML that anyone who can open a pull request wrote. Served with:

| Header | Why |
|---|---|
| `Cache-Control: no-store, must-revalidate` | A cached preview means a reviewer refreshes and sees the version they already rejected. |
| `X-Robots-Tag: noindex, nofollow` | Unreleased documentation has no business in a search index. |
| `X-Content-Type-Options: nosniff` | |
| `Referrer-Policy: no-referrer` | |

Only `GET` and `HEAD`; everything else is `405`. Path traversal is handled by `http.Dir`, which also rejects
the Windows device names and alternate separators a hand-crafted request might try.

These are hardening, not a WAF. For content you genuinely do not trust, put
[Frontdoor](../runbooks/frontdoor.md) in front.

## GitHub App permissions

Grant these and nothing else:

| Permission | Access |
|---|---|
| Contents | Read-only |
| Pull requests | Read and write |
| Checks | Read and write |
| Metadata | Read-only |

Install on **selected repositories**, not all of them.

Installation tokens last an hour and are cached in memory with a five-minute refresh margin. The App JWT
lasts nine minutes — GitHub's limit is ten — and is backdated sixty seconds against clock skew, because a host
running a minute fast otherwise fails every API call with a 401 that says nothing about clocks.

## Operational advice

**Set both passwords, first.** `docpreview console password -role admin` and `-role viewer`. Until the viewer one
exists nothing is gated, and `docpreview console status` says what is set and what this daemon listens on.

**Bind loopback, and do not share the daemon.** Default `listen` is `127.0.0.1:8471`. Publish
`docpreview webhook-only` and point the tunnel at that: a share of the daemon publishes the dashboard, every
preview and `/api/secrets` alongside the webhook. See the [webhook tunnel runbook](../runbooks/webhook-tunnel.md).
The login is a second line here, not a substitute — `dashboard-only` publishes a read-path allowlist that does not
include `/api/admin` at all.

**Rotate the private key.** See the [GitHub App runbook](../runbooks/github-app.md). Zero-downtime, and the
step people forget is deleting the old key afterwards.

**Keep the master key out of the config file.** `vault.key_source` names where to read it from — a command or a
file — so the key itself is never a config value. `exec:` is the form that keeps it out of every file on the
machine; see [the master key](#the-master-key).

**Close pull requests.** With `teardown_on_close: true` that withdraws the URL. Otherwise the TTL gets it in
72 hours.
