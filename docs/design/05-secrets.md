# Secrets and redaction

Two separate problems. **Storage**: credentials must not sit in plaintext on disk. **Egress**: a credential
handed to a build must not come back out in a log or a pull request comment.

The requirement, verbatim: *"if we configure the build with an env var, whatever that value is __MUST NOT__ show
up in logs regardless of how it's logged always replaced with five asterisks `*****` regardless of how long the
secret --actually-- is."*

## The vault

One file, `<data_dir>/vault.age`, encrypted with [age](https://age-encryption.org). Not a directory of files:
the set of key *names* is itself informative, and a directory leaks it through filenames even when every value
is encrypted.

The vault is opened **lazily, on first use**. A setup with the local exposer and no source-control integration
needs no secrets at all, and demanding a passphrase to serve a directory on loopback would be pure ceremony.

## The master key

The vault is a secrets manager, so its own key is the secret-zero problem in miniature: the thing that protects
everything else has nowhere inside the system to live. [OWASP's answer][owasp] is not to solve that but to move
it — "you will often have to secure the primary secret of that secrets management solution in a secondary
secrets management solution." `vault.key_source` is where that secondary mechanism plugs in.

[owasp]: https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html

Resolution order, strongest first:

| Source | Spelling | What it costs |
|---|---|---|
| A command | `key_source: "exec:op read op://ops/docpreview"` | the key exists in this process and nowhere else on the machine |
| A file | `key_source: "file:/etc/docpreview/master.key"` | anyone who can read that path can read every secret |
| The environment | `$DOCPREVIEW_MASTER_KEY` | readable by every process under the same user; lands in service definitions, process listings and crash dumps |
| A person | nothing configured | a human unlocks after every restart |

Whatever the source, the material is either an age X25519 secret key or, failing that, a passphrase (scrypt).
`docpreview vault keygen -out <path>` mints one straight into a file at 0600, so it never reaches a clipboard
or a shell history.

**Nothing configured is the default**, and it is a state rather than a failure: the daemon boots with a locked
vault, serves the dashboard, and is unlocked from there. That is the only configuration where the key is not at
rest anywhere — and the price is that a reboot leaves the daemon inert until somebody arrives. Both halves of
that trade are real, which is why there is a setting rather than an answer.

The environment variable is supported and no longer recommended. OWASP is blunt about why: environment
variables "are generally accessible to all processes and may be included in logs or system dumps", and using
them is "not recommended unless the other methods are not possible."

**A key file may not live inside `data_dir`.** Config validation refuses it. `data_dir` holds `vault.age`, so a
key beside it means one directory read yields both the ciphertext and the key that opens it, which is not
encryption at rest under any reading. Everything protecting a key file is its location and its permissions, so
a location that defeats that has to be a startup error and not a paragraph in a document like this one.

On Unix a key file readable by group or other is refused for the same reason. On Windows it is not: permissions
there are ACLs, which a mode bit does not describe — `os.Stat` reports 0666 for an ordinary file regardless of
who can open it — so the check would reject every key file for a reason that is not true.

The `exec:` form runs argv directly, never through a shell. The command comes from a config file, and handing a
config value to `sh -c` would turn "where is my key" into arbitrary command construction. Double quotes are
honoured so a path with a space in it can be one argument; there is no expansion, globbing, or operators. A
helper gets one minute, because `op read` can legitimately block on a biometric prompt and killing that after
five seconds would make the recommended configuration the one that does not work.

### The `Secret` type

```go
type Secret struct{ value []byte }
```

It implements `Stringer`, `Formatter`, `GoStringer` and `json.Marshaler`, and **all four return
`***REDACTED***`**. Reading the value requires calling `Reveal()` or `RevealString()` explicitly.

That is four interfaces because there are four ways a value leaks by accident: `fmt.Println(s)`, `%v`, `%#v`,
and being a field in a struct somebody serializes. Covering three of them would be worse than covering none,
because it would look safe.

It deliberately has **no `UnmarshalJSON`**. A secret should never arrive from a JSON document.

## Injection

```yaml
build:
  secrets:
    ALGOLIA_WRITE_KEY: "demo.algolia_key"     # env var name → vault key
```

`buildSecrets` in `cmd/docpreview/main.go` resolves the map at startup and passes it to
`Daemon.WithBuildSecrets`, which constructs the builder with `pipeline.NewBuilderWithSecrets`.

**A missing vault key fails startup**, naming the variable and the command that fixes it. The alternative is a
build that runs with the variable unset, produces a site missing whatever the credential was for, reports
success — and, worse, a redactor built from one fewer value than the operator believes.

### This was dead config for a while

`WithBuildSecrets` had no caller anywhere in the tree. The config key parsed, `docpreview init` wrote it out,
the documentation described it, and nothing read it. Builds ran with the variable unset and every log was
unredacted.

It was invisible because **an unredacted log looks exactly like a log with no secrets in it.** There is no
error, no warning, and no visible difference — the only way to notice is to configure a secret, print it
deliberately, and look.

Guarded now by `cmd/docpreview/secrets_test.go`, and by the demo, which prints the secret on every build.

### Scoped to a project

`build.secrets` is one map for the whole daemon, which is the wrong shape for the case that motivated it. A
documentation site assembling several private repositories needs a credential per source — a build script
dispatching on `process.env.BB_REPO_TOKEN_ONPREM` and falling back to SSH — and those credentials are not the same
for every project the daemon serves. A single global map means every project's build can read every other project's
tokens.

So a project can own environment variables of its own, at
`project/<platform>/<owner>/<repo>/<ENV>` in the same vault (`vault.ProjectSecretKey`, `internal/vault/vault.go:87`).
Four decisions in that:

- **A key prefix, not a second store.** The vault is one flat encrypted map, and the key is the only structure it
  has (`vault.ProjectPrefix`, `internal/vault/vault.go:74`). A nested format would change the on-disk shape of every
  existing vault to express what a prefix already can.
- **The slash is the separator, and that is load-bearing.** A global key is validated as letters, digits, dot, dash
  and underscore, so `PUT /api/secrets/{key}` cannot write into the namespace and a project's variables cannot
  shadow `github.private_key`. Relaxing `validKey` removes the only thing keeping the scopes apart —
  `TestGlobalSecretKeyCannotReachTheProjectNamespace` is what stops that happening quietly.
- **Routed under the project**, at `PUT /api/projects/{platform}/{owner}/{repo}/secrets/{env}` and the matching
  `DELETE` (`internal/daemon/projects.go:79-80`), so the scope is expressed by the URL and the vault key is composed
  server-side from path values already validated as a project identity. Both go through the same `gated` pair of
  checks as a global credential write, for the reason in "Two gates" below: a project's variables are credentials in
  the process that runs its build.
- **Resolved per build, not cached.** `Daemon.SetProjectSecrets` takes a function, not a map
  (`internal/daemon/daemon.go:66`). The vault may be locked at startup, and a token added from the projects page then
  applies to the next build with no rearm and no restart — which matters because the operator adding it is usually
  looking at the build that just failed without it. Listing a project's variables is a prefix scan of `Keys()`, which
  is why the variable name is the *last* segment: an owner or a repository may contain a dot or a dash, so splitting
  on those would be ambiguous.

The name must match `[A-Z_][A-Z0-9_]*` and be at most 128 characters (`validEnvName`,
`internal/daemon/projects.go:429`). Stricter than a vault key, because this one is not a lookup key: it becomes a
variable in a process running a build script, and a name with a dot or a dash is settable through `exec.Cmd` and
unreadable from `sh` — so the operator would store a value the build could never see. Upper case is not a shell
requirement; it is the convention every build script this exists for already follows, and refusing the alternative
is cheaper than supporting two spellings of one name.

**`Builder.WithSecrets` merges and rebuilds the redactor in one call**, and the two must never be separable. A
builder copied with a larger secrets map and the base redactor injects a credential the scrubber was never told
about, and the first `set -x` puts it in a pull request comment. It also copies rather than mutates, because two
projects build concurrently from one base builder — mutation would hand whichever built second the other's tokens,
which is the failure the scoping exists to prevent. Both are tested in
`internal/pipeline/projectsecrets_test.go`.

Because the merged map is what `buildEnv` reserves against `.docpreview.yml`, a project's variables inherit
invariant 6 for free: a pull request cannot shadow one.

## Redaction

`internal/redact` is built from the **values**, not the names. A build that prints its own environment — which
npm does on failure, and any script under `set -x` does always — therefore produces asterisks.

```go
r, tooShort := redact.New(values)
```

Six encodings are matched for each value:

| Encoding | Example |
|---|---|
| raw | `dpfake_9f2a…` |
| URL query | `dpfake_9f2a…` percent-encoded |
| URL path | the path-escaped form, which differs |
| JSON-escaped | as it appears inside a JSON string |
| base64 std | `Authorization: Basic …` |
| base64 URL | the URL-safe alphabet |
| base64 raw | unpadded |

Patterns are ordered **longest first**. Otherwise a secret that is a prefix of another is replaced inside it,
leaving the tail of the longer one exposed: registering both `abcd` and `abcd1234` must not turn the latter into
`*****1234`.

`Mask` is exactly `*****` — five asterisks, regardless of the real length. A mask whose width tracked the secret
would leak the length.

`minLength` is 4. Anything shorter is refused, with a count logged and **not** the name — the name is the lookup
key into the vault. A three-character secret would match half the English in a build log.

`ScrubError` preserves the error chain, so `errors.Is` still works on a scrubbed error.

### Redaction happens before anything durable exists

`buildlog.Writer.emit` is the only path from build output to disk or to a subscriber, and it scrubs there. A
file that contains a secret for even a moment is a file that can be read, backed up, or swept into a crash dump.

Redaction operates on text, which needs whole lines: a secret split across two `Write` calls is invisible to a
scrubber looking at each call in isolation. So the writer buffers by line. See
[06-build-logs.md](06-build-logs.md).

### A second scrubber, for a credential the redactor never sees

`internal/redact` is built from the values docpreview was *given*. A GitHub installation token is not one of
those: it is minted per clone, lives for an hour, and never appears in config or in the vault. Git echoes the
remote URL — token and all — into several of its error messages, so `pipeline.scrub` redacts URL userinfo in git
output before it becomes an error string (`internal/pipeline/clone.go:138`). Build output is attached to the pull
request comment, so the failure mode is a live credential in the pull request.

**This leaked exactly what it existed to hide.** It split the authority on the **first** `@`. A Bitbucket app
password is authenticated with an email address as the username, so:

```
https://someone@example.com:TOKEN@bitbucket.org/ws/docs.git
```

redacted `someone`, and emitted `:TOKEN@bitbucket.org/ws/docs.git` verbatim — into the build log, and from there
into the comment. The one input a URL scrubber has to survive is the one it was written without thinking about.

The rule now is RFC 3986's (`internal/pipeline/clone.go:161`): after `://`, the authority ends at the first `/`,
`?`, `#` **or whitespace**, and userinfo is everything before the **last** `@` inside it. Whitespace because these
are prose lines with URLs embedded in them, not bare URLs. An unescaped `@` in userinfo is illegal, but git
accepts it and people write it, so the scrubber has to be correct for input that is not.

Guarded by `internal/pipeline/clone_test.go`, which asserts the token is absent — not that the output matches a
string — over the email-username case, two credentialed URLs on one line, and a line already containing
`***REDACTED***@`, which an earlier walk-and-replace version turned into an infinite loop.

This is a **worked example of why this section exists**: the scrubber was correct on every URL anyone tested it
with, and the encoding it got wrong was the one a real provider uses.

## Proving it

`demo/Test-Redaction.ps1` stands up an isolated daemon with its own vault, runs a build that prints a real
secret seven ways — raw, URL-encoded, JSON, base64, inside a URL, as an `Authorization` header, and **split
across two writes with no newline between them** — then greps the entire data directory. Non-zero exit if it
finds anything.

The main demo also prints it on every build, so the dashboard always has a line of asterisks in it:

```
preflight: search key   *****
preflight: index url    https://user:*****@algolia.net/1/indexes
preflight: auth header  Authorization: Basic *****
```

That exists because working redaction and absent redaction are indistinguishable without it.

## Deliberate limits

- **A transformed secret survives.** Six encodings are matched and nothing else. A build that hashes, encrypts,
  or prints a credential in fragments defeats it. This is a last line of defence over output docpreview does not
  control, not a guarantee.
- **A secret straddling the 64 KiB flush boundary survives.** Lines longer than the cap are flushed in exact
  chunks rather than buffered without limit, and each chunk is scrubbed — but a secret spanning exactly that
  split is missed.
- **`build.secrets` is still global.** One map from the server config, applied to every build. A project's *own*
  variables are scoped — see "Scoped to a project" above — but anything in `build.secrets` reaches every build on
  the daemon, so a credential that belongs to one repository belongs in that project's variables instead. A
  per-provider scope is still wanted; see `TODO.md`.

## The admin surface

`SecretsAdmin` (`internal/daemon/secrets.go`) sets, generates and deletes vault entries from the browser, served
at `/secrets`. It exists because the alternative was a three-command terminal ceremony in the right order — mint a
master key, remember to persist it, pipe a value into `vault set` — whose failure mode is a daemon that starts and
then cannot open its own vault.

**A credential-write endpoint on an unauthenticated surface is worse than no UI**, and the dashboard has no
authentication. That has not changed; it is why the surface is gated rather than simply added. On a loopback-only
daemon the boundary is the one that already protects `docpreview vault set` — anyone who can reach `127.0.0.1` can
run the binary — so this adds no reachability a shell did not have. The moment a listener is not loopback, it
would, and it refuses to serve.

**It never returns a value.** Not on list, not in an error. The state is names, labels and set/missing flags
(`internal/daemon/secrets.go:183`). The single exception is `POST /api/secrets/{key}/generate`, which returns what
it just minted in the response to the call that minted it, once, with no way to ask again
(`internal/daemon/secrets.go:321`). That is not a read. It exists because a webhook secret has to be identical in
two places — GitHub's form and this vault — and a UI that can only accept values makes "paste the value you
generated earlier" answerable only with "from where?".

**Every write re-arms.** `rearm(changed)` re-resolves everything derived from vault contents: the redactor, which
is compiled from the values, and the GitHub client, which reads its private key and webhook secret at
construction (`internal/daemon/secrets.go:49`). A secret changed at runtime that does not rebuild the redactor is
a secret that appears verbatim in the next build log. The argument is the key that changed, empty when the vault
itself was just unlocked and every key is new at once.

### Two gates, and the gap between them

Writes pass through `gated`, which requires both (`internal/daemon/secrets.go:375`):

| Check | Asks | Catches |
|---|---|---|
| `Available` | where the daemon *listens* | a listener bound to `0.0.0.0`, and a ziti listener |
| `isLocalRequest` | where this request *came from* | a tunnel, which `Available` cannot see at all |

Neither substitutes for the other. `Available` is a property of configuration, checked once per call against
`cfg.Listeners`; `zrok2 share public http://127.0.0.1:8471` makes every route internet-reachable while every
listener is still loopback, so `Available` says yes and the credential API is on the internet
(`internal/daemon/secrets.go:80`).

`isLocalRequest` closes that with two conditions. `RemoteAddr` must be loopback — under a tunnel it is, since the
daemon sees the connection from the local tunnel process, so this alone proves nothing. And the request must carry
no forwarding header: anything proxying to the daemon sets `X-Forwarded-For` or `Forwarded`, including
docpreview's own `webhook-only` proxy. A caller can add one itself, but that only makes the check stricter, which
is the right direction to be wrong in.

A tunnel that strips forwarding headers defeats this, which is why the recommended arrangement is to tunnel the
`webhook-only` proxy rather than the daemon: then the credential API is not reachable at all and this check is a
second line rather than the only one. The trust boundaries are in [10-security.md](10-security.md).

A ziti listener is refused outright even though the overlay authenticates the dialer, because the admin surface
does not yet check the dialing identity — "enrolled at all" would be the whole authorization, which is not enough
for credential writes (`internal/daemon/secrets.go:130`).

**The read path is deliberately not gated.** It returns no values, and a page that can explain why it is
read-only is better than one that 404s. What it reports instead is `can_write` and `read_only_why`, which the page
renders as a read-only panel; see [07-dashboard.md](07-dashboard.md).

Guarded by `internal/daemon/secrets_test.go`: non-loopback listener, ziti listener, remote address, forwarded
header, locked vault, and a round trip asserting no response body ever carries a stored value.

### Still open

- **Audit who set what, when.** Not who read it — nobody can.
- **Per-identity authorization under ziti**, which is what would let the surface exist on the overlay at all.

## Invariants

1. `Secret` never renders its value through any standard interface.
2. A configured `build.secrets` key that is missing from the vault fails startup.
3. The redactor is built from values, and every value reaching a build is in it.
4. Nothing unredacted is written to disk or handed to a subscriber.
5. The mask is fixed-width.
6. A repository cannot shadow an operator secret's variable name, whether it is server-wide or the project's own.
7. No API response carries a stored secret value; `generate` returns only what that call created.
8. Every write to the vault re-arms the redactor and anything else built from vault contents.
9. A mutating credential call requires both a loopback-only daemon and a local, unforwarded request. That includes
   a project's own variables, which are credentials in a process that runs a build.
10. A credential in git's own output is redacted before it becomes an error string.
11. A project's secret is separated from a global one by a character no global key may contain.
12. Injecting a value and adding it to the redactor are one operation, never two.
