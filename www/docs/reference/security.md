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

**Not defended against:**

- Someone already running code as the docpreview user. They have won; no file format changes that.
- Anything that proxies to the daemon while stripping forwarding headers. The locality gate below would read
  those requests as local. Nothing docpreview ships does this; a proxy you put in front of it might.
- Managing credentials from anywhere but the host. There is no password on `/api/secrets`, so a remote caller
  gets a read-only view and nothing else. That is a missing feature, not a defence.
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

## The credential API

The dashboard can create the vault, unlock it, store a credential, delete one, and generate a webhook secret.
Those endpoints live under `/api/secrets`, on the same listener that serves the dashboard, the previews and the
webhook — so how reachable they are is a property of your deployment, not of the code. Two conditions must both
hold before any of them will act (`internal/daemon/secrets.go:375`):

- **Every configured listener is loopback.** A daemon bound to `0.0.0.0` refuses credential writes, and so does
  one serving over an OpenZiti listener: the admin surface does not yet check the dialing identity, so "enrolled
  at all" would be the whole authorization (`internal/daemon/secrets.go:130`).
- **The request itself arrived locally** — a loopback `RemoteAddr` **and** no `X-Forwarded-For`,
  `X-Forwarded-Host`, `X-Real-Ip` or `Forwarded` header (`internal/daemon/secrets.go:103`).

### Why the second condition exists

Checking where the daemon listens is not enough, and the gap is a tunnel. `zrok2 share public
http://127.0.0.1:8471` publishes every route the daemon serves while the listener is still loopback, so the first
condition answered yes with `/api/secrets` on the internet — and with the vault unlocked, `PUT`, `DELETE` and
generate would all have succeeded for anyone holding the share URL.

`RemoteAddr` alone cannot tell the difference. In zrok's proxy mode the daemon sees the connection from the local
tunnel process, so the address is loopback too, and `Host` is whatever the client sent. The forwarding-header test
is what closes it: anything that proxies to the daemon sets one of those headers, including docpreview's own
`webhook-only` proxy. A caller that adds a header itself only makes the check refuse more, which is the safe
direction to be wrong in.

### This is a boundary, not authentication

Nothing here is a credential check. The gate answers "did this request originate on this machine", and it answers
it from evidence the network arrangement supplies rather than from anything the caller proves. It assumes nothing
proxies to the daemon while stripping forwarding headers. A real password on the credential surface is still
outstanding — until it exists, managing credentials from another machine is not supported rather than merely
inadvisable.

On a loopback-only daemon the boundary is the same one that already protects `docpreview vault set`: anyone who
can reach `127.0.0.1` can run the binary. The API adds no reach that a shell did not already have.

### Point the tunnel at `webhook-only`, not the daemon

The architectural half of the fix, and the one that matters more than the header check. `docpreview webhook-only`
forwards exactly `POST /webhook/github` and answers 404 to everything else, so the credential API is not
reachable through the tunnel at all and the locality gate becomes a second line rather than the only one. With
`-zrok-name` it binds no local TCP port either. See the [webhook tunnel runbook](../runbooks/webhook-tunnel.md).

### Reading is deliberately open

`GET /api/secrets` is not gated. It returns names and set/unset flags, never a value, and it reports `can_write`
plus `read_only_why` so a remote dashboard renders the panel read-only with the reason on it. A panel that
vanished instead would read as a broken feature, and a panel offering buttons that 403 is worse than one that
explains itself.

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
- `build.command` is ignored under the `local` driver. Under `docker` it is honored, because the blast radius
  is a container.
- `build.env` cannot set reserved variables — a pull request that could set `DOCUSAURUS_BASE_URL` could break
  its own preview in a way that looks like a docpreview bug.
- `detect.script` is resolved through symlinks and re-checked against the workspace root, because a symlink
  inside an attacker-controlled tree can point anywhere.
- The detect script runs with a three-variable environment. A build server's environment contains things a
  pull request author should not be handed.

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

**Bind loopback, and do not share the daemon.** Default `listen` is `127.0.0.1:8471`. Publish
`docpreview webhook-only` and point the tunnel at that: a share of the daemon publishes the dashboard, every
preview and `/api/secrets` alongside the webhook. See the [webhook tunnel runbook](../runbooks/webhook-tunnel.md).

**Rotate the private key.** See the [GitHub App runbook](../runbooks/github-app.md). Zero-downtime, and the
step people forget is deleting the old key afterwards.

**Keep the master key out of the config file.** `vault.key_source` names where to read it from — a command or a
file — so the key itself is never a config value. `exec:` is the form that keeps it out of every file on the
machine; see [the master key](#the-master-key).

**Close pull requests.** With `teardown_on_close: true` that withdraws the URL. Otherwise the TTL gets it in
72 hours.
