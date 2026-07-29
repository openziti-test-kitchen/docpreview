# Storage

sqlite via `modernc.org/sqlite` — pure Go, no cgo, so `go build` produces a static binary on any platform
without a C toolchain. That constraint comes from "must run ANYWHERE", and it is the reason this is not
`mattn/go-sqlite3`.

One file, `<data_dir>/docpreview.db`. Four tables: `previews`, `jobs`, `builds`, and `comments` — the last of
which exists only for the local platform, which has nowhere else to keep a comment
(`internal/store/store.go:94-109`, and [09-scm.md](09-scm.md) for the protocol it serves).

## Layout on disk

```
<data_dir>/
  docpreview.db          previews, jobs, builds, comments
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

`PendingJobs` returns `enqueued_at` alongside the payload (`internal/store/store.go:225`), so the caller gets a
`PendingJob{PR, EnqueuedAt}` rather than a bare pull request. The column was already there for `Claim`'s
ordering; carrying it out of the store is what lets a queued row on the dashboard show how long it has been
waiting. Without it the only timestamp available for a re-queued preview was the stored row's `updated_at` —
the age of its last *finished* build — so a job enqueued seconds ago rendered as hours old. See rule 6 in
[07-dashboard.md](07-dashboard.md).

## `builds`

| Column | Notes |
|---|---|
| `preview_id`, `build_id` | composite primary key |
| `platform`, `owner`, `repo`, `number`, `branch` | the pull request, exploded — this table is read for display |
| `commit_sha` | head SHA of the attempt |
| `state` | `building`, `ready` or `failed` |
| `reason` | failure summary, already scrubbed |
| `started_at`, `finished_at` | unix millis; `finished_at` is 0 while the build runs |

One row per build attempt (`internal/store/store.go:76`). It exists because `previews` cannot express this:
that table holds the **current** state of a preview and is overwritten on every rebuild, so the history of what
a branch had done lived nowhere. Two surfaces needed it — the dashboard's build picker, which could list stored
log files but say nothing about how each one ended, and the activity feed, which was an in-memory ring and so
empty after every restart. See [07-dashboard.md](07-dashboard.md).

**Written twice per build.** Once when the build starts, with `building` (`internal/daemon/daemon.go:914`), so a
build in flight appears in the history rather than materialising only when it ends — which is precisely when
somebody is watching. Once with its outcome (`daemon.go:944` and `daemon.go:952`). `SaveBuild` is an upsert on
`(preview_id, build_id)` that updates only `state`, `reason` and `finished_at`, so the second write cannot move
`started_at`.

**Keyed `(preview_id, build_id)`, not by commit.** The build id is `<timestamp>-<sha12>`
(`internal/daemon/daemon.go:203`), so one commit rebuilt twice — a retry, a reopened pull request — is two rows,
which is what somebody reading the history wants to see.

`d.saveBuild` (`daemon.go:456`) logs a failure rather than propagating it, and uses its own context rather than
the build's: it is called from paths whose context may already have been cancelled by a supersede, and a
superseded build's outcome is exactly what the history should record.

Only the three states above are recorded. `queued` is in `jobs` and nowhere else, and a skipped build never
reaches `saveBuild` — the dashboard's picker renders both anyway, because a row it cannot label is better than a
row it hides.

## Why JSON payloads rather than columns

`model.PullRequest` is carried whole as JSON in `previews` and `jobs` rather than exploded into columns.
`builds` explodes it, because that table is read to render a list and never to reconstruct a build.

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
3. `store.PruneBuilds` on the **same** `build.keep_logs` window (`internal/daemon/daemon.go:1208`), so a build
   row and its log file disappear together and the picker never offers an outcome whose log has gone. Deliberately
   not derived from the log files: a log can fail to open while the build still happened, and that build belongs
   in the history (`internal/store/store.go:334`).
4. `exposer.Reap(ctx, keep)` with the current preview IDs.

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
7. A build row is written when the build starts, not only when it ends.
8. Build rows and build logs are pruned on one window, so neither outlives the other.
