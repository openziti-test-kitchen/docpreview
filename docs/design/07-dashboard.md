# The dashboard

One embedded HTML file, `internal/daemon/dashboard.html`, served at `/`. No build step, no dependency to fetch,
no CDN — the binary stays the only artifact that has to be deployed, which is the premise of the project.

`/v2` was the second layout while both existed side by side. It won and replaced the original; the path now
returns 301 to `/`, because it was linked from the old footer and is in browser histories.

## Two pages, one document

Credentials live at `/secrets`, not on the operations dashboard. They started as a collapsible panel at the top
of `/`, and two things were wrong with that. A credential form permanently occupying the top of an operations
screen invites pasting into it by reflex. And a distinct path is something a proxy or a future authentication
layer can gate — a panel inside `/` cannot be, since gating it means gating the status page with it.

### There is no Secrets control on the dashboard

Not hidden, not conditional: **gone** (`internal/daemon/dashboard.html:422-425`, where the comment stands in for
it). `/secrets` is reached by typing it or from a runbook.

Two reasons, and neither is about clutter. A control for credential management on an operations screen is
unrelated to everything around it — the top bar is filters and search, and a link out to a write surface reads as
one more of them. And a form for pasting a private key invites being pasted into, which is the same argument that
moved the panel off `/` in the first place; a link to it from the same place keeps the reflex and only adds a
click.

An earlier attempt kept the link and hid it conditionally, driven by a `secrets` field on `/status` reporting
whether a `SecretsAdmin` was wired. That field went with the link — `Status` is now exposer, pending, running,
previews and events (`internal/daemon/daemon.go:1226`). A status payload growing a field to tell a page whether to
render a link is the wrong shape: the route already 404s when nothing is wired (`internal/daemon/ingress.go:134`),
which is the same answer without a field to keep in step.

### One document, two paths

It is **the same embedded document**, switched on `location.pathname`
(`internal/daemon/dashboard.html:1491`). Splitting it into two files would mean two copies of the styles, the
escaping helpers and the fetch wrapper, kept in step by hand.

The route is registered only when a secrets admin is wired (`internal/daemon/ingress.go:134`), so a daemon with
no credential management answers 404 rather than serving a page whose every control is refused. Guarded by
`TestSetupPageIsServedAtItsOwnPath` in `internal/daemon/ingress_test.go:160`.

`/secrets` opens **no status stream** (`internal/daemon/dashboard.html:1546`). Nothing on it is live, and an
`EventSource` per idle browser tab is a connection the daemon holds for nothing. It also skips the 5-second
timestamp redraw, for the same reason: there are no relative times on it.

### Read-only for a caller that is not local

The state carries `can_write` and `read_only_why` (`internal/daemon/secrets.go:173`), and the page mirrors what
the server will decide so it never offers an action that is going to 403.

| `can_write` | What renders |
|---|---|
| true | rows, fields, Set / Generate / Delete |
| false, vault open | rows and their set/missing flags; no fields, no buttons, and a banner saying why |
| false, vault locked | one sentence; **not** the unlock form |

Rows still render because seeing that two credentials are set is useful from a colleague's browser, and hiding
the panel outright reads as a missing feature rather than a deliberate boundary
(`internal/daemon/dashboard.html:1246`).

The locked case is the one that matters. A passphrase prompt served to a remote browser asks someone to type the
one secret that opens everything into a form whose POST the server will refuse — worse than no panel at all
(`internal/daemon/dashboard.html:1377`). The boundary itself is in [05-secrets.md](05-secrets.md).

## Layout

```
┌─ sticky top bar ─────────────────────────────────────────────┐
│ docpreview  [All|Ready|Building|Queued|Failed]  [project ▾] [search] │
├──────────────────────────────────┬───────────────────────────┤
│ Projects (4)                     │ Activity · all projects   │
│                                  │ [All|Ready|Building|…]    │
│ ▶ mydocs   new-install-guide #12 │ 14:22 ● Ready mydocs#12   │
│   ●3 ●1        [Building] 4s  Open ↗                         │
│                                  │ 14:21 ● Building …        │
│ ▼ handbook api-reference #7      │                           │
│   [preview ▾] [build ▾] [Following][Expand][Download]         │
│   ● streaming this build                                     │
│   ┌────────────────────────────┐ │                           │
│   │ $ yarn build               │ │                           │
│   └────────────────────────────┘ │                           │
└──────────────────────────────────┴───────────────────────────┘
```

## One row per project, not per preview

A project with a dozen open pull requests produced a dozen rows all naming the same project, and the list
stopped being scannable at exactly the point it started being useful.

Each row shows the project's **most interesting** preview and per-state count chips. The rest are a dropdown
inside the expanded row, labelled `branch · #num · state · 4m ago`.

"Most interesting" is by state first, then time:

```js
const ORDER = ["failed", "building", "queued", "ready"];
```

A failure nobody has looked at matters more than a build that went green a minute later. A project row saying
Ready while one of its branches is broken is a lie of omission.

## Filters

| Control | Filters | Why it is separate |
|---|---|---|
| Top counters | previews, by state | counts what is true *now* |
| Project select | previews, by project | scales past three projects; chips do not |
| Search | project, branch, PR number, name | when you know the branch but not the project |
| Feed filters | activity, by kind | a history, not a state |

**A select, not chips, for projects.** Chips are nicer at three projects and a wall at three hundred; the
browser already knows how to render a long list, scroll it, and type-ahead it. It is only rebuilt when the set
of projects actually changes, because rebuilding a `<select>` closes its dropdown.

**The feed has its own kind filter** rather than following the top counters. The feed is a history: narrowing it
to "ready" would hide the queued and building entries that led there. Only kinds actually present get a button —
a row of five filters where four are always empty is four things to read and discount every time.

### Finished attempts show one row, not three

Every state transition is recorded, so one build leaves `queued`, `building` and `ready` behind. Shown as three
rows that reads wrong, and an operator said so: a row saying **Queued** four minutes ago, wearing the same word and
the same coloured dot a live queued row wears, looks like a job that is stuck. It is a fact about the past written
in the present tense.

`collapse` drops an attempt's non-terminal entries once that attempt has reached a terminal state, and keeps them
while it has not — a build queued or building *now* still has its row, which is when that row carries information.
Grouped by preview **and** commit: two attempts of one pull request are two builds, and collapsing them together
would hide the older one's outcome. Verified against a real 19-entry history: 13 rows, and all 13 attempts still
represented.

**The kind filter bypasses it.** Picking "queued" is asking for exactly what was hidden, so a filter that answered
with nothing would be a filter that lies. The filter counts come from the uncollapsed set for the same reason.

### An entry opens the build it names, not just the preview

Clicking one used to call `reveal(preview_id)` and ignore the commit — while its own tooltip promised to open the
build. Every preview of one repository shares a project row, so the visible effect was re-picking the preview
*inside* a row that was usually already open, and clicking an entry for the preview already showing did nothing at
all. It read as a dead button, which is what it was reported as.

`reveal` now takes the commit and selects that build in the picker, matching on the build id's suffix — a build id
is `<date>-<time>-<short sha>` and the event carries only the sha. The newest build takes the picker's empty value
rather than its own id, because empty means "follow the live stream" and the newest log is still being written.

The row also flashes on every click. A click that legitimately changes nothing is indistinguishable from a broken
one, and that case is the common one here rather than the edge.

## The activity feed survives a restart

The feed is a 200-entry in-memory ring (`internal/daemon/events.go:43`), so before this it was empty after every
restart — a list headed "recent activity" that forgot everything the moment the process did, which reads as a
broken feed rather than as an empty one. `Daemon.backfill` seeds it from the `builds` table at startup
(`internal/daemon/events.go:100`), before any worker can add to it, so the restored history sits behind this run's
events rather than interleaved with them (`internal/daemon/daemon.go:351`).

**Oldest first.** `add` writes into a ring and `recent` walks backwards from the last write, so the newest restored
entry has to be the last one written. `RecentBuilds` returns newest-first, so `backfill` iterates it in reverse
(`events.go:107`).

Each row is stamped with `finished_at`, falling back to `started_at` for a build that was in flight when the
process died — the time of the transition it describes, which is the rule below. Only the states the `builds` table
records appear; `queued` lives in the job queue and nowhere else. See [08-storage.md](08-storage.md).

## The log pane

Inline in the expanded row, 15 rem by default, `Expand` to 34 rem. The previous layout gave it 30 rem
permanently whether or not a log was open, which pushed everything else off screen.

**Exactly one stream is open at a time.** Nothing is fetched for a collapsed project. Several at once would mean
N server-sent-event connections from one tab, and browsers cap concurrent connections per origin — the sixth
open row would hang with no visible reason.

### The banner

Above the terminal, from the server's `start` event:

| Situation | Banner |
|---|---|
| Building | ● streaming this build (green) |
| Queued / Ready / Failed | ● previous build · `20260728-183138-7c6a873` — nothing is running |
| Finished while watched | ● this build finished |
| No log | ● no build log |

A queued preview replays its last completed build. Without the banner the pane looked like the queued build had
already produced all of it.

### The build picker

Beside the preview picker, listing every stored build of the selected preview with an outcome icon and the word
for it — ✅ succeeded, ❌ failed, 🔨 running, ⏳ queued, ⏭️ skipped (`internal/daemon/dashboard.html:1064`).

**The word is there as well as the icon.** An emoji alone is unreadable to a screen reader, and at a glance ❌ is
as easily "cancelled" as "failed". The icons are the same ones the rows use, so one glance means the same thing in
both places.

**A build with no recorded row renders as unknown, not hidden** (`internal/daemon/stream.go:390-393`). Logs
predating the `builds` table have no outcome, and so does a build whose row was pruned before its log. The log is
still readable either way, and a picker that silently omits a log it could serve is worse than one that admits it
does not know how the build ended.

**The newest entry keeps the empty value**, which means "follow the live stream" rather than "download this file"
(`internal/daemon/dashboard.html:1043-1047`). A running build's log file is still being written, so reading it as
a file shows a truncated copy that never updates. There is no separate synthetic "latest" row above it: the newest
build *is* the latest, so the extra entry said the same thing twice and made the list one longer than the number
of builds.

**Hidden, not disabled, when there is only one build** (`dashboard.html:1036`). A greyed-out control invites
clicking; nothing at all reads as "there is only one build", which is the truth.

Fetched on demand from `/logs/{preview}` when a row expands (`dashboard.html:1014`), not folded into `/status`.
A status payload carrying every build of every preview grows without bound on a busy repository and is re-sent on
every state change — for a list nobody is looking at unless a row is open. The outcomes are joined onto the log
metadata server-side (`internal/daemon/stream.go:394-418`) rather than stored on the log file: the log is bytes on
disk, and how a build ended is the daemon's knowledge.

The rewrite is skipped while the picker is the active element, because rewriting an open `<select>` closes it with
no visible cause (`dashboard.html:1032`). Skipped rather than stashed in `dataset.pending` as rule 3 does, because
this list only changes when a build starts or ends and the next expand refetches it — there is no repeating tick
to keep missing.

### Following

On by default. `appendLog` scrolls to the bottom as lines arrive.

The first version had a second, invisible gate: it only scrolled if you were already within 40 px of the bottom.
Scroll up to read and it silently stopped following **while the button still said "Following"**. A control that
lies about its own state is worse than one that does the wrong thing.

Now: scrolling away flips it to Paused, clicking Following jumps to the bottom and re-arms, opening a different
log re-arms it. `ui.autoScrolling` guards the scroll handler against the write `appendLog` just did, which would
otherwise read as the reader scrolling.

### Log colouring

Three cases, all structural rather than interpreting the tool's output: a command (`$ …`), something
error-shaped, and a redaction. `*****` is highlighted in amber **deliberately** — so redaction is visibly
happening rather than silently assumed.

## The preview link is on the row

Opening the site is the commonest thing anyone wants from a list of previews; burying it behind a click to
reveal a build log inverts that.

It is a **sibling** of the toggle button, not inside it: an anchor nested in a button is invalid markup and
would both navigate and toggle.

It is only a link while the state is `ready`. Queued, building and failed have no URL, and a torn-down one has a
URL that no longer answers — rendering either as live sends the reader to an error page. An anchor with no
`href` is not a link, which is exactly the wanted state, and the styling follows from that fact rather than from
a `disabled` attribute anchors ignore. The tooltip says why.

## Rendering rules

These are the ones that produced visible bugs.

### 1. Update by key; never rebuild the list

Setting `innerHTML` on the list every status tick destroyed and recreated the open log pane — a few hundred
lines of log thrown away and re-inserted twice a second. That was the flicker. It also dropped hover and focus.

Rows are created on first sight and mutated in place, keyed by project. Each has a signature; an unchanged
signature is a no-op.

### 2. Do not move what the reader is touching

`groups()` sorts by state, so a project that starts building climbs to the top — under a reader mid-sentence in
an expanded log. Moving a DOM node also closes any `<select>` open inside it and drops focus, so a background
wave arriving while the preview dropdown was open shut it with no explanation.

```js
function frozen(el) {
    return ui.open !== null || el.contains(document.activeElement);
}
```

Reordering is suspended while a row is expanded or the list has focus. New rows are still appended, so nothing
is hidden; the order just stops rearranging itself under your hands. **The sort is right when you arrive at the
page and wrong the moment you start using it.**

### 3. Defer, do not skip, an update to a focused control

A `<select>` rewritten while its dropdown is open closes it. There is no DOM event for "the dropdown is open",
but a select can only open while focused — so the update is stashed in `dataset.pending` and applied on blur.

### 4. State is not markup

`updateHead` returns early when the head is the active element, to avoid stealing focus by replacing it. The
`open` class and `aria-expanded` used to be set at the *bottom* of that function, past the guard.

Clicking a row makes the button the active element. So the guard fired, the class never went on, `term()` —
which selects `.item.open .term` — found nothing, and **the first expand of any row showed an empty log** until
some other click moved focus. They are now set unconditionally in `renderList`, before `updateHead` runs.

A class other code queries by is state. It does not belong behind a repaint optimisation.

### 5. Pin a choice the reader made

The picker had no stored selection; `groups()` re-derived it as `list[0]` every render. Since that list is
sorted by state, a build starting anywhere in the project made the new preview "first" and swapped it into the
dropdown under a reader part-way through a different log.

Expanding pins the selection. It holds until the reader picks another or collapses the row; collapsing releases
it, so reopening later lands on whatever is newest *then*. If the pinned preview is torn down, the pin
re-anchors rather than floating again.

### 6. A relative age must come from the event it labels

This one is server-side, and it is here because the bug was only ever visible on this page.

Queued rows showed `updated_at` from the preview's last **finished** build. A stored preview keeps that timestamp
while the next build runs, so a job enqueued seconds ago rendered as "3h ago" — on the one screen someone looks
at to see whether the queue is moving.

`Daemon.Status` assembles rows from three branches, and only the middle one was wrong
(`internal/daemon/daemon.go:1102`):

| Branch | Age comes from |
|---|---|
| stored row + running build | `b.started` |
| stored row + queued job | `j.EnqueuedAt` — this was the stored row's `updated_at` |
| in flight with no stored row | `b.started`, or `j.EnqueuedAt` for a queued one |

`Store.PendingJobs` returns `enqueued_at` alongside the payload to make that possible
(`internal/store/store.go:194`); the column was already there for `Claim`'s ordering.

The third branch — a first build of a branch, which has no stored row anywhere — used `time.Now()`. That is worse
than a stale timestamp: it re-renders as "just now" on every poll, so a job wedged in the queue for an hour looks
freshly arrived forever, which is the one thing that row exists to reveal (`internal/daemon/daemon.go:1149`).

**The rule:** every timestamp handed to this page is the time of the transition it describes. There is no case
where "now" is the right answer, because the page's own clock already supplies that.

## Colour and motion

The palette is a validated set — the status hues are reserved for state and never reused decoratively. Dark mode
is stepped deliberately rather than inverted, because an inverted palette puts saturated hues on a dark surface
where they vibrate.

**Building spins; queued pulses.** A dot that fades is ambiguous — it reads as "stale" as easily as "working" —
where rotation only means one thing. Waiting for a worker and doing work must not look identical. The spinner
appears in three places: the row's state pill, the per-project count chip, and the top Building counter, so
work is visible without reading a number or finding the row.

Drawn with a CSS border: one element, no request, and it inherits the state colour it is already given. The
track is `transparent` with two lit sides. It is **not** `color-mix()`: `--edge` is set inline to a value that is
itself a `var()`, and nesting that second indirection inside `color-mix` makes the whole `border` declaration
invalid — the browser drops it, leaving a transparent box and no spinner at all.

`prefers-reduced-motion: reduce` **slows** the spin to 2.4 s rather than stopping it. Stopping it left a static
ring indistinguishable from a decorative dot, so the one thing the spinner exists to say was silently dropped
for anyone with the setting on — including anyone who has it on without knowing. The state is also carried by a
word and a colour for anyone who needs no motion at all.

## Safety

```js
const esc = s => String(s ?? "").replace(/[&<>"']/g, …);
const safeURL = u => /^https?:\/\//i.test(String(u ?? "")) ? String(u) : "";
```

Escaping closes attribute breakout but not scheme abuse: `javascript:` and `data:` survive it intact. Preview
URLs come from the exposer rather than from a pull request, so it is not reachable today — but the rule belongs
with the render, not with an assumption about who writes the value.

A dashboard that silently freezes is worse than one that admits it, so the footer reports `live` /
`reconnecting…`. Relative timestamps are redrawn on a 5-second interval, because the status stream is silent
when nothing changes — which is exactly when "2s ago" has become a lie.

## Invariants

1. One log stream open at a time, and only for the expanded row.
2. Nothing the reader is focused on or reading is rebuilt or moved.
3. State classes are set unconditionally, outside repaint guards.
4. A control's label matches its behaviour.
5. Only `ready` previews get a live link.
6. Every interpolated value is escaped; every URL is scheme-checked.
7. Every timestamp is the time of the transition it labels, never `time.Now()` at assembly.
8. The page never offers a control the server would refuse: no `can_write`, no fields and no buttons.
9. `/secrets` opens no `EventSource`, and nothing on `/` links to it.
10. A stored build log is reachable from the picker even when nothing records how its build ended.
11. `/status` carries only what changes on a state change; per-preview detail is fetched on demand.
