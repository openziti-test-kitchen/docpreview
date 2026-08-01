# 21 — Several exposers at once, and one per project

**Status: a plan. None of this is built.** Today a daemon publishes through exactly one exposer, chosen by
`exposer.kind`, and every preview has exactly one URL.

## What is wanted, and why the current shape refuses it

Three requests, which turn out to be one change:

1. **Several exposers at once.** A preview published to zrok for the world and to ziti for people inside the
   overlay, from one build.
2. **A per-project choice.** The public documentation site goes out through zrok; the internal runbooks go out
   through ziti only, and nothing about them appears on the internet.
3. **Changing it easily**, without editing YAML and restarting.

The third is already half-built — `exposer.kind` became a stored setting the dashboard writes, and the exposer
panel on `/secrets` reads it — and that is what exposed the shape problem. Four sections each with an `Enable`
button reads as four independent switches. It is not: `exposer.kind` is a single value, so enabling `local` turns
zrok off, which is a surprising way to discover that a preview has one URL.

The surprise is the honest signal. The data model, not the UI, is what says "one".

## What "one" is written into

| Where | What it says |
|---|---|
| `store.Preview` | one `URL`, one `Name` |
| `store.Build` | one `URL`, one `Name` |
| `expose.Publication` | one URL and one handle, returned by `Publish` |
| `Daemon.exposer` | one `expose.Exposer`, resolved at wiring |
| `Daemon.publishOrAdopt` | one publication per spec |
| The pull request comment | one **Preview** row |
| `Reap(ctx, keep)` | one keep-set, over one exposer's shares |
| The dashboard payload | one `url` per preview and per build |

Every one of those becomes plural. That is the size of the work, and none of it is subtle except the reaping.

## The reaping is the dangerous part

`Reap` deletes every share an exposer recognises as its own and that the keep-set does not claim. Today the
keep-set is built from every preview and build URL in the database, which is safe because there is one exposer and
every URL belongs to it.

With several, **a keep-set assembled from all publications and handed to each exposer is still safe** — each
exposer only recognises its own shares — but a keep-set assembled *per exposer* from the wrong subset is not: zrok
reaping with a keep-set built from ziti's publications deletes every live zrok share. This is the same footgun as
two daemons on one exposer account, arriving from inside a single daemon, and it deserves the same treatment:

**Invariant: a publication is keyed by exposer, and an exposer's keep-set contains exactly the publications
recorded against it.** A test that publishes through two fake exposers, reaps, and asserts both survive is the
guard. Without it the failure is silent — deleting a share you believe you own is a normal thing to do — and it
takes out live preview URLs on restart.

## The schema

A publications table, replacing the URL columns:

```sql
CREATE TABLE publications (
  preview_id  TEXT    NOT NULL,
  build_id    TEXT    NOT NULL DEFAULT '',   -- '' is the preview's own current publication
  exposer     TEXT    NOT NULL,              -- zrok2 | frontdoor | ziti | local
  name        TEXT    NOT NULL,
  url         TEXT    NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (preview_id, build_id, exposer)
);
```

`build_id = ''` for the preview's current publication keeps one table rather than two, and the primary key is the
thing that makes "one publication per exposer per build" a database rule rather than a convention.

## Changing a project's exposer: pause, purge, resume

**The decision: an exposer cannot be changed under a live project.** Pausing the project is the first step, the
purge is automatic and total, and only then can the exposer change. Resuming rebuilds from scratch.

That is a deliberate choice of a loud, explicit, destructive operation over a quiet, clever one. The alternatives
were all worse in the same way:

- **Re-point the existing publications.** The old exposer's shares have to be withdrawn, the new one's created,
  every URL in every comment rewritten, and if any step fails halfway the database describes a world that does not
  exist. Every failure mode is partial.
- **Leave the old publications and let reap sort it out.** That is the per-exposer keep-set problem above, run
  at the worst possible moment — the one restart where the set of exposers is not what the publications were
  recorded against.
- **Migrate.** Nothing to migrate, if the state is deleted first.

Pausing already exists in shape: a project can be disabled, and a pull request can be unlinked. This joins them
into one operation with a defined end state — **nothing published, nothing queued, nothing cached** — from which
changing the exposer is not a change at all. There is no state to be consistent with.

### What the purge removes

All of it, and the list is the specification:

| | Why it goes |
|---|---|
| Queued jobs for the project | A job that runs after the exposer changed publishes through the wrong one |
| Every publication and its share | The old exposer owns them; nothing else will ever reap them once it is disabled |
| Reserved names on the old exposer | They cost quota and hold a URL nothing serves. **The only part that is not obviously right** — a name is what keeps a URL stable, so releasing it means the URL is not reserved if the project ever moves back |
| Built artifacts | They are rebuilt on resume, and keeping them invites republishing under the new exposer without a build, which skips the base-URL check |
| Build logs | They belong to builds that no longer have a publication |
| The package-manager cache | Not strictly necessary. Included because "purge" that leaves something behind is the kind of half-measure this exists to avoid, and the cost is one slow build |
| The pull request comment | Retracted, not left pointing at a dead URL |

### What it does not remove

The project row, its settings, its credentials, and the record that a pull request was unlinked. Purging is about
published state, not about configuration — an operator changing an exposer is not asking to re-enter a token.

### The flow

1. **Pause** the project. It stops accepting webhook deliveries and its queued work is dequeued.
2. **Purge** runs, and reports what it removed. It is not a separate button: a paused project with published
   state is exactly the inconsistent middle this design exists to eliminate.
3. **Change the exposer**, per project or installation-wide. Nothing is published, so nothing can be wrong.
4. **Resume.** The scan queues the open pull requests and the branch preview, and they build fresh.

The confirmation must say the two things an operator will otherwise discover afterwards: every preview URL for
this project changes, and every open pull request comment is rewritten when its build finishes. Neither is
recoverable by pausing again.

### Why this makes multi-exposer simpler, not harder

With this in place the publications table stops needing to survive an exposer change at all. It only ever holds
publications for exposers that are currently enabled on projects that are currently live, because the only way to
change either is through a purge. The per-exposer keep-set invariant still holds and is still worth its test, but
it no longer has to be correct across a transition — there is no transition.

### There is no migration, and that is the point

The first draft of this plan treated moving `previews.url` and `builds.url` into the new table as the risky part —
a migration on a live database, tested against a copy, losing every URL in every open comment if it went wrong.

It does not need to happen. **A publication is a cache, not a fact.** The facts are the preview's *name* and its
artifacts on disk, and every URL is reproducible from the name: `Reap` then republish is what startup already
does on every restart, and it produces the same names and therefore the same URLs it produced last time. That is
why a restart does not change anybody's links.

So the migration is: **create the table empty**. Recovery fills it on the first start, exactly as it fills the
exposer with shares, and the old columns are dropped once nothing reads them.

Two consequences, both acceptable and both worth knowing:

- **The first start after the change reaps everything and republishes it.** The keep-set is built from
  publications, and there are none yet, so that start is the slow path — one exposer round trip per preview, the
  behaviour from before adoption existed. One restart, and only the first.
- **A URL that is *not* reproducible changes once.** Under zrok it is: the name is derived from the preview and
  the reserved name outlives the share. Under Frontdoor a share id is assigned by the tenant, so those URLs are
  new after this lands, and the comments are rewritten to match on the first status change.

Changing which exposers are enabled has the same shape and needs no special handling: publications belonging to
an exposer that is no longer enabled are deleted by that exposer's own reap, and the enabled ones republish. That
is the ordinary startup path, not a migration.

## The comment

One row becomes a small table:

```text
| Status  | ✅ Ready                                            |
| Preview | https://docs-feature-x.shares.zrok.io/  (public)    |
|         | https://docs-feature-x.preview.ziti/    (overlay)   |
```

Two things to get right. The **order** must be stable, or every status change rewrites the comment with the rows
shuffled and the edit history becomes noise. And a ziti or local URL needs saying what it is: a reviewer who clicks
`127.0.0.1` or an overlay hostname and gets nothing will report it as a broken preview. The label is not
decoration — it is the difference between "this link is for you" and "this link is for somebody on the overlay".

Every existing comment gets rewritten once, on the first status change after the change lands.

## Per-project selection

The easy half. `projects` already carries per-project build settings, so this is one more column: a list of exposer
kinds, empty meaning "the installation's default set". The projects page has the fields next to the driver and
image, and `.docpreview.yml` must **not** be allowed to set it — the same rule as every other project field, for
the same reason: the file arrives in the pull request, so on any repository where opening one is not a privilege
its author would otherwise choose where their branch gets published.

## Names collide per exposer, not globally

`Collides` and the name-release path treat names as one namespace. With several exposers, two previews may render
to the same name in different exposers without conflict, and the collision check has to be per exposer or it
refuses publishes that are fine. `Daemon.releaseNames` similarly releases per exposer.

## The order to build it in

1. **Pause and purge, on the exposer that exists today.** It stands alone and is useful before any of the rest:
   it is the honest answer to "I switched exposer and my comments are full of dead links", which has already
   happened once by accident. Ship it, use it, and the risky part of every later stage is gone.
2. **Schema and fan-out.** Publications table created empty, `Daemon` holds a set of exposers, per-exposer
   keep-sets, recovery loops over the set. No migration — see above. No UI change and no comment change: with one
   exposer in the set the behaviour is identical, which is what makes this stage reviewable.
3. **The two-exposer reap test**, before anything else uses the fan-out. Two fake exposers, publish through both,
   reap, assert both survive.
4. **The comment.** One row per publication, labelled, stable order. Exercised against the demo's four projects.
5. **Per-project selection**, on the projects page, gated on the project being paused.
6. **The exposer panel becomes checkboxes.** `Enable` means "add to the set" rather than "replace", and the panel
   stops implying a choice of one — which it currently does correctly, because it currently is one.

Stages 2 and 3 are one commit. Landing the fan-out without the reap test is landing the footgun.

## What this does not change

**One daemon per exposer account** still holds, per exposer. Two daemons sharing a zrok account still delete each
other's shares; several exposers in one daemon does not touch that.

**The exposer choice still takes a restart.** Exposers are constructed at wiring, and swapping one under a running
daemon would leave published previews pointing at an exposer that no longer owns them. Adding to the set is the
same: it takes effect on the next start, and the panel says so.
