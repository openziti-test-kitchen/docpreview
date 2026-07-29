# Trust boundaries

What is defended, what is not, and why.

## The three sources of input

| Source | Trust | Consequence |
|---|---|---|
| Server config, vault | **Trusted.** An operator wrote it. | Can set anything, including what executes. |
| Webhook payloads | **Untrusted until verified.** | HMAC-checked, size-capped, rejected without explanation. |
| Repository contents | **Attacker-influenced.** Anyone who can open a pull request. | Every field validated; nothing in it chooses what runs on the host. |

The third is the one that shapes most of the code.

## The central decision: contributors are trusted, strangers are not

Under the `local` driver a build runs `package.json` scripts on the host with the daemon's privileges. That is
**fine** for a repository whose contributors you already trust — the case docpreview is built for — and
unacceptable for a public one.

Two things follow, and they hold each other up:

1. **Fork pull requests are refused** at the webhook layer (`internal/scm/github/webhook.go`). There is no flag
   for this.
2. **The `docker` driver exists** for repositories whose contributors are not fully trusted: a throwaway
   container, the workspace bind-mounted, 4 GB, 2 CPUs, no host environment.

Removing the fork refusal without switching to docker turns "anyone on the internet can open a pull request"
into "anyone on the internet can run code on this host". They are one decision, not two.

## What `.docpreview.yml` can and cannot do

It arrives from the pull request. Every field is validated.

| Field | Constraint |
|---|---|
| `build.dir`, `build.output` | Must not escape the repository root — checked for absolute paths, `..`, and Windows drive-qualified paths |
| `build.command` | Honored **only under the docker driver**; ignored under `local`, where it would be arbitrary host shell |
| `build.env` | Cannot set any reserved variable |
| `base_url` | Normalized; used for both build and serve so they cannot drift |
| `detect.script` | 60-second cap, five environment variables, path resolved through symlinks and re-checked |

The reserved variables are `DOCUSAURUS_BASE_URL`, `DOCPREVIEW_BASE_URL`, `DOCPREVIEW`, `DOCPREVIEW_COMMIT`,
`DOCPREVIEW_BRANCH`, `DOCPREVIEW_PR`, `DOCPREVIEW_REPO`, and **every operator secret's name**.

Each has a distinct reason:

- Base URL: a pull request that could set it could break its own preview in a way that looks like our bug.
- Commit stamp: a site that stamps its own commit is trusted to be telling the truth about which build it is. A
  forged one lets a stale preview claim to be current.
- Secret names: shadowing `ALGOLIA_WRITE_KEY` would let a pull request watch what the build does with a value
  **the redactor does not know about**.

The symlink re-check on `detect.script` is not theoretical: a symlink in a working tree the attacker controls
can point anywhere on the host.

## Credential handling

Covered in [05-secrets.md](05-secrets.md). The short form:

- Encrypted at rest with age, one file, opened lazily.
- `Secret` refuses to render itself through `Stringer`, `Formatter`, `GoStringer`, or `json.Marshaler`.
- Clone URLs carry tokens and never reach a log, an error, or a persisted git remote.
- Everything a build could print is scrubbed in six encodings before it touches disk.
- Git output is scrubbed by a *second* scrubber, because an installation token is minted per clone and the
  value-based redactor has never seen it (`internal/pipeline/clone.go:138`).

The last of those is the one with a worked failure behind it. `scrubLine` bounds the authority at the first `/`,
`?`, `#` or whitespace after `://` and takes the **last** `@` inside it — RFC 3986's definition of userinfo
(`internal/pipeline/clone.go:161`). It used to take the first, which meant a username containing an `@` (a
Bitbucket credential is an email address) got redacted while the token after it was emitted verbatim into the log
and from there into the pull request comment. If you touch this function, the property to preserve is that the
token is absent from the output, which is what `internal/pipeline/clone_test.go` asserts — not that the output
matches an expected string.

## The webhook endpoint

The only route that has to be reachable from outside — and the process that publishes it is
`docpreview webhook-only` (`cmd/docpreview/webhookonly.go`), not the daemon. See
[the credential surface](#the-credential-surface) for why that split exists; it is a security decision, not a
deployment convenience. Three defences at the endpoint itself:

1. **Size cap before reading.** The body must be fully read before it can be verified, so an unbounded read is
   an allocation primitive for anyone who can reach the port.
2. **HMAC-SHA256 with `hmac.Equal`**, not `==`.
3. **401 with no diagnostic.** Which part of the signature was wrong is a hint.

Responding **202 immediately** and doing the work asynchronously is also a defence of sorts: a slow build cannot
hold a connection open, and GitHub's ten-second budget cannot be exhausted.

## The credential surface

The dashboard has no authentication. That was acceptable while it was read-only; it now mutates the vault, and
this section is what replaced "there is no endpoint that mutates anything".

`/api/secrets` unlocks the vault, stores, deletes and generates. It is mounted on the same mux as the webhook, the
previews and the dashboard (`internal/daemon/ingress.go:135`), so its reachability is decided by the deployment
rather than by the code. Two independent gates run before any mutating handler
(`internal/daemon/secrets.go:375`), and neither substitutes for the other because they fail differently:

| Gate | Question | Catches |
|---|---|---|
| `Available` (`internal/daemon/secrets.go:130`) | Is every configured **listener** loopback? | a daemon bound to `0.0.0.0`, and any ziti listener |
| `isLocalRequest` (`internal/daemon/secrets.go:103`) | Did this **request** originate here? | a tunnel, which `Available` cannot see at all |

### The tunnel hole, and why it is architectural

`Available` was the whole gate, and it was insufficient in a way no listener check could fix.
`zrok2 share public http://127.0.0.1:8471` publishes every route the daemon serves while the listener is still
loopback — so the gate answered yes with `/api/secrets` on the internet, and with the vault unlocked `PUT`,
`DELETE` and generate would all have succeeded for anyone holding the share URL.

**No check inside the daemon can distinguish that case.** In zrok's proxy mode the daemon sees the connection
from the local tunnel process, so `RemoteAddr` is loopback; `Host` is client-supplied and therefore worthless.
The distinction does not exist at that layer, which is why the answer is not a smarter predicate but a second
process: `docpreview webhook-only` (`cmd/docpreview/webhookonly.go`) forwards only `POST /webhook/github` and the
tunnel points at that, so the credential API is not reachable through it at all.

Treat that as the primary defence. Anything that reintroduces "the daemon's own origin is what gets shared"
reopens the hole regardless of what the request-level check does.

### The two conditions in `isLocalRequest`

1. A loopback `RemoteAddr`. Every loopback form, not just `127.0.0.1` — `127.0.0.53` is what systemd-resolved
   uses. Note that over a zrok SDK listener `RemoteAddr` is `ziti-edge-router connId=…, logical=…`, not a
   `host:port` at all, so `SplitHostPort` fails, `ParseIP` fails, and the address test refuses it.
2. No `X-Forwarded-For`, `X-Forwarded-Host`, `X-Real-Ip` or `Forwarded` header. This is the one that closes the
   tunnel gap: anything proxying to the daemon sets one, including docpreview's own webhook-only proxy
   (`r.SetXForwarded()`, `cmd/docpreview/webhookonly.go:92`). A caller can add a header itself, which only makes
   the check stricter — the wrong answers are all in the refusing direction.

**Deliberate limit: this is a boundary, not authentication.** It asks where a request came from and answers from
evidence the network arrangement supplies, not from anything the caller proves. It assumes nothing proxies to the
daemon while stripping forwarding headers. On a loopback-only daemon that boundary is exactly the one already
protecting `docpreview vault set` — anyone who can reach `127.0.0.1` can run the binary — so the API adds no
reach a shell did not have. It is not a substitute for a credential, and it must not be described as one.

**Open: a password on the secrets surface.** Recorded in `TODO.md`. Managing credentials from anywhere but the
host needs a real credential of its own. Under a ziti listener the dialing identity is the natural hook —
`edge.ServiceConn.GetDialerIdentityId` reached through `http.Server.ConnContext` — and until that exists
`Available` refuses ziti listeners outright, because "enrolled at all" would otherwise be the whole
authorization for a credential write.

### The read path is deliberately not gated

`GET /api/secrets` answers from anywhere. It returns names and set/unset flags and never a value, and it reports
`can_write` with a `read_only_why` computed from the same two predicates the write path uses
(`internal/daemon/secrets.go:215`). So a remote dashboard renders the panel read-only with the reason on it.

The reasoning is worth keeping: hiding the panel makes a remote operator see a broken feature, and showing
buttons that 403 makes them see a bug. Mirroring the gate into the state the page renders is what avoids both, and
it is why `snapshot` takes the request — read-only-ness is a property of the caller, not of the daemon.

`generate` returns the value it just minted, once, in the response to the call that minted it. That is not a read
path: there is no way to ask for it again (`internal/daemon/secrets.go:321`).

## Serving preview content

Build output is arbitrary bytes produced by a tool nobody vetted, from a repository anyone with push access can
change.

- Build log downloads are served as **attachments** with `nosniff` and a declared `Content-Length`. Rendering
  them inline would let them be interpreted.
- Preview sites are served as static files with no-store caching. They are the artifact under review; serving
  them is the point.
- The dashboard escapes every interpolated value and scheme-checks every URL. Escaping closes attribute
  breakout but not `javascript:` or `data:`, which survive it intact.

## OpenZiti

The ziti exposer publishes previews onto an overlay reachable only from an enrolled identity. That is a stronger
boundary than a public URL with an unguessable name, and it is the differentiated part of this project.

**Open, and honest about it:** the current implementation uses one wildcard service with previews separated by
the HTTP `Host` header. The header is client-supplied, so anyone holding the `docpreview-reader` attribute can
reach every preview by sending any hostname. It is authorization at the network edge — you must be enrolled —
but not per preview. Options are recorded in `TODO.md`.

The admin listener names its own identity rather than reusing the exposer's. Hosting previews and hosting the
admin surface are different grants, and the admin surface is the more sensitive of the two since it enumerates
every open documentation pull request. Wiring them to one identity by default would make separating them later
a migration rather than an edit.

**No revocation story.** `configure ziti` bootstraps a network but there is no `docpreview identity remove`.
This is the largest known gap.

## What is deliberately not defended

- **A malicious operator.** Config and vault are trusted completely.
- **A proxy that strips forwarding headers.** `isLocalRequest` would read its requests as local. Nothing
  docpreview ships does this; something an operator puts in front of the daemon might.
- **A contributor with push access.** They can already run code in your CI; docpreview does not widen that.
- **Resource exhaustion by a trusted contributor.** A build that allocates 4 GB under the local driver will.
  Use docker.
- **A transformed secret.** Six encodings are matched. A build that hashes or fragments a credential defeats
  redaction.
- **Timing and traffic analysis on the overlay.**

## Invariants

1. Fork pull requests never reach the builder.
2. Repository config cannot choose what executes under the local driver.
3. Repository config cannot set a reserved or secret-named variable.
4. No repository-supplied path escapes the repository root.
5. Webhook bodies are capped before being read and verified before being acted on.
6. Verification failures reveal nothing.
7. Credentials are encrypted at rest and unrenderable in memory.
8. Build logs are scrubbed before reaching disk.
9. Every mutating endpoint on `/api/secrets` requires a loopback-only daemon **and** a request that originated on
   the machine. Adding one that checks only the first reopens the tunnel hole.
10. No endpoint reads a stored secret back. `generate` returns what it minted, in that response, and nowhere else.
11. Git output is redacted at the userinfo boundary RFC 3986 defines, not at the first `@`.
