# The dashboard

One embedded HTML file, `internal/daemon/dashboard.html`, served at `/`. No build step, no dependency to fetch,
no CDN — the binary stays the only artifact that has to be deployed, which is the premise of the project.

`/v2` was the second layout while both existed side by side. It won and replaced the original; the path now
returns 301 to `/`, because it was linked from the old footer and is in browser histories.

## Three pages, one document

Credentials live at `/secrets`, not on the operations dashboard. They started as a collapsible panel at the top
of `/`, and two things were wrong with that. A credential form permanently occupying the top of an operations
screen invites pasting into it by reflex. And a distinct path is something a proxy or a future authentication
layer can gate — a panel inside `/` cannot be, since gating it means gating the status page with it.

### The Secrets control left the dashboard, and came back gated by the server

For a while there was no link at all: not hidden, not conditional, gone. Two reasons, and neither was about
clutter. A control for credential management on an operations screen is unrelated to everything around it — the
top bar is filters and search, and a link out to a write surface reads as one more of them. And a form for pasting
a private key invites being pasted into, which is the same argument that moved the panel off `/` in the first
place; a link to it from the same place keeps the reflex and only adds a click.

An earlier attempt at keeping the link hid it conditionally, driven by a `secrets` field on `/status` reporting
whether a `SecretsAdmin` was wired. That field went with the link and is not coming back — `Status` is exposer,
instance, pending, running, previews and events (`internal/daemon/daemon.go:1803`). A status payload growing a
field to tell a page whether to render a link is the wrong shape: the route already 404s when nothing is wired
(`internal/daemon/ingress.go:148`), which is the same answer without a field to keep in step.

What the top bar has now is a separate question with its own answer. `#admin-nav` holds **Projects**, **Secrets**
and **Clear caches**, and it is drawn only after `GET /api/admin` says this request is one the daemon would accept
a write from (`showAdminLinks`, `internal/daemon/dashboard.html:2662`, and `Ingress.admin` in
`internal/daemon/admin.go`). That endpoint runs the same `isLocalRequest` check the write endpoints run, and
reports false both for a surface that is not wired and for one that is wired but would refuse this caller —
from the page's side those are the same fact (`TestAdminStateIsFalseForUnwiredSurfaces`,
`TestAdminStateIsFalseWhenForwarded`).

Three things about that arrangement. A Host-header test in the page would have been worthless: `Host` is whatever
the client typed, so a tunnelled visitor sending `localhost` would pass it — where the connection came from is the
daemon's to know. Hiding a link is not a security boundary and is not doing any work as one; typing `/secrets`
through a tunnel still gets nowhere, and the endpoint is absent from the dashboard-only proxy's allowlist so the
fetch 404s there and the links stay absent. And the links are styled quieter than the filters beside them
(`internal/daemon/dashboard.html:127-134`), because they leave this page and nothing on an operations screen
should draw the eye toward a form for pasting a private key.

**Clear caches is one button, not one per row.** Per row it appeared on every open row at once, which reads as
part of reading a log rather than as the rare repair it is — and a preview's cache is deleted with the preview
anyway, so the manual version exists only for the pull request whose next build has to work now. It confirms
first, because it clears *every* preview's cache and that is not visible from the button (`wireClearCache`,
`internal/daemon/dashboard.html:2728`). `DELETE /api/cache` is served by `ProjectsAdmin` rather than by a third
admin surface, so there is one gate instead of two copies of the same checks to keep in step
(`internal/daemon/projects.go:85-87`).

### One document, three paths

It is **the same embedded document**, switched on `location.pathname`
(`internal/daemon/dashboard.html:2217-2219`). Splitting it into three files would mean three copies of the styles,
the escaping helpers and the fetch wrapper, kept in step by hand.

Each admin route is registered only when its admin is wired — `/secrets` at `internal/daemon/ingress.go:161`,
`/projects` at `internal/daemon/ingress.go:177` — so a daemon with no credential management answers 404 rather
than serving a page whose every control is refused. Guarded by `TestSetupPageIsServedAtItsOwnPath` in
`internal/daemon/ingress_test.go:160`.

Neither admin page opens a **status stream** (`internal/daemon/dashboard.html:2631-2641`). Nothing on them is
live, and an `EventSource` per idle browser tab is a connection the daemon holds for nothing. They also skip the
5-second timestamp redraw, for the same reason: there are no relative times on them.

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
(`internal/daemon/dashboard.html:1966`, and `TestSecretsReadIsAllowedRemotelyAndReportsReadOnly`).

The locked case is the one that matters. A passphrase prompt served to a remote browser asks someone to type the
one secret that opens everything into a form whose POST the server will refuse — worse than no panel at all
(`renderUnlock`, `internal/daemon/dashboard.html:2095`). The boundary itself is in [05-secrets.md](05-secrets.md).

`/projects` reports the same two fields for the same reason (`internal/daemon/projects.go:136-137`), and its write
path shares both of the credential surface's gates: a project row decides what command runs on the build host, so
it is a more direct route to executing code here than the vault is (`internal/daemon/projects.go:97-132`).

### `/projects`, and the two bugs that made it unreadable

The third page manages projects and each project's own environment variables. It was reported as noisy and hard to
follow, and neither cause was in its rendering logic.

**`el.hidden = true` did nothing to `.wrap`.** `[hidden]` supplies `display: none` at the user-agent level, which
loses to any author rule that sets `display` — and `.wrap` sets `grid`. So `hideOperationsChrome`
(`internal/daemon/dashboard.html:2623`) marked the previews and activity sections hidden and they rendered anyway,
empty, under both admin pages. Half a screen of operations chrome below a form, on a page with no operations on it.
The fix is one rule, `[hidden] { display: none !important }`, **last in the stylesheet**
(`internal/daemon/dashboard.html:620`): `!important` alone suffices in a browser, and last as well so the jsdom
harness — which resolves the cascade by document order — agrees with one. `tools/dashboardtest/projects.mjs` asks
the *computed style*, not the property, and reports "previews and activity are gone, not merely marked hidden".

**Errors were reported into a hidden element.** `run()` had the secrets renderer and `#setup-body` hardcoded, so a
failed project save rendered *project* state through the *credential* renderer and prepended the resulting exception
to a panel that is `hidden` on `/projects`. From the operator's side: a button that does nothing and says nothing.
Both are arguments now, and the projects page passes its own pair through `runProject`
(`internal/daemon/dashboard.html:2193-2213`); the harness asserts the notice lands in `#projects-body` *and* that
nothing was written into the hidden secrets panel.

The rest was arrangement rather than defect, but the arrangement was the complaint:

| Before | Now | Why |
|---|---|---|
| Fields as one `·`-joined line in an 18rem column | Label-over-value pairs in a wrapping grid | The line wrapped mid-value — `output:` ending one line and `build` starting the next, which reads as a field called build |
| The add form permanently open, eleven inputs | Behind **New project** | It outweighed the single project it existed to add |
| Edit filled that same form, far below the row | Expands the row in place | Nothing connected the row to the form that was editing it |
| `github` as bare monospace under the title | A platform chip beside the name | Indistinguishable from a stray word |
| Nothing about credentials | A **Secrets** control with a count | The page is where a missing token is diagnosed |

One panel open at a time, held in `projOpen` outside the render (`internal/daemon/dashboard.html:2244`), because a
dozen expanded cards is the wall of fields this replaced. Clicking the open panel's own button closes it. Which one
is open survives the refresh after a save, so adding three variables is three clicks rather than a hunt each time —
the add form is the exception and closes on success, since its project now has a card of its own.

The add form and the edit form are one function (`projectForm`), because `PUT` is an upsert and two UIs for one
operation drift. In edit the platform, owner and repo inputs are absent rather than read-only: changing them would
silently create a second project rather than rename this one. Every other field is sent on every save, including
from the Disable button, because the `PUT` is a whole-row upsert and omitting a field would clear it — the harness
asserts that disabling a project does not erase its build command.

Inherited server-wide variables are shown greyed rather than omitted: "this project has no variables" and "this
project has none of its own" look identical otherwise, and only one of them means a build is about to fail
(`GlobalSecrets`, `internal/daemon/projects.go:149`). Names only, never values, on the same rule as the credential
page and for the same reason: this payload is readable from anywhere the dashboard is, while writing it is
loopback-only.

Covered by `tools/dashboardtest/projects.mjs`, which needs no daemon — including the two bugs above, because both
will come back the moment somebody adds a fourth page.

### What the page asks the daemon for

| Path | Answers with | Registered |
|---|---|---|
| `GET /events` | SSE: a `status` event whenever the `/status` payload differs, plus an idle heartbeat | always |
| `GET /status` | the same payload, once, as JSON — the footer links it for a human | always |
| `GET /api/admin` | `{secrets, projects, why}` — whether *this request* could use either surface | always |
| `GET /logs/{preview}` | `{preview_id, live, logs[]}`; each log is a `Meta` plus `state`, `reason`, `url` | always |
| `GET /logs/{preview}/stream` | SSE: one `start`, then `line` events, then `done` | always |
| `GET /logs/{preview}/download[/{build}]` | the log as an attachment, bounded by the declared length | always |
| `GET /api/secrets`, `PUT`/`DELETE`/`generate`/`unlock` | the credential state; never a value | with a `SecretsAdmin` |
| `GET /api/projects` (+ `PUT`/`DELETE`, `/secrets/{env}`) | the whole projects state | with a `ProjectsAdmin` |
| `DELETE /api/cache[/{preview}]` | `{cleared: […]}` | with a `ProjectsAdmin` |

Every mutating call answers with the fresh state rather than `{"status":"ok"}`, which is why the page has no
separate reload path after a write: `run` renders whatever comes back.

`/events` is not push all the way down — the handler re-reads the daemon's state on a 700 ms ticker per connected
browser, so it is *more* database work than the one-second poll it replaced (`internal/daemon/stream.go:38-58`).
What it bought is on the browser's side: one held connection instead of a request per second, a change visible in
well under a second, and a payload sent only when it differs — which is what stopped the page redrawing itself, and
losing the reader's text selection, twice a second while nothing was happening.

## Layout

```
┌─ sticky top bar ───────────────────────────────────────────────────────────┐
│ docpreview [All|Ready|Building|…] [proj ▾] [search] Projects Secrets       │
├──────────────────────────────────┬─────────────────────────────────────────┤
│ Projects (4)                     │ Activity · all projects                 │
│                                  │ [All|Ready|Building|…]                  │
│ ▶ mydocs   new-install-guide #12 │ 14:22 ● Ready mydocs#12                 │
│   ●3 ●1     [Building] 4s  Open ↗│        next-fixes · 85912e2 ⧉           │
│                                  │ 14:21 ● Building …                      │
│ ▼ handbook api-reference #7      │                                         │
│   [preview ▾] [build ▾]          │                                         │
│   [Open build ↗]                 │                                         │
│   ● streaming this build         │                                         │
│   ┌──────────────────────────┐   │                                         │
│   │ $ yarn build           ⧉ │   │                                         │
│   └──────────────────────────┘   │                                         │
│   [Following][Expand]  [Download]│                                         │
└──────────────────────────────────┴─────────────────────────────────────────┘
```

The two `⧉` are copy controls, and each is hidden until its own row or pane is hovered or focused — see
[the copy controls](#the-copy-controls). The admin links appear only for a request the daemon would accept a write
from.

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
while it has not — a build queued or building *now* still has its row, which is when that row carries information
(`internal/daemon/dashboard.html:1635`). Grouped by preview **and** commit: two attempts of one pull request are two
builds, and collapsing them together would hide the older one's outcome. Dropped rather than merged into one row
with a duration, because the timestamps are the history and a synthesised row has no honest one. Verified against a
real 19-entry history: 13 rows, and all 13 attempts still represented.

**One pass, newest first, which is the order the daemon sends.** A non-terminal entry is dropped only when a
*newer* terminal entry for the same attempt has already been seen. The first version collected every terminal key
up front instead, which dropped a queued entry whose terminal entry was *older* — so re-running a commit that had
already finished lost the row saying the new attempt was waiting, which is the one row somebody watching a rebuild
is looking for. `tools/dashboardtest/feed.mjs` drives that case as "a finished commit queued again", alongside the
report it was written for: while a build was queued the feed showed only the queued entry and everything else came
back once the build started.

**The kind filter bypasses it.** Picking "queued" is asking for exactly what was hidden, so a filter that answered
with nothing would be a filter that lies. The filter counts come from the uncollapsed set for the same reason.

### An entry names its commit, and lends it out

Without the commit a branch built eight times produced eight rows reading `docpreview#3 / next-fixes-3 — ready`,
identical apart from the clock: the one thing a reader wants — *which push was this* — was the one thing the feed
had and did not show. The message is dropped when it only repeats the label, because the daemon sends "ready" for a
ready event and the row said Ready twice; a message carrying more than that ("ready in 24s", a skip reason, a
failure) is kept.

**The rows are `div role="button"`, not `<button>` and not links** (`internal/daemon/dashboard.html:1748`). Two
things a real button made impossible. Text inside one cannot be selected, so the commit could be read on screen and
not copied. And a button cannot contain a button, so there was nowhere to put a copy control. The cost is doing by
hand what the element gave for free: `role`, `tabindex="0"`, and Enter/Space in a `keydown` handler
(`internal/daemon/dashboard.html:1765`). Worth it for a rail whose whole purpose is telling you which commit did
what. `tools/dashboardtest/clicks.mjs` selects the rows by class rather than by tag for exactly this reason.

An entry with nothing left to open is a plain `div` with no role, dimmed, and says so in its tooltip. The **server**
decides that, not the page: `Openable` is set at assembly (`internal/daemon/daemon.go:1494`), because retention
prunes older builds and an entry can name a live preview and a build that is gone. It used to be "does the preview
still exist", checked in the page, which was the right question only while a preview had one build. The preview
check stays as well, since a torn-down preview cannot be revealed whatever the server says about its build. Dimmed
rather than removed, because a build that ran is worth knowing about after its artifacts are pruned — and dimmed
rather than merely inert, because a row identical to its clickable neighbours that does nothing is the thing this
rail was reported for twice.

### An entry opens the build it names, not just the preview

Clicking one used to call `reveal(preview_id)` and ignore the commit — while its own tooltip promised to open the
build. Every preview of one repository shares a project row, so the visible effect was re-picking the preview
*inside* a row that was usually already open, and clicking an entry for the preview already showing did nothing at
all. It read as a dead button, which is what it was reported as.

`reveal` now takes the commit and selects that build in the picker, matching on the build id's suffix — a build id
is `<date>-<time>-<short sha>` and the event carries only the sha. The newest build takes the picker's empty value
rather than its own id, because empty means "follow the live stream" and the newest log is still being written.

**Two ways there is no build to select, and they are not alike** (`selectBuildForCommit`,
`internal/daemon/dashboard.html:1866`). A queued or running build has no log file *yet* and the live stream **is**
that build, so it falls through to the stream and clicking a building row shows it running. Anything else with no
log file has none to show: a skipped build never wrote one, and an old one may have been pruned. Falling through
there silently swapped in the newest build's log, so the picker moved while the pane did not — which is precisely
how this was reported. Now the pane is cleared and the banner says so.

The row also flashes on every click, and the entry that was clicked keeps a selection mark (`.ev.sel`, a left bar in
the entry's own state colour). A click that legitimately changes nothing is indistinguishable from a broken one, and
that case is the common one here rather than the edge. The mark is keyed on the entry's **timestamp**, not on
preview-and-commit: an attempt still running has two entries, queued and building, and keying on the attempt marked
both — so clicking between them changed nothing again. One entry marked means every click moves the mark, which is
what `tools/dashboardtest/clicks.mjs` asserts for every clickable row, along with the pane's first line and the
requirement that the next re-render not lose any of it.

## The activity feed survives a restart

The feed is a 200-entry in-memory ring (`eventLogSize`, `internal/daemon/events.go:18`), so before this it was empty
after every restart — a list headed "recent activity" that forgot everything the moment the process did, which reads
as a broken feed rather than as an empty one. `Daemon.backfill` seeds it from the `builds` table at startup
(`internal/daemon/events.go:113`), before any worker can add to it, so the restored history sits behind this run's
events rather than interleaved with them (`internal/daemon/daemon.go:414`). The status payload carries the newest 60
of the ring, not all 200 (`internal/daemon/daemon.go:1868`).

**Oldest first.** `add` writes into a ring and `recent` walks backwards from the last write, so the newest restored
entry has to be the last one written. `RecentBuilds` returns newest-first, so `backfill` iterates it in reverse
(`internal/daemon/events.go:120`).

Each row is stamped with `finished_at`, falling back to `started_at` for a build that was in flight when the
process died — the time of the transition it describes, which is the rule below. Only the states the `builds` table
records appear; `queued` lives in the job queue and nowhere else. See [08-storage.md](08-storage.md).

## The log pane

Inline in the expanded row, 15 rem by default, `Expand` to 34 rem. The previous layout gave it 30 rem
permanently whether or not a log was open, which pushed everything else off screen. Under 62.5 rem of viewport it is
20 rem and expands to 75 vh, because on a phone there is nothing beside it competing for the screen.

**Exactly one stream is open at a time.** Nothing is fetched for a collapsed project. Several at once would mean
N server-sent-event connections from one tab, and browsers cap concurrent connections per origin — the sixth
open row would hang with no visible reason.

### The view controls moved under the pane

Above the log is what you are looking at; below it is how you are looking at it. The pickers and **Open build** stay
on top, because they are navigation and belong with the row's own controls. **Following**, **Expand** and
**Download** sit in a second bar under the terminal (`.logfoot`, `internal/daemon/dashboard.html:1126-1131`).

They all used to share one bar, which put those three a few pixels from the row's **Open** button — the one control
here anybody reaches for often. So the cost of a misclick was landing on a different preview or losing your place in
a live log. Nothing destructive is adjacent to them now.

### The banner

Above the terminal. The first four come from the server's `start` and `done` events; the last three are the page's
own, for the cases where it went looking for a stored build rather than following the stream:

| Situation | Banner |
|---|---|
| Building | ● streaming this build (green) |
| Queued / Ready / Failed | ● previous build · `20260728-183138-7c6a873` — nothing is running |
| Finished while watched | ● this build finished |
| No log | ● no build log |
| A stored build picked | ● build `20260728-183138-7c6a873` — not live |
| That build unreadable | ● could not read build `…`: 404 Not Found |
| No log kept for a commit | ● no build log was kept for `85912e2` — it was skipped, so nothing was built |

A queued preview replays its last completed build. Without the banner the pane looked like the queued build had
already produced all of it.

The banner is part of the pane and travels with it: `ui.banner` is re-applied when the pane is rebuilt from
`ui.buffer` rather than reconnecting, which would replay the whole log from the top
(`internal/daemon/dashboard.html:1231-1243`).

### The build picker

Beside the preview picker, listing every stored build of the selected preview with an outcome icon and the word
for it — ✅ succeeded, ❌ failed, 🔨 running, ⏳ queued, ⏭️ skipped (`buildIcon` and `buildLabel`,
`internal/daemon/dashboard.html:1437-1457`).

**The word is there as well as the icon.** An emoji alone is unreadable to a screen reader, and at a glance ❌ is
as easily "cancelled" as "failed". The icons are the same ones the rows use, so one glance means the same thing in
both places.

**A build with no recorded row renders as unknown, not hidden** (`internal/daemon/stream.go:390-393`). Logs
predating the `builds` table have no outcome, and so does a build whose row was pruned before its log. The log is
still readable either way, and a picker that silently omits a log it could serve is worse than one that admits it
does not know how the build ended.

**The newest entry keeps the empty value**, which means "follow the live stream" rather than "download this file"
(`internal/daemon/dashboard.html:1382-1395`). A running build's log file is still being written, so reading it as
a file shows a truncated copy that never updates. There is no separate synthetic "latest" row above it: the newest
build *is* the latest, so the extra entry said the same thing twice and made the list one longer than the number
of builds.

**Shown from the first build, not the second** (`internal/daemon/dashboard.html:1379`). It used to be hidden below
two entries, on the reasoning that a one-item picker is useless — hidden rather than greyed out, because a
greyed-out control invites clicking where nothing at all reads as "there is only one build". The reasoning was
wrong: the entry carries the outcome and the age, so with one build it is not a picker at all, it is the only place
the dashboard says whether that build succeeded. Hiding it left a failed build with no visible result beside the
row. It is hidden now only when there are no builds to list.

Fetched on demand from `/logs/{preview}` when a row expands (`loadBuilds`, `internal/daemon/dashboard.html:1352`,
called from `updateBody` at `1213`), not folded into `/status`. A status payload carrying every build of every
preview grows without bound on a busy repository and is re-sent on every state change — for a list nobody is
looking at unless a row is open. The outcomes are joined onto the log metadata server-side (`Ingress.listLogs`,
`internal/daemon/stream.go:394-423`) rather than stored on the log file: the log is bytes on disk, and how a build
ended is the daemon's knowledge.

The rewrite is skipped while the picker is the active element, because rewriting an open `<select>` closes it with
no visible cause (`internal/daemon/dashboard.html:1370`). Skipped rather than stashed in `dataset.pending` as rule 3
does, because this list only changes when a build starts or ends and the next expand refetches it — there is no
repeating tick to keep missing.

### Each build has its own Open

`Open build ↗` sits beside the build picker and goes to the **selected build's** own share, pinned to its commit —
where the row's `Open` goes to the branch share, which follows whatever built last (`updateOpenBuild`,
`internal/daemon/dashboard.html:1407`). One button could never be honest about both, which is why the log pane could
say "build `85912e2` — not live" beside a button that went somewhere else entirely.

The share behind it is published per build, beside the branch share rather than in place of it
(`Daemon.publishBuildShare`, `internal/daemon/daemon.go:1435`), and its name and URL are written onto the build's row
so the picker can offer it (`internal/daemon/stream.go:411`, the `url` on each entry of `GET /logs/{preview}`). The
row has to be rewritten after the share exists, or the next reap treats the share as an orphan
(`internal/daemon/daemon.go:1394-1404`).

**It is best effort, deliberately.** The branch share is the contract and is already live by the time this runs.
Every way the second one can fail — a reserved-name quota on the exposer account, a name collision, an exposer that
cannot mint a second share — is a reason to keep going rather than to fail the build: an error here would turn "you
get one fewer URL than you hoped" into a comment saying the docs did not build. So it logs at warn and returns
empty (`TestBuildShareFailureDoesNotFailTheBuild`).

Which is why the control is **greyed rather than hidden** when the selected build has no URL. Hidden, it would come
and go as the picker moved and read as a rendering fault; greyed with a reason in the tooltip reads as the fact it
is. A build has no share of its own when it failed, when it predates per-build publishing, or when creating the
share did not work.

### Following

On by default. `appendLog` scrolls to the bottom as lines arrive.

The first version had a second, invisible gate: it only scrolled if you were already within 40 px of the bottom.
Scroll up to read and it silently stopped following **while the button still said "Following"**. A control that
lies about its own state is worse than one that does the wrong thing.

Now: scrolling away flips it to Paused, clicking Following jumps to the bottom and re-arms, opening a different
log re-arms it. `ui.autoScrolling` guards the scroll handler against the write `appendLog` just did, which would
otherwise read as the reader scrolling.

Choosing a stored build from the picker turns it off (`showBuild`, `internal/daemon/dashboard.html:1329`). A reader
who has deliberately gone back to an earlier build does not want the live one scrolling in over it.

### Log colouring

Three cases, all structural rather than interpreting the tool's output: a command (`$ …`), something
error-shaped, and a redaction. `*****` is highlighted in amber **deliberately** — so redaction is visibly
happening rather than silently assumed.

## The copy controls

There are two, and they copy different things.

**The log pane's** copy button floats over the terminal rather than sitting in a toolbar, because reaching for it
happens while reading the scrollback — and a log pane has no spare horizontal room in the bar above it. It copies
`innerText`, not `innerHTML`: the pane renders spans for command lines, errors and masks, so the HTML would paste
markup, and the text is what somebody pasting into an issue actually wants — masks included, since `*****` is the
truth about that line (`internal/daemon/dashboard.html:1141-1165`).

**The feed's** copy button sits on the commit rather than on the row, because the commit is what anybody wants out
of the rail: a sha to paste into a terminal or a message. Copying the whole row would hand over the clock and the
branch too and make them edit it back down. Its click calls `stopPropagation`, or the row underneath would open as
well (`internal/daemon/dashboard.html:1772-1790`).

Both are **hidden until their own row or pane is hovered, or the button itself has keyboard focus** — `:focus-within`
rather than `:focus`, so tabbing to one reveals it. One per feed row, permanently visible, turns the rail into a
column of icons and buries the history it exists to show; over the terminal, permanently visible, it covers the first
line of output. Under `@media (hover: none)` the feed's button is always shown, because a touch screen has no hover
to reveal it with, and the same media query enlarges the log's to a real touch target and moves it clear of a narrow
scrollbar.

Both fall back rather than failing silently, because `navigator.clipboard` needs a secure context:
`http://127.0.0.1` qualifies but not every browser agrees, and the dashboard is also reachable over a tunnel. The log
button selects the pane's contents and says "Selected — press Ctrl+C" in its tooltip, since there is no room for
words in an icon button. The feed's says "could not copy — select the text instead", which works precisely because
the row is a `div` and not a `button`. Both show a tick for 1.5 s on success and then go back to the copy icon.

Inline SVG, not an icon font and not 📋: a font is a network request for two glyphs, and the emoji renders as a
different picture on every platform, at a size nothing here controls (`ICON_COPY`,
`internal/daemon/dashboard.html:774`).

## The preview link is on the row

Opening the site is the commonest thing anyone wants from a list of previews; burying it behind a click to
reveal a build log inverts that.

It is a **sibling** of the toggle button, not inside it: an anchor nested in a button is invalid markup and
would both navigate and toggle.

**A URL, not a state, decides whether it is clickable** (`updateGoLink`, `internal/daemon/dashboard.html:1059`). It
used to require `state === "ready"`, which greyed the button on every rebuild — but a rebuild withdraws nothing: the
previous build is still published and still serving, which is the whole point of building the new one beside it
rather than in place. The button went grey exactly when somebody was most likely to be watching a build and wanting
to look at what is live. When the state is not `ready` the tooltip says which build is behind the link, so a
stale-looking page is explained rather than confusing.

A preview with no URL is the other case and still greys: nothing has ever been published for it, or it has been
withdrawn, and rendering that as live sends the reader to an error page. An anchor with no `href` is not a link,
which is exactly the wanted state, and the styling follows from that fact rather than from a `disabled` attribute
anchors ignore. The tooltip says why. That rule is scoped to `.row .go` on purpose: unscoped, `.go:not([href])` also
matched the activity rail's entries — which have no `href` because they are not anchors — and greyed the whole panel
out at 0.3 opacity with `pointer-events: none`. A generic class name in a single stylesheet is a collision waiting
for its second user.

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
(`internal/daemon/daemon.go:1902-1916`):

| Branch | Age comes from |
|---|---|
| stored row + running build | `b.started` |
| stored row + queued job | `j.EnqueuedAt` — this was the stored row's `updated_at` |
| in flight with no stored row | `b.started`, or `j.EnqueuedAt` for a queued one |

`Store.PendingJobs` returns `enqueued_at` alongside the payload to make that possible
(`internal/store/store.go:300`); the column was already there for `Claim`'s ordering.

The third branch — a first build of a branch, which has no stored row anywhere — used `time.Now()`. That is worse
than a stale timestamp: it re-renders as "just now" on every poll, so a job wedged in the queue for an hour looks
freshly arrived forever, which is the one thing that row exists to reveal (`internal/daemon/daemon.go:1937-1953`).
Guarded by `TestStatusReportsQueuedBuilds` and `TestStatusReportsAFirstBuildWithNoStoredRow`.

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
`reconnecting…`. `EventSource` reconnects by itself, so this only has to report. Relative timestamps are redrawn on
a 5-second interval, because the status stream is silent when nothing changes — which is exactly when "2s ago" has
become a lie.

## A tab left open across a restart says so

The page is one embedded file served `no-store` but with no cache busting *inside* it, so a tab left open across a
rebuild keeps running the JavaScript it loaded. That produced three false bug reports in one afternoon — a layout
that had already moved, a filter that had already been fixed, a row that was already collapsed — each costing a
diagnosis before somebody thought to reload.

So `Status` carries an `instance`, and the page compares it against the first one it saw
(`checkInstance`, `internal/daemon/dashboard.html:2701`; the field is `internal/daemon/daemon.go:1819`). On a change
it appends a fixed banner at the bottom: "The daemon restarted. This page is running older code, so what you see may
not match what it does", with a **Reload** button.

**The process start time, not a version stamped at compile time.** A build id cannot tell a restart of the same
binary from no restart at all, and restart is the event that matters. The page compares rather than trusts, so any
change prompts a reload.

**Not an automatic reload.** The reader may be part-way through a log, or have a selection they are copying, and a
page that reloads itself under them is worse than a stale one. It says so and waits — and it appends the banner once,
because a restart during a restart would otherwise stack them.

## Invariants

1. One log stream open at a time, and only for the expanded row.
2. Nothing the reader is focused on or reading is rebuilt or moved.
3. State classes are set unconditionally, outside repaint guards.
4. A control's label matches its behaviour.
5. A preview link is live exactly when there is a URL to serve, never inferred from the state.
6. Every interpolated value is escaped; every URL is scheme-checked.
7. Every timestamp is the time of the transition it labels, never `time.Now()` at assembly.
8. The page never offers a control the server would refuse: no `can_write`, no fields and no buttons; and no admin
   link unless `/api/admin` says this request could use it.
9. Neither admin page opens an `EventSource`.
10. A stored build log is reachable from the picker even when nothing records how its build ended.
11. `/status` carries only what changes on a state change; per-preview detail is fetched on demand.
12. Whatever the page tells the reader it did, it can show: a click that changes nothing else still leaves a mark.
13. A page older than the daemon answering it says so, and never reloads itself.
