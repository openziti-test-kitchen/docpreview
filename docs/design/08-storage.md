# Storage

sqlite via `modernc.org/sqlite` — pure Go, no cgo, so `go build` produces a static binary on any platform
without a C toolchain. That constraint comes from "must run ANYWHERE", and it is the reason this is not
`mattn/go-sqlite3`.

One file, `<data_dir>/docpreview.db`. Two tables.

## Layout on disk

```
<data_dir>/
  docpreview.db          previews, jobs
  vault.age              secrets
  workspaces/<id>/       scratch clones, deleted when output is copied out
  artifacts/<id>/        what is being served
  logs/<id>/<build>.log  build output
```

Everything except the vault is keyed by preview ID, and all of it is reconstructible: the workspaces from git,
the artifacts by rebuilding, the logs not at all — but a lost log costs nothing durable.

## `previews`

| Column | Notes |
|---|---|
| `preview_id` | primary key, `sha256(platform\|owner\|repo\|number)` |
| `payload` | the `model.PullRequest`, JSON |
| `name` | the label the exposer actually used |
| `url` | what the comment links to |
| `base_url` | what the site was built with |
| `artifact_dir` | what is being served |
| `commit` | head SHA at build time |
| `state` | `ready`, `failed`, `skipped`, `torn-down` |
| `reason` | failure summary, already scrubbed |
| `updated_at` | drives TTL and the dashboard's "ago" |

**The table holds committed states only.** A row is written when a build succeeds; `queued` and `building` never
appear in it, and `Daemon.Status` composes those from the job queue and the running map at read time. See
[04-concurrency.md](04-concurrency.md).

`base_url` is stored because recovery has to reconstruct the handler with the same value the site was built
with. Recomputing it from config would produce a different answer if the config changed between runs, and the
site would 404 every asset.

## `jobs`

| Column | Notes |
|---|---|
| `preview_id` | primary key — **at most one pending job per pull request** |
| `payload` | the `model.PullRequest`, JSON |
| `enqueued_at` | unix millis, the claim order |

The primary key is the whole design. `Enqueue` is an upsert, so a reviewer pushing five commits in two minutes
leaves one job holding the fifth.

`Claim` is `DELETE ... RETURNING`, atomic in sqlite, so two workers cannot both take the same job.

`PendingCount` answers "how many" for a header. `PendingJobs` returns the rows, because a dashboard that lets
you click the number and see which ones needs them — and a queued build has no preview record yet: the first
build of a branch exists nowhere but here until it finishes.

## Why JSON payloads rather than columns

`model.PullRequest` is carried whole as JSON in both tables rather than exploded into columns.

It is written by one program and read by the same program; nothing queries by owner or branch. Exploding it
would mean a migration every time the struct gains a field, in exchange for query capability nobody uses. The
fields that *are* queried — preview id, state, updated_at, enqueued_at — are real columns.

## Recovery

At startup, in this order:

1. **Reap everything remote.** `exposer.Reap(ctx, nil)`. Nothing is serving yet — the process just started — so
   every share the exposer owns is an orphan.
2. **Republish each recorded preview** from its artifacts on disk.

That restores working URLs within a second or two of startup without re-cloning or re-running a single
`npm install`. It is why a restart is cheap enough to do casually.

The order matters and is the reverse of the intuitive one. Reaping *after* republishing would delete what was
just restored.

Only `ready` rows are republished. A `failed` row describes a build that produced nothing; a `torn-down` one
describes a preview that was deliberately removed.

If the artifact directory is gone, the row is dropped rather than republished — there is nothing to serve, and
leaving the row would advertise a URL that 404s.

### The URL can move

Under exposers whose address is not derived from the name, republishing yields a different URL. When it does,
the row is rewritten and a fresh `ready` report is published, because a comment pointing at a dead URL is worse
than no comment.

This used to matter constantly: the local exposer allocated an ephemeral port per preview, so *every* URL moved
on *every* restart. It now serves previews on a path derived from the name, which is stable — see
[02-exposers.md](02-exposers.md).

## Sweeping

The reaper ticks hourly. Preview TTLs are measured in days, so anything finer is wasted wakeups, and anything
coarser leaves a dead preview linked from a comment for most of a working day.

Each tick:

1. Tear down previews older than `preview.ttl`.
2. `logs.Sweep(build.keep_logs)`. Build logs outlive their previews only until the retention window closes —
   they can contain anything a build printed, so an unbounded pile is a liability rather than an asset.
3. `exposer.Reap(ctx, keep)` with the current preview IDs.

## Path safety

Preview IDs arrive from URL paths (`/logs/{preview}/download`) and become directory names. Two defences, either
sufficient:

- `net/http`'s mux cleans the path before routing, so `..` redirects rather than reaching the handler.
- `Store.previewDir` rejects anything containing a separator or a dot.

The test asserts *no log content is ever served* rather than a specific status code, because pinning the status
would make it fail if the mux's normalization changed — which would be a false alarm.

## Invariants

1. At most one pending job per preview.
2. `Claim` cannot hand the same job to two workers.
3. The preview table holds committed states only.
4. Recovery reaps before it republishes.
5. `base_url` is stored, not recomputed.
6. A preview ID from a URL cannot escape its directory.
