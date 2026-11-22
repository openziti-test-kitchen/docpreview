// Checks that the activity feed's kind chips count each build once, by the state it has reached.
//
// The feed keeps every transition a build goes through, so one finished build leaves "queued",
// "building" and "ready" behind. The chips count what collapse() keeps, one row per attempt, so
// a finished build never shows under "Building".
//
// Built from its own events rather than status.fixture.json's, so each case controls exactly
// which transitions exist.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/feedchips.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));

const vc = new VirtualConsole();
vc.on("jsdomError", e => console.log("PAGE ERROR:", e.message));

// overridable, to run the assertions against another copy of the dashboard
const dashboard = process.env.NFPREVIEW_DASHBOARD ||
  join(here, "..", "..", "internal", "daemon", "dashboard.html");

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously", url: "http://127.0.0.1:8471/", virtualConsole: vc,
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

const repo = "github:example/site";

function preview(id, number, state) {
  return {preview_id: id, repo, number, branch: `b${number}`, name: `site-b${number}`,
    url: `https://site-b${number}.shares.zrok.io/`, state, updated_at: "2099-01-01T00:00:10.000Z"};
}

// One attempt's transitions, oldest first, each a second after the last. Returned newest first,
// which is the order the daemon sends.
function attempt(id, number, commit, kinds, startSecond) {
  return kinds.map((kind, i) => ({
    at: new Date(Date.UTC(2099, 0, 1, 0, 0, startSecond + i)).toISOString(),
    kind, repo, preview_id: id, number, branch: `b${number}`, commit, message: kind,
  })).reverse();
}

function load(previews, events, kind = null) {
  ui.previews = previews;
  ui.events = events.sort((a, b) => new Date(b.at) - new Date(a.at));
  ui.status = {exposer: "zrok2", pending: 0, running: 0, previews, events};
  ui.open = null;
  ui.project = "";
  ui.query = "";
  ui.kind = kind;
  win.eval("render()");
}

// The chips as {label: count}, keyed by the visible label ("Building", "Ready", "All").
function chips() {
  const out = {};
  for (const b of win.document.querySelectorAll("#feed-filters button")) {
    const n = Number(b.querySelector(".c")?.textContent);
    const label = b.textContent.replace(/\s*\d+\s*$/, "").trim();
    out[label] = n;
  }
  return out;
}

function rowKinds() {
  return [...win.document.querySelectorAll("#events .ev")]
    .map(r => r.querySelector(".k")?.textContent || "?");
}

let failures = 0;
function check(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g === w) {
    console.log(`  ok    ${name}`);
  } else {
    failures++;
    console.log(`  FAIL  ${name}\n        got  ${g}\n        want ${w}`);
  }
}

// Three builds that all finished: two ready, one failed. Nine events, three attempts.
const finished = [
  ...attempt("p1", 1, "aaa1111", ["queued", "building", "ready"], 0),
  ...attempt("p2", 2, "bbb2222", ["queued", "building", "ready"], 10),
  ...attempt("p3", 3, "ccc3333", ["queued", "building", "failed"], 20),
];
const finishedPreviews = [preview("p1", 1, "ready"), preview("p2", 2, "ready"),
  preview("p3", 3, "failed")];

load(finishedPreviews, structuredClone(finished));
const settled = chips();
check("finished builds: no Building or Queued chip",
  [settled.Building, settled.Queued], [undefined, undefined]);
check("finished builds: one chip count per build", settled, {All: 3, Failed: 1, Ready: 2});

// the same history plus a fourth build that is running now, which must still get a "Building" chip
const running = [...structuredClone(finished),
  ...attempt("p4", 4, "ddd4444", ["queued", "building"], 30)];
const runningPreviews = [...finishedPreviews, preview("p4", 4, "building")];

load(runningPreviews, structuredClone(running));
check("a running build: Building 1, and still no Queued chip",
  [chips().Building, chips().Queued], [1, undefined]);

load(runningPreviews, structuredClone(running), "building");
check("Building selected: lists only the running build", rowKinds().length, 1);

// that build finishes while "Building" is selected. The chip disappears and the filter clears
// with it. Otherwise the feed is empty and there is no chip left to press.
const done = [...structuredClone(running),
  ...attempt("p4", 4, "ddd4444", ["ready"], 40)];
ui.previews = [...finishedPreviews, preview("p4", 4, "ready")];
ui.events = done.sort((a, b) => new Date(b.at) - new Date(a.at));
win.eval("render()");
check("selected kind gone: the filter clears", ui.kind, null);
check("selected kind gone: the feed shows every build", rowKinds().length, 4);

console.log(failures ? `\n${failures} failure(s)` : "\nno failures");
process.exit(failures ? 1 : 0);
