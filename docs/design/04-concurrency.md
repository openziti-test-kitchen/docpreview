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

`Daemon.Handle` cancels any in-flight build for the same preview before enqueuing the replacement. Each build
runs under a context derived from the daemon's.

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
