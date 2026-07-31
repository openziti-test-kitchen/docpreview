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

**The open question this plan does not answer:** whether `previews.url` and `builds.url` stay as the primary
exposer's publication — backwards compatible, every existing query keeps working, one exposer stays privileged —
or move out entirely, which is cleaner and changes every read path and the dashboard payload at once. The second is
right and the first is what makes the migration survivable on a live database. Decide before writing the migration,
not during.

Whichever, the migration has to be tested against a copy of a real database. The live instance has 29 publications
in it, and a migration that loses them loses every URL in every open pull request comment.

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

1. **Schema and fan-out.** Publications table, migration, `Daemon` holds a set of exposers, per-exposer keep-sets,
   recovery loops over the set. No UI change and no comment change: with one exposer in the set the behaviour is
   identical, which is what makes this stage reviewable.
2. **The two-exposer reap test**, before anything else uses the fan-out. Two fake exposers, publish through both,
   reap, assert both survive.
3. **The comment.** One row per publication, labelled, stable order. Exercised against the demo's four projects.
4. **Per-project selection**, on the projects page.
5. **The exposer panel becomes checkboxes.** `Enable` means "add to the set" rather than "replace", and the panel
   stops implying a choice of one — which it currently does correctly, because it currently is one.

Stages 1 and 2 are one commit. Landing the fan-out without the reap test is landing the footgun.

## What this does not change

**One daemon per exposer account** still holds, per exposer. Two daemons sharing a zrok account still delete each
other's shares; several exposers in one daemon does not touch that.

**The exposer choice still takes a restart.** Exposers are constructed at wiring, and swapping one under a running
daemon would leave published previews pointing at an exposer that no longer owns them. Adding to the set is the
same: it takes effect on the next start, and the panel says so.
