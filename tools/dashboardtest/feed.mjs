// Renders the activity feed under several states and reports how many rows appear.
//
// Checks that a queued build does not hide every other entry in the feed: the daemon
// always sends recent history alongside the queued event, so the feed should show all
// of it. The feed is filtered in three places — the project scope, the kind filter, and
// collapse() — and reading them proves nothing about which one is dropping rows.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/feed.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

import {liveStatus} from "./live.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const daemon = process.env.DOCPREVIEW_URL || "http://127.0.0.1:8471";

// Through live.mjs because /status is behind the login. A 401 body is valid JSON, so
// fetching it directly would hand the page an object with no previews, and the error
// would surface from inside the page instead of from here.
const {status: base, source} = await liveStatus(daemon);
console.log(`state: ${source}\n`);

const vc = new VirtualConsole();
vc.on("jsdomError", e => console.log("PAGE ERROR:", e.message));

// Overridable, as in the other harnesses: an assertion that can only be run against the
// current file cannot show that it would have failed before the change.
const dashboard = process.env.DOCPREVIEW_DASHBOARD ||
  join(here, "..", "..", "internal", "daemon", "dashboard.html");

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously", url: `${daemon}/`, virtualConsole: vc,
  beforeParse(win) {
    win.EventSource = class { constructor() { this.readyState = 1; } close() {} addEventListener() {} };
    win.fetch = async () => ({ok: true, status: 200, statusText: "OK",
      json: async () => ({logs: []}), text: async () => "{}"});
    win.matchMedia = () => ({matches: false, addEventListener() {}});
    win.HTMLElement.prototype.scrollIntoView = function () {};
    win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
    win.requestAnimationFrame = fn => setTimeout(fn, 0);
  },
});

const win = dom.window;
await new Promise(r => win.addEventListener("load", r));
const ui = win.eval("ui");

// An attempt that is queued and nothing more. Deliberately no building event and no
// terminal event for its commit, so a filter that only works once a second event
// exists for the same attempt cannot hide behind one.
function withQueuedOnly(status) {
  const s = structuredClone(status);
  const p = s.previews[0];
  s.previews[0] = {...p, state: "queued"};
  s.events.unshift({
    at: "2099-01-01T00:00:00.000-00:00", kind: "queued", repo: p.repo,
    preview_id: p.preview_id, number: p.number, branch: p.branch,
    commit: "newpush", message: "queued",
  });
  return s;
}

// And the same attempt once it starts, which is when the feed came back.
function withBuilding(status) {
  const s = withQueuedOnly(status);
  s.previews[0] = {...s.previews[0], state: "building"};
  s.events.unshift({...s.events[0], at: "2099-01-01T00:00:01.000-00:00", kind: "building",
    message: "building"});
  return s;
}

// A commit that already finished, queued again — a rebuild after clearing the cache,
// or a retry. The queued row must survive: collapse suppresses a non-terminal entry
// only when a NEWER terminal entry exists for the same attempt, and an older one
// must not count.
function withRequeueOfAFinishedCommit(status) {
  const s = structuredClone(status);
  // Preferring an event on the preview the expanded case scopes to.
  //
  // It took the first `ready` event of any preview, which on the fixture is the only preview
  // and on a live daemon is whichever repository finished first — a different one from the
  // expanded row, so the expanded view legitimately showed no queued entry and the harness
  // reported it as the page dropping it. The scenario is "a finished commit is queued again",
  // not "on whichever preview the data happened to name".
  const first = s.previews[0]?.preview_id;
  const done = s.events.find(e => e.kind === "ready" && e.preview_id === first) ||
    s.events.find(e => e.kind === "ready");
  if (!done) return s;
  s.previews = s.previews.map(p =>
    p.preview_id === done.preview_id ? {...p, state: "queued"} : p);
  s.events.unshift({...done, at: "2099-01-01T00:00:00.000-00:00", kind: "queued",
    message: "queued", url: undefined});
  return s;
}

function rows(status, open) {
  ui.previews = status.previews;
  ui.events = status.events;
  ui.status = status;
  ui.open = open;
  ui.kind = null;
  ui.project = "";
  ui.query = "";
  win.eval("render()");

  const all = [...win.document.querySelectorAll("#events .ev")];
  const kinds = {};
  for (const r of all) {
    const k = r.querySelector(".k")?.textContent || "?";
    kinds[k] = (kinds[k] || 0) + 1;
  }
  return {n: all.length, kinds};
}

const project = win.eval(`project(${JSON.stringify(base.previews[0])})`);
console.log(`project name: ${project}\n`);

let failures = 0;
// The collapsed row count the baseline produced, whatever the data source. Set on the first
// scenario and used as the floor for the rest.
let floor = null;
// "then building" wants **no** queued row.
//
// The feed shows one row per attempt, carrying the state that attempt has reached:
// queued, then building, then success or failed, in place. Appending a row per
// transition would mean three rows for one build and a feed that is mostly its own
// history. A queued row surviving next to the building row for the same attempt is
// the bug this guards against.
//
// The queued-only and requeue cases still want one, because there is no later state for
// that attempt to collapse into.
for (const [name, status, wantQueued] of [
  // The baseline goes first and sets the floor every later scenario is measured against.
  ["baseline (as the daemon has it)", base, 0],
  ["queued only, no building yet", withQueuedOnly(base), 1],
  ["then building", withBuilding(base), 0],
  ["a finished commit queued again", withRequeueOfAFinishedCommit(base), 1],
]) {
  for (const open of [null, project]) {
    const r = rows(status, open);
    const label = `${name} / row ${open ? "expanded" : "collapsed"}`;
    console.log(`${label.padEnd(52)} ${String(r.n).padStart(3)} rows  ` +
      Object.entries(r.kinds).map(([k, v]) => `${k}:${v}`).join(" "));

    const queued = r.kinds.Queued || 0;
    if (queued !== wantQueued) {
      failures++;
      console.log(`   FAIL: ${queued} queued rows shown, want ${wantQueued}`);
    }
    // The history must never shrink because something is in flight. That is the
    // reported symptom, and this is the assertion for it.
    //
    // Compared against the baseline this run measured, not a constant. It was `< 15`, which
    // held against a live daemon with a few days of history and failed against the fixture
    // beside this file — so the harness reported four failures for having less data rather
    // than for anything the page did. A floor that depends on which source answered is not an
    // assertion.
    //
    // Collapsed only. Expanding a row scopes the feed to that one preview, so a handful of
    // rows there is the feature working.
    if (!open) {
      if (floor === null) {
        floor = r.n;
      } else if (r.n < floor) {
        failures++;
        console.log(`   FAIL: ${r.n} rows, down from ${floor} — the history collapsed`);
      }
    }
  }
  console.log(`   (payload carried ${status.events.length} events)`);
}
console.log(failures ? `\n${failures} failure(s)` : "\nno failures");
process.exit(failures ? 1 : 0);
