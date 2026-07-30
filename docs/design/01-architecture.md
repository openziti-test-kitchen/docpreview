# Architecture

## What the system is

A push to a documentation branch produces a URL a reviewer can open, and one pull request comment that is edited
in place to carry it. Everything else is in service of that sentence.

```
webhook ──► queue ──► worker ──► clone ──► detect ──► build ──► COMMIT PHASE ──► comment
  (sqlite)              │                                          │
                        └── build log ──► disk + live subscribers   └── artifacts, exposer, database row
```

## The two seams

Everything portable about docpreview comes from two interfaces. Neither is speculative generality: each has
three or more real implementations that behave differently enough to have forced the shape.

### `scm.Client` — where pull requests come from

```go
Platform() model.Platform
VerifyWebhook(ctx, headers, body) ([]Event, error)
CloneURL(ctx, pr) (string, error)
ChangedFiles(ctx, pr) ([]string, error)
Publish(ctx, Report) error       // upsert the comment
Retract(ctx, pr) error
```

Implementations: `scm/github`, `scm/local` (bare repos plus a `post-receive` hook, for running the whole loop
with no network), and a stub for Bitbucket that returns 501.

`Publish` takes a `Report`, not a string. The client renders it. That is what keeps the comment format identical
across platforms and keeps `daemon` from knowing Markdown.

### `expose.Exposer` — where previews are served

```go
Kind() string
Validate(ctx) error
Publish(ctx, Spec, http.Handler) (*Publication, error)
Reap(ctx, keep map[string]bool) error
Close() error
```

**It takes an `http.Handler`, not a port.** This is the decision the rest of the package hangs off. zrok's SDK
returns a `net.Listener` on an OpenZiti overlay — a zrok preview never binds a local TCP port and never appears
in `netstat`. Demanding a port would have forced zrok into a worse shape to satisfy an abstraction only
Frontdoor needs. Frontdoor, whose agent dials *in* from outside, binds a real port internally; that is its
problem, not the interface's.

See [02-exposers.md](02-exposers.md).

## Package ownership

| Package | Owns | Must not |
|---|---|---|
| `model` | `Repo`, `PullRequest`, `PreviewID()`, `SanitizeName()` | import anything of ours |
| `config` | server config, repo config, validation | import `expose` (the dependency runs the other way) |
| `store` | sqlite: previews, jobs | know what a preview is *for* |
| `vault` | age-encrypted secrets, the `Secret` type | ever render a value |
| `redact` | scrubbing known values from text | know where the text came from |
| `pipeline` | clone, detect, build | publish anything |
| `preview` | serving a built directory as an `http.Handler` | know about exposers |
| `expose` | turning a handler into a URL | know about builds |
| `scm` | pull requests and comments | know about builds |
| `daemon` | wiring, the queue, the commit phase, HTTP ingress | contain logic that belongs in the above |
| `zitiadmin` | provisioning an OpenZiti network | be needed at runtime |

The rule that keeps this honest: **`daemon` is allowed to know about everything, and nothing is allowed to know
about `daemon`.** A helper that needs to reach back into the daemon belongs in the daemon.

## `PreviewID` — the identity of a preview

```go
sha256("platform|owner|repo|number")
```

Deliberately **excludes the branch and the commit**. A pull request that is force-pushed, renamed, or rebased is
the same pull request, and its preview must keep the same identity — otherwise a force-push orphans the
artifacts, the share, and the comment, and leaves a second preview beside the first.

Everything durable is keyed by it or prefixed with it: the database row, the artifact and log directories, the
package cache, the in-flight build map. Anything keyed by something *unrelated* to it — a branch, a name — is a bug
waiting to be found; see [02-exposers.md](02-exposers.md) for the four places it already was.

The one refinement is publication identity. A preview publishes more than once — a branch share plus one per build
— so the exposers key on `expose.Spec.Key()`, which is the preview id for the branch share and
`<preview>/<build>` for a build's. That is still derived from the preview id and nothing else, so the property
above holds; what changed is that "one preview" and "one publication" stopped being the same thing.

## The name, and how it differs from the ID

`PreviewID` is for machines and is stable. The **name** is for humans and appears in the URL:

```
name_template → RenderName → SanitizeName → "mydocs-new-install-guide"
```

Default `{{.Repo.Name}}-{{.Name}}`. The name must be unique per live publication, because every exposer keys a
public address on it. It is not required to be stable — but it is, in practice, because the default template
depends only on the repository and the branch. A build share's name is that name plus `-<sha7>`, appended rather
than re-rendered, so a template that separates repositories keeps separating them.

## Request flow, precisely

1. `POST /webhook/{platform}` → `Ingress.webhook` → `client.VerifyWebhook` → `[]scm.Event`
2. Respond **202 immediately**. GitHub gives a webhook ten seconds; the work does not fit in it.
3. `Daemon.Handle` per event. For `EventBuild`: cancel any in-flight build for this preview, report `queued`,
   `store.Enqueue` (which *replaces* any pending job for the same preview), nudge a worker.
4. A worker claims the oldest job (`DELETE ... RETURNING`, atomic in sqlite) and runs `Daemon.build`.
5. `runPipeline`: clone → load `.docpreview.yml` → changed files → detect → resolve the preview name and base
   URL → open a build log → build → **commit phase**.
6. The commit phase, under a per-preview lock, checks it is still the current build and then does the four
   irreversible things: replace the artifact directory, construct the site handler, publish, save the row.
7. `report` publishes the comment and records an event for the dashboard.

Steps 1–5 are reversible and can be abandoned at any point. Step 6 cannot, which is why it is one block with a
liveness check at the top. See [04-concurrency.md](04-concurrency.md).

## What the daemon holds in memory

| Field | Keyed by | Purpose |
|---|---|---|
| `live` | preview ID | the current `*expose.Publication`, so a rebuild can replace it |
| `running` | preview ID | the in-flight `*build`, so a newer push can cancel it |
| `commit` | preview ID | a mutex serializing the publish phase |
| `events` | — | a 200-entry ring of recent state changes, for the dashboard |
| `logs` | preview ID | live build-log writers and their subscribers |

None of it survives a restart, and none of it needs to: `recover` rebuilds `live` from the database, `running`
is empty by definition, and a lost in-flight build is recovered by pushing again.

## Startup

```
config → store → exposer → scm clients → validate → daemon → recover → listeners → serve
```

`validate` runs before anything binds. Discovering that the zrok environment is not enabled *after* the first
webhook arrives means a pull request comment that never appears and no obvious place to look.

`recover` reaps every remote object the exposer owns — nothing is serving yet, so all of it is an orphan from a
previous process — then republishes each recorded preview from its artifacts on disk. That restores working
URLs in a second or two without re-cloning or re-running a single `npm install`.

## Deliberate limits

- **One `.docpreview.yml` per repository**, so a monorepo with several documentation sites cannot preview the
  one a pull request touched.
- **Fork pull requests are refused** at the webhook layer. Building one runs a stranger's `package.json` under
  an installation token. There is no flag for this. See [10-security.md](10-security.md).
- **A lost build is not retried.** `Claim` deletes the job rather than marking it running, so a crash mid-build
  loses that build. The alternative is a row stuck in "running" forever after a hard kill, and telling those
  apart needs heartbeats nobody wants to operate. Pushing again fixes it.
