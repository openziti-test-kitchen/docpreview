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
  docpreview.db                  previews, jobs, builds, comments
  vault.age                      secrets
  workspaces/<id>/<sha12>/       scratch clones, one per commit, siblings pruned best-effort
  artifacts/<id>/<build-id>/     what is being served, one directory per build
  logs/<id>/<build>.log          build output
  cache/<id>/{npm,yarn,pnpm}/    package manager caches, when cache_dir is unset
```

Everything except the vault is keyed by preview ID, and all of it is reconstructible: the workspaces from git,
the artifacts by rebuilding, the caches by refetching, the logs not at all — but a lost log costs nothing durable.

The two per-build directories are keyed differently and that is not an oversight. A workspace is named by the
**commit** (`buildDirName(pr.HeadSHA)`, `internal/pipeline/clone.go:82`), so that two builds of *different* commits
never share a directory — a supersede's loser is still unwinding while the winner clones, and sharing one let the
loser's cleanup delete files under the winner. Rebuilding the *same* commit reuses the name and wipes it first, since
a leftover tree would leave deleted pages in the preview. An artifact
directory is named by the **build id**, which carries a timestamp as well, because one commit rebuilt twice is two
outcomes and the history has to be able to serve either.

The cache root is `build.cache_dir` when set and `<data_dir>/cache` otherwise (`config.CacheRoot`), because that
one is the directory an operator moves to a disk with room. It is keyed on the preview for the same reason
everything above is, and one more: a preview ID is a hex digest, so nothing arriving from a webhook is ever joined
onto that path — see [03-build-pipeline.md](03-build-pipeline.md).

## `previews`

| Column | Notes |
|---|---|
| `preview_id` | primary key, `sha256(platform\|owner\|repo\|number)` — and `\|branch` appended when `number` is 0, see below |
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

### Number 0 means "no pull request", and the branch becomes the identity

A branch preview — the permanent preview of a repository's default branch — has no pull request. No platform
numbers a pull request zero, so the zero value of the field is free to mean that, and `model.PullRequest.IsBranch()`
is the one predicate everything else turns on.

The id has to fold in the branch for that case, because every branch of one repository would otherwise hash to the
same value and the second branch built would take over the first one's row, share and artifacts.

:::danger The numbered form must never change

This id is the primary key here, the foreign key in `builds` and `comments`, the tag on every remote share, and the
directory name of every artifact and log. Adding the branch to the hashed input for *numbered* pull requests as
well would silently orphan all of it: every restored preview reaped as an orphan on the next sweep, and a duplicate
comment posted on every open pull request. `TestPreviewIDIsStableForPullRequests` pins the numbered form with a
literal for exactly that reason.

:::

Three consequences elsewhere, each enforced where it happens rather than by a flag threaded through the build path:
nothing is reported to the platform (`Daemon.report` returns early, and `teardown` skips `Retract`), the
changed-file gate is skipped because there is no diff to take, and the TTL reaper leaves the row alone because
`main` is still `main` after a quiet fortnight.

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
| `name`, `url` | this build's own share, empty when it never had one |

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

### `name` and `url`: a build's own share

Per-build publishing gives each build a URL pinned to the commit it was built from, beside the branch share the
pull request comment links to ([02-exposers.md](02-exposers.md)). Those two columns are what make it survive a
restart, and they had to exist before the feature could ship for two separate reasons:

1. **Startup reaps with an empty keep-set** and then republished *previews* only, so every build URL 404'd after a
   restart while `builds.url` went on advertising it. `restoreBuildShares` (`internal/daemon/daemon.go:529`) reads
   these rows and republishes each build's share from `artifacts/<preview>/<build>/`. Reported as "that should
   still be there", and it should have been.
2. **The hourly reap's keep-set is spelled in publication keys**, so it has to name each build share as well as
   each preview, or the sweep deletes every build share minutes after it was published
   (`daemon.go:1770`).

`ClearBuildShare` (`internal/store/store.go:561`) is the third case: a build whose artifacts `keep_builds` has
already pruned. Its share is gone and nothing can republish it, so the two columns are set to empty **and the row
stays** — the history happened, and deleting it to express "the link is dead" would lose the outcome as well. An
`UPDATE` of two columns rather than a `DELETE`. Leaving the URL in place would keep the dashboard offering a link
to something no longer on disk, which is the same failure in slower motion.

### The first schema migration

`CREATE TABLE IF NOT EXISTS` does nothing at all to a table that already exists, so `name` and `url` appear only in
databases created after they were added — every existing one silently lacks them and every query naming one fails.
`migrate` (`internal/store/store.go:199`) is one list of `ALTER TABLE ... ADD COLUMN` statements applied on every
`Open`, and both columns are declared **twice**: in `schema` for a new database and in `migrate` for an old one.

There is no `schema_version` table, so each statement has to be idempotent by itself. `ADD COLUMN` is not, so
"duplicate column name" is treated as the success it is — which means the check is on sqlite's error *text*, and a
future release that rewords it would make these look failed. That is the accepted trade for having no versioning at
the first migration that can be expressed additively; the version table is worth adding at the first one that
cannot. Additive only: a column that has to change type or go away needs a real migration, and running one from
here would run it again on every restart. `internal/store/migrate_test.go` opens an old-shaped database, asserts a
pre-existing row is undisturbed, and opens it a second time to prove the migration is not a one-shot.

## Why JSON payloads rather than columns

`model.PullRequest` is carried whole as JSON in `previews` and `jobs` rather than exploded into columns.
`builds` explodes it, because that table is read to render a list and never to reconstruct a build.

It is written by one program and read by the same program; nothing queries by owner or branch. Exploding it
would mean a migration every time the struct gains a field, in exchange for query capability nobody uses. The
fields that *are* queried — preview id, state, updated_at, enqueued_at — are real columns.

## Recovery

At startup, in this order:

1. **Decide what should exist.** Every `ready` preview with artifacts on disk, plus every build row that recorded a
   URL and still has artifacts. That set is the keep-set, and it is assembled before anything is deleted or
   published.
2. **Ask the exposer what it already has.** `Adoptable(ctx)`, keyed by publication key. Optional: an exposer that
   cannot answer means step 4 creates everything, which is what this used to do unconditionally.
3. **Reap what nothing claims.** `exposer.Reap(ctx, keep)`.
4. **Restore each publication**, adopting where the exposer already holds one and creating where it does not, then
   the same for each build share (`restoreBuildShares`). A build whose artifacts are gone has its two share columns
   cleared instead.

That restores working URLs in seconds without re-cloning or re-running a single `npm install`. It is why a restart
is cheap enough to do casually.

The order matters and is the reverse of the intuitive one. Reaping *after* republishing would delete what was
just restored.

### Why the keep-set, and not `Reap(ctx, nil)`

This used to reap with an empty keep-set, on the reasoning that nothing serving can have survived the process that
served it. Half of that is true. The **overlay listener** dies with the process; the **share** does not — it lives
on the controller, holds its name, and can be bound by a new listener through its token.

Deleting them all therefore paid twice for nothing. Measured on 2026-07-30 against the hosted zrok controller, for
two pull requests and thirteen publications: **85 seconds** to delete them, then **183 seconds** to create thirteen
identical ones, with every preview URL 404ing throughout and no build able to start. The same restart with adoption
takes **3 seconds**.

So the rule is now: delete only what the database no longer claims — a torn-down preview, a pruned build, a share
left behind by a create that timed out after succeeding — and bind a listener to everything else. A failed adoption
falls through to `Publish`, which replaces whatever holds the name, so the fallback is also the cleanup.

Only `ready` rows are republished. A `failed` row describes a build that produced nothing; a `torn-down` one
describes a preview that was deliberately removed.

If the artifact directory is gone, the row is dropped rather than republished — there is nothing to serve, and
leaving the row would advertise a URL that 404s. Artifacts that are *present but do not match the stored
`base_url`* are dropped for the same reason (`errArtifactsUnusable`, `internal/daemon/daemon.go:466`): see
[03-build-pipeline.md](03-build-pipeline.md).

### A republish that fails

Any *other* failure — the publish itself, which on a hosted controller means a timeout — leaves artifacts that are
still good and a share that was never created. The row is therefore **marked, not deleted**: `store.FailPreview`
empties `url`, sets `failed`, and records a reason naming Rebuild, and `Daemon.markUnpublished` sends a matching
failed report so the pull request comment stops offering the link.

Emptying the URL is the load-bearing half. The dashboard's Open button is enabled by the presence of a URL rather
than by the state — a rebuild leaves the previous share serving, so a state test would grey the button on every
rebuild — which means a row that keeps its URL keeps offering it however the state reads. Seen live on 2026-07-30:
`CreateShare` timed out at startup and `/status` reported `state: ready` with a URL answering 502 for the rest of
the day.

`name` is deliberately left on the row. Teardown reads it to give the exposer's reserved name back, and on zrok
that name is a quota-bearing object, so clearing it here would leak one per failed restore with nothing left to
name it.

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
4. `exposer.Reap(ctx, keep)` with the current publication keys — every preview id, plus `<preview>/<build>` for
   each build row that recorded a URL. If any part of that set cannot be read, the sweep is **skipped** rather than
   run on what was read: an incomplete keep-set does not under-delete, it deletes live shares
   (`internal/daemon/daemon.go:1770`).

## What teardown removes, and what it leaves

`Daemon.teardown` (`internal/daemon/daemon.go:880`) runs when a pull request closes, when `preview.ttl` expires, and
from the dashboard. It holds the preview's commit lock for the whole of it, because a build already inside its
publishing phase would otherwise finish and reinstall the preview that was just removed — leaving a live share and
no database row.

Removed, in this order:

| | |
|---|---|
| the exposer's *names* | first, before the shares — see below |
| the branch publication, and every build publication under `<preview>/` | |
| `artifacts/<preview>/` | every build's output, not just the newest |
| `workspaces/<preview>/` | |
| the preview's cache directory | `PreviewCacheDir`, when a cache root is configured |
| the build logs | `logs.Remove` |
| the pull request comment | `client.Retract` |
| the `previews` row | `DeletePreview` |

**Names go before shares, and that ordering is deliberate.** On an exposer whose names are quota-bearing objects
with their own lifetime — zrok, today — de-reserving first makes a crash mid-teardown self-healing, because the next
startup's `Reap` deletes the share and the platform collects the non-reserved name with it. Reversed, a crash
between the two leaks the name silently and forever. See [02-exposers.md](02-exposers.md).

**Every step's error is collected, and none of them aborts the rest.** A failure to release a name or retract a
comment must not strand the artifacts, the workspace and the cache as well.

What it leaves: **the `builds` rows.** `DeletePreview` is one statement against `previews`
(`internal/store/store.go:628`), and nothing removes the history — which is intended, since the history of what this
branch did is still true. It disappears on `build.keep_logs` with its log files instead. The `comments` row goes
too, but as a side effect of `Retract`, which on the local platform is a `DeleteComment` and on a hosted platform
deletes the comment on the pull request and touches no table at all.

**The pending `jobs` row goes too**, through `store.Dequeue` — the same statement the dashboard's cancel button
uses. `Claim` was once the only thing that removed a job, so a push landing moments before a close left one behind
for a worker to claim minutes later: it built, wrote the preview row back and published a share for something an
operator had deliberately removed. Cancelling the in-flight build and dropping the queued one are the two halves of
"stop the work", which is what unlinking a pull request is a button for.

Its failure is *collected*, unlike a name release or a comment retraction, because a surviving job puts back
everything the rest of teardown removes.

One race is left and is narrow: a worker that has claimed the job but not yet registered it in `d.running` is
visible in neither place. The commit lock bounds it — that build cannot publish until the teardown returns — and
closing it properly means the commit phase re-reading the pull request's state, which is an API call it does not
make today.

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
9. A column added to a table that has already shipped is declared **twice** — in `schema` and in `migrate` — or every
   existing database silently lacks it and every query naming it fails.
10. A build row outlives its share. A share that can no longer be republished has its `name` and `url` cleared, never
    the row deleted.
11. A reap runs on a complete keep-set or not at all.
12. A row never advertises a URL nothing is serving. A publication that could not be restored takes its `url` with
    it — `FailPreview` for a preview, `ClearBuildShare` for a build.
13. No job outlives the preview it would build. Teardown removes the queued one; nothing else has to notice.
