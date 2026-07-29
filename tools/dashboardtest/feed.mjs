// Renders the activity feed under several states and reports how many rows appear.
//
// Written for a specific report: while a build is queued the feed showed only the
// queued entry, and everything else came back once the build started. The feed is
// filtered in three places — the project scope, the kind filter, and collapse() —
// and reading them proves nothing about which one is dropping rows.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/feed.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const daemon = process.env.DOCPREVIEW_URL || "http://127.0.0.1:8471";

let base;
try {
  base = await (await fetch(`${daemon}/status`)).json();
  console.log(`state: live, from ${daemon}\n`);
} catch {
  base = JSON.parse(readFileSync(join(here, "status.fixture.json"), "utf8"));
  console.log("state: fixture\n");
}

const vc = new VirtualConsole();
vc.on("jsdomError", e => console.log("PAGE ERROR:", e.message));

const dom = new JSDOM(readFileSync(join(here, "..", "..", "internal", "daemon", "dashboard.html"), "utf8"), {
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

// The reported scenario: an attempt that is queued and nothing more. Deliberately
// no building event and no terminal event for its commit — the earlier harness
// always added a building entry beside the queued one, which is why it never
// reproduced this.
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
  const done = s.events.find(e => e.kind === "ready");
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
for (const [name, status, wantQueued] of [
  ["baseline (as the daemon has it)", base, 0],
  ["queued only, no building yet", withQueuedOnly(base), 1],
  ["then building", withBuilding(base), 1],
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
    if (r.n < 15) {
      failures++;
      console.log(`   FAIL: only ${r.n} rows — the history collapsed`);
    }
  }
  console.log(`   (payload carried ${status.events.length} events)`);
}
console.log(failures ? `\n${failures} failure(s)` : "\nno failures");
process.exit(failures ? 1 : 0);
