# Concurrency and the commit phase

The requirement, in the user's words: *"if 2 commits come through the ONLY result that should be published is
the second."*

That sentence is harder than it looks, because "the second" has to win a race against a build that started
first and may finish later.

## The queue

sqlite, one table, **at most one pending job per pull request**:

```sql
INSERT INTO jobs (preview_id, payload, enqueued_at) VALUES (?, ?, ?)
ON CONFLICT(preview_id) DO UPDATE SET payload = excluded.payload, enqueued_at = excluded.enqueued_at
```

Replacement, not accumulation. A reviewer fixing typos pushes five commits in two minutes; building all five
wastes four builds and — worse — publishes four previews nobody will look at before the fifth replaces them.

`Claim` is `DELETE ... RETURNING`, atomic in sqlite, so two workers racing cannot both get the same job.
Deleting rather than marking "running" means a crash mid-build loses that build. That is the right trade: the
alternative is a row stuck in "running" forever after a hard kill, and distinguishing "running" from
"abandoned" needs heartbeats nobody wants to operate.

Workers are woken directly on enqueue; the one-second poll is only the fallback that picks up jobs left by a
previous process.

## Three mechanisms, and why one is not enough

### 1. Cancellation

`handleBuild` cancels any in-flight build for the same preview and **clears its entry from `d.running`**
(`internal/daemon/daemon.go:629-635`). Each build runs under a context derived from the daemon's.

Clearing, not just cancelling. The entry is what a build's `isCurrent` check reads, so a build cancelled while
nothing else is registered — the replacement is enqueued but no worker has claimed it yet — would still see itself
as current and publish.

This happens *after* the enqueue, so the report ordering below is not affected by it: the replacement job is in the
queue before the old build is told to stop.

Cancellation alone is **not sufficient**, for two reasons:

- The zrok SDK's `CreateShare` takes no context and cannot be interrupted.
- Publishing a name withdraws whatever currently holds it. A superseded build reaching `Publish` therefore
  *destroys the newer preview* rather than merely wasting its own effort.

### 2. Generation ownership by pointer identity

```go
type build struct {
    cancel  context.CancelFunc
    pr      model.PullRequest
    started time.Time
}
```

`d.running[previewID]` holds a `*build`. The deferred cleanup clears the entry **only if it is still ours**:

```go
if d.running[id] == me {
    delete(d.running, id)
}
```

It is a pointer specifically so identity is comparable. The first version stored the `context.CancelFunc`, and
func values are not comparable in Go — so the defer could only test `!= nil`, and a late-finishing superseded
build deleted the cancel handle belonging to the build that replaced it. The next push then had nothing to
cancel.

This is the same shape as the exposer fix in [02-exposers.md](02-exposers.md): **an object that outlives its
successor must not be able to clean up on its behalf.**

### 3. The commit phase

Everything before it is reversible. Everything in it is not: it replaces the served artifacts, takes the public
name away from whoever holds it, and rewrites the database row.

```go
commit := d.commitLock(pr.PreviewID())
commit.Lock()
defer commit.Unlock()

if ctx.Err() != nil || !d.isCurrent(pr.PreviewID(), me) {
    return out, decision, errSuperseded
}

replaceDir(result.OutputDir, artifactDir)
site, _ := preview.New(artifactDir, repoCfg.Build.BaseURL)
pub, _ := d.exposer.Publish(ctx, spec, site)

d.live[id] = pub                 // closing the old one, by identity
d.store.SavePreview(saveCtx, ...)
```

**The check and the writes happen together, under one per-preview lock.** A build that is no longer current
stops there having changed nothing. Checking liveness outside the lock would leave a window in which a
superseded build passes the check and then publishes over its successor — which is precisely the bug that
existed before the phase was introduced.

The lock is per preview, not global: two different pull requests publishing at the same time is normal and
should not serialize.

### The detached save context

```go
saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
```

The write must complete even though this build's own context may have been cancelled moments ago. The share is
already live; a database row that does not match it would survive a restart as an unreapable orphan.

### Undoing a publish

If `SavePreview` fails, the share is live and nothing durable records it. Left alone that means a working URL, a
comment saying the build failed, an empty `/status`, and a restart that reaps the share as an unknown orphan —
publication and persistent state pulled apart, which is exactly what the phase exists to prevent.

So `unpublish` withdraws it. It deliberately does **not** restore whatever was published before: `Publish`
withdrew that to take the name, so there is nothing to restore to. "Nothing is live" is the only consistent
state reachable from there, and it is an accurate one.

## Reports have an order, and it is not the order they were made in

Three changes, all from watching a real pull request comment say the wrong thing.

### 1. `queued` is reported before the enqueue

`handleBuild` calls `d.report(… StateQueued …)` and *then* `store.Enqueue` (`internal/daemon/daemon.go:603-620`).

The other way round is a race, and it lost most of the time. `Enqueue` nudges a worker, the worker claims the job
and reports `building` — all of it faster than the queued report's network round trip. So the comment settled on
"Queued" for a build that was already running, and stayed there until the build finished. Emitting it first means
the job does not exist yet, so nothing can be building.

The report is best-effort where the enqueue is not: a queue write that fails means no build and the caller has to
hear about it, while a comment that fails to say "queued" is a cosmetic loss on the way to saying "building".

### 2. `staleReport` drops anything ranked below what has already been reported

Ordering the emitters is not enough — several goroutines report on one preview, and a supersede storm has more
than one build reporting at once. `Daemon.staleReport` (`internal/daemon/daemon.go:511`) keeps the furthest state
reported per preview and drops any report ranked below it:

```go
queued 1 → building 2 → ready | failed | skipped 3 → anything unrecognised 4
```

Unrecognised is 4, not 0: a state this function has not been taught about — a teardown, something added later — is
terminal rather than stale, because silently dropping it is the worse failure (`reportRank`, `daemon.go:479`).

**Keyed by commit.** The lifecycle restarts with every push, so `ready` → `queued` is backwards within one commit
and correct across two. A report whose commit differs from the mark resets it rather than being compared against
it.

**Strictly below, not below-or-equal.** Two `ready` reports for one commit are legitimate: a republish after a
restart can move the URL, and the comment has to be rewritten to carry the new one.

Teardown deletes the mark along with `live`, `running` and `commit` (`daemon.go:668`). The lifecycle is over, so the
next report starts a new one; keeping the mark would make a reopened pull request's `queued` look stale against the
terminal state of the build that was torn down, and it would be dropped.

The drop happens in `report` before `d.record`, not on the way out to the platform (`daemon.go:1095`). The
dashboard reads the same reports, and three surfaces disagreeing about a build's state is worse than any one of
them being briefly behind.

**A timestamp cannot do this job**, and the observed failure is the proof. In the inversion this defends against
the queued report was stamped *later* than the building report, because it was created later. A timestamp records
when a message was made; the invariant being violated is lifecycle position. Sorting by time reproduces the wrong
order faithfully.

Tests: `internal/daemon/publisher_test.go:150-194`.

### 3. Platform writes are debounced per preview

`internal/daemon/publisher.go` holds a report for 250 ms and writes only the newest one for that preview. Every
state change is an API write, and a comment upsert is two requests when it has to search for the comment first; a
build emits four-ish reports and a supersede storm multiplies that by the number of pushes, straight into GitHub's
secondary rate limit — which fires on burst rather than on volume.

**Safe because a report is a snapshot, not an event.** Nothing downstream accumulates them: the comment is
rewritten whole each time. A report superseded inside the window was never going to leave a trace, so writing it
would only have cost a request.

Two details are the difference between working and subtly broken:

- **The timer is not reset on each report** (`publisher.go:91-99`). Replacing the payload and leaving the timer
  running bounds the delay at one window. Resetting it is the classic debounce failure — a steady stream of reports
  postpones the write forever, and the preview never reports because it keeps almost reporting.
- **`Close` flushes** (`publisher.go:130`), before the exposer closes, and `Run` calls it after the workers exit
  (`daemon.go:376`). A shutdown inside the window would otherwise lose the last report of every in-flight build —
  usually the terminal one, leaving a comment reading "Building" for a build that finished. After `Close`, a
  straggler is written straight through rather than dropped: shutdown is exactly when a terminal state most needs
  to reach the pull request.

Keyed by preview rather than globally, because two pull requests reporting at once are unrelated and a shared timer
would be a shared latency.

**It is not an ordering mechanism.** The last report in a window wins, so a debouncer fed out-of-order reports
faithfully publishes the wrong one. That is `staleReport`'s job, done before a report reaches here.

## Reporting a superseded build

```go
if errors.Is(err, errSuperseded) || (ctx.Err() != nil && parent.Err() == nil) {
    log.Info("build superseded, discarding result")
    return
}
```

No comment is written. A newer build is already updating the same comment, and a late failure message would
overwrite a perfectly good "ready".

This suppresses **only the comment**. Everything with a lasting effect is guarded inside the commit phase,
because by the time control returns here those writes have already happened or been skipped.

## Recording a failure without destroying a working preview

Two things are true at once after a failed rebuild, and the obvious implementations each throw one away.

- **Overwriting the row with `failed`** loses the fact that the previous build is *still serving*. On the next
  restart, recovery skips a non-ready row and the reaper then deletes a share that was working fine.
- **Writing nothing**, which is what the first version did, means a failure has no row — so it never appears on
  the dashboard and there is nothing to click to reach its log. Which is exactly when somebody wants the log.

`recordFailure` therefore leaves an existing preview alone and inserts a row only when there is none. The build
log is written either way and is reachable by preview ID.

## Status is composed, not stored

The preview table holds committed states only — a row is written when a build *succeeds*. `queued` and
`building` are never in it.

`Daemon.Status` composes three sources:

| Source | Contributes |
|---|---|
| `store.ListPreviews` | the committed rows |
| `store.PendingJobs` | queued |
| `d.running` | building, and rows for first builds that have no stored preview yet |

Reading the table alone reported every in-flight build as whatever it was last time, or omitted it entirely on a
branch that had never finished a build. The dashboard's Building counter read 0 while builds were visibly
running, and the activity feed disagreed with it because events carry the state directly.

**Building wins over queued.** A push that supersedes a running build cancels it and enqueues the replacement,
so the same preview is briefly both; the interesting half is the one doing work, and a row that flips to Queued
mid-build reads as progress going backwards.

Not persisted, on purpose: a row saying "building" would survive a crash forever, and writing transient state
would overwrite the URL and reason of a preview that is still serving perfectly well.

Tests: `internal/daemon/status_test.go`.

## Invariants

1. Two pushes end with the second published, never the first.
2. A superseded build changes nothing durable and writes no comment.
3. Ownership is tested by pointer identity, never by key alone.
4. The liveness check and the irreversible writes are inside one per-preview lock.
5. A failed rebuild does not retract a working preview.
6. Transient states are composed at read time, never persisted.
7. A report for a state a commit has already passed is dropped, on every surface, whatever its timestamp says.
8. The debouncer's timer is never reset by a new report, and `Close` flushes what it holds.
