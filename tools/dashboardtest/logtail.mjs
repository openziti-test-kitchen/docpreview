// Loads the real dashboard in a DOM and watches one build happen underneath an
// expanded row, driving the build picker the way an operator would.
//
// # Why this exists
//
// The dashboard's log pane depends on which of four state objects a render happens to
// see, which is not visible by reading the page. This harness plays the sequence out:
// expand a row while nothing is running, let a build start under it, step back to the
// finished build, then pick the running one out of the dropdown and check whether
// output that arrives *after* that click lands in the pane and whether the pane is
// still following it.
//
// # What it asserts
//
//   - Selecting the running build must turn Following back on. The only other path to
//     a stored build turns following off, and the live entry shares that code path —
//     without an explicit override, lines would arrive and the viewport would never
//     move, which from the reader's side is a log that looks stopped at the instant
//     they selected it.
//   - The pane is only honest about the *newest* build being live. Any other
//     stored build must stay a file download, because /logs/{id}/stream serves
//     whatever is current rather than what was asked for, so "stream when a build
//     is running" would silently show one build's output under another's label.
//
// # Running it
//
// Needs node and jsdom, like every harness here, and needs no daemon: the whole
// point is a build timeline this file controls, which a live daemon cannot be
// asked to produce on cue.
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node logtail.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
// The page under test, overridable so a fix can be checked against the code it
// fixes. A harness that only ever loads the current file cannot tell a real
// assertion from one that would have passed regardless of the change.
const dashboard = process.env.DOCPREVIEW_DASHBOARD ||
  join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

const PREVIEW = "abc123def456";
const PROJECT = "acme/docs";
// Build ids are "<date>-<time>-<short sha>", and the suffix is what the activity
// feed matches on, so they have to look like the real thing.
const OLD = "20260730-090000-old1234";
const NEW = "20260731-120000-new5678";
const THIRD = "20260731-130000-thr9012";

// The server's view, mutated as the timeline advances. Newest first, which is what
// buildlog.Store.List guarantees and what the picker's "index 0 is the live entry"
// rule rests on.
let logs = [
  {preview_id: PREVIEW, build_id: OLD, size: 4096, state: "ready", seconds: 20,
   mod_time: "2026-07-30T09:00:20.000Z", started_at: "2026-07-30T09:00:00.000Z"},
];
let liveNow = false;
const fetched = [];
const streams = [];

function statusPayload(state, at) {
  return {
    exposer: "test", instance: "harness-1", pending: 0, running: liveNow ? 1 : 0,
    previews: [{
      preview_id: PREVIEW, repo: `github:${PROJECT}`, number: 7, branch: "feat/x",
      name: "acme-docs-feat-x", url: "https://example.invalid/", state,
      updated_at: at, pr_url: "https://example.invalid/pr/7",
    }],
    events: [{
      repo: `github:${PROJECT}`, preview_id: PREVIEW, number: 7, branch: "feat/x",
      at: "2026-07-30T09:00:20.000Z", kind: "ready", commit: "old1234",
      message: "ready in 20s", openable: true,
    }],
  };
}

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));
vc.on("error", (...a) => fail(`console.error: ${a.map(String).join(" ")}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/",
  virtualConsole: vc,
  beforeParse(win) {
    // A stream that behaves like the daemon's, because the distinction this whole
    // file turns on is invisible otherwise: a *stored* build is a file that ends,
    // and the live one keeps arriving. So the stub announces which it is with a
    // start event and, when live, stays open so the harness can push a line in
    // after the click and see whether the pane grows.
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        if (!/^\/logs\/.+\/stream$/.test(url)) return;
        streams.push(this);
        const build = logs[0]?.build_id || "";
        setTimeout(() => {
          if (this.readyState !== 1) return;
          if (liveNow) {
            this.emit("start", JSON.stringify({live: true, build_id: build}));
            this.emit("line", `STREAM-HEAD ${build}`);
            // Deliberately no done: a live build's stream is still open, which is
            // the state every assertion below is about.
          } else {
            this.emit("start", JSON.stringify({live: false, build_id: build, seconds: 20}));
            this.emit("line", `REPLAY ${build}`);
            this.emit("done", JSON.stringify({live: false}));
          }
        }, 15);
      }
      emit(type, data) { for (const fn of this.listeners[type] || []) fn({data}); }
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
      close() { this.readyState = 2; }
    };
    // Pushes one line into whichever log stream is currently open — the build
    // writing another line. Nothing else in the harness can distinguish a pane
    // that is tailing from a pane that merely rendered once.
    win.__tail = text => {
      const src = streams.filter(s => s.readyState === 1).pop();
      if (!src) return false;
      src.emit("line", text);
      return true;
    };
    win.fetch = async url => {
      fetched.push(url);
      // A stored build is a download of plain text. Its body says "snapshot" so a
      // pane filled from the file is distinguishable from one fed by the stream —
      // without this mark a truncated read of a running build's log would look
      // identical to a working live tail.
      const dl = url.match(/^\/logs\/([^/]+)\/download\/(.+)$/);
      if (dl) {
        const build = decodeURIComponent(dl[2]);
        return {ok: true, status: 200, statusText: "OK",
          text: async () => `SNAPSHOT ${build}\nsecond line of ${build}\n`,
          json: async () => ({})};
      }
      const body =
        url === "/status" ? statusPayload("ready", "2026-07-30T09:00:20.000Z") :
        url === `/logs/${PREVIEW}` ? {preview_id: PREVIEW, live: liveNow, logs} :
        url === "/api/admin" ? {secrets: true, projects: true} : {};
      return {ok: true, status: 200, statusText: "OK",
        json: async () => body, text: async () => JSON.stringify(body)};
    };
    win.matchMedia = () => ({matches: false, addEventListener() {}});
    win.HTMLElement.prototype.scrollIntoView = function () {};
    win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
    win.requestAnimationFrame = fn => setTimeout(fn, 0);
    win.getSelection = () => ({isCollapsed: true});
  },
});

const win = dom.window;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 200));

// `const ui = {...}` at the top level of a classic script is a lexical binding and
// not a property of window, so the only way to it is the page's own scope.
const ui = win.eval("ui");
if (!ui) {
  console.log("FATAL: the page's ui object is unreachable; its script did not run");
  process.exit(1);
}

const settle = (ms = 150) => new Promise(r => setTimeout(r, ms));
const apply = async (state, at) => {
  win.eval(`applyStatus(${JSON.stringify(statusPayload(state, at))})`);
  await settle();
};

const buildSel = () => win.document.querySelector('.item.open [data-role="build"]');
const pane = () => win.document.querySelector(".item.open .term");

const observe = () => {
  const sel = buildSel();
  const follow = win.document.querySelector('.item.open [data-role="follow"]');
  return {
    selected: sel ? (sel.value || "(live)") : "no picker",
    options: sel ? [...sel.options].map(o => `${o.value || "(live)"}`) : [],
    labels: sel ? [...sel.options].map(o => o.textContent) : [],
    banner: win.document.querySelector(".logstate")?.textContent?.trim() || "",
    kind: ui.banner?.kind || "",
    streaming: !!ui.logSource,
    follow: ui.follow,
    followBtn: follow?.textContent || "none",
    text: pane()?.textContent || "",
  };
};

// Picks an option the way a reader does — by the build it names, not by knowing
// what value the page put behind it. Which value that is happens to be the thing
// under test, so the harness must not assume it.
function choose(buildID) {
  const sel = buildSel();
  const opt = [...sel.options].find(o => o.textContent.includes(buildID));
  if (!opt) { fail(`the picker has no option naming ${buildID}`); return false; }
  sel.value = opt.value;
  sel.dispatchEvent(new win.Event("change", {bubbles: true}));
  return true;
}

/* ── 1. A row expanded while nothing is running ─────────────────────────── */

await apply("ready", "2026-07-30T09:00:20.000Z");
win.document.querySelector(".item .head").click();
await settle(250);

{
  const s = observe();
  console.log("\n1. row expanded, nothing running");
  console.log(`   picker=${JSON.stringify(s.options)} banner="${s.banner}"`);
  console.log(`   pane="${s.text.trim().split("\n")[0]}"`);
  if (!buildSel()) fail("no build picker in the expanded row");
  if (!/REPLAY/.test(s.text)) fail(`the pane did not replay the last build: "${s.text.trim()}"`);
}

/* ── 2. A build starts underneath the open row ──────────────────────────── */
//
// A push arrives with nothing clicked and no reload. The picker has to learn about
// the new build on its own, because the only other way to see it is a reload.

logs = [{preview_id: PREVIEW, build_id: NEW, size: 128, state: "building",
         mod_time: new Date().toISOString(), started_at: new Date().toISOString()},
        ...logs];
liveNow = true;
await apply("building", new Date().toISOString());
await settle(400);

{
  const s = observe();
  console.log("\n2. a build started under the open row");
  console.log(`   picker=${JSON.stringify(s.options)}`);
  console.log(`   labels=${JSON.stringify(s.labels)}`);
  console.log(`   banner="${s.banner}" streaming=${s.streaming}`);
  if (!s.labels.some(l => l.includes(NEW))) {
    fail(`the picker never listed the build that started (${NEW}) without a reload`);
  } else {
    ok(`the picker listed ${NEW} without a reload`);
  }
  if (!s.labels.some(l => /running/i.test(l))) {
    fail(`the picker lists the new build but does not say it is running: ` +
      JSON.stringify(s.labels));
  }
}

/* ── 3. Back to the finished build ─────────────────────────────────────── */
//
// A stored build must be a file, and must stop following: a reader who
// deliberately went back does not want the live one scrolling in over it. This
// step is here to set that state up for step 4, and to prove the fix did not
// turn every selection into a stream.

if (choose(OLD)) {
  await settle(200);
  const s = observe();
  console.log("\n3. selected the finished build");
  console.log(`   selected=${s.selected} banner="${s.banner}" follow=${s.followBtn}`);
  if (!/SNAPSHOT/.test(s.text)) {
    fail(`a stored build should be read as a file, pane says "${s.text.trim().split("\n")[0]}"`);
  }
  if (s.streaming) fail("a stored build left a stream open; it will scroll the live build in");
  if (s.follow) fail("selecting an older build left Following on");
  win.__tail(`INTRUDER ${NEW}`);
  await settle(80);
  if (/INTRUDER/.test(observe().text)) {
    fail("the live build's output reached a pane showing a stored build");
  }
}

/* ── 4. Now show the running build, from the dropdown ──────────────────── */
//
// Everything that matters here is about what happens *after* the click: a pane that
// renders the log so far and then sits there is precisely what a file read looks
// like, and it is indistinguishable from a working stream until another line arrives.

if (choose(NEW)) {
  await settle(200);
  const before = observe();
  const grew = win.__tail(`TAIL-1 ${NEW}`);
  await settle(120);
  const s = observe();

  console.log("\n4. selected the running build from the dropdown");
  console.log(`   selected=${s.selected} banner="${s.banner}" streaming=${s.streaming}`);
  console.log(`   follow=${s.followBtn} pane first line="${s.text.trim().split("\n")[0]}"`);

  if (/SNAPSHOT/.test(before.text)) {
    fail("the running build was read as a stored file — the pane shows the log up " +
      "to that instant and then stops");
  }
  if (!s.streaming) fail("no stream is open for the running build");
  if (!grew) fail("nothing was streaming, so no line could be pushed in");
  if (!/TAIL-1/.test(s.text)) {
    fail("a line written after the selection never reached the pane — the pane is " +
      "not tailing the build it says it is showing");
  } else {
    ok("output written after the selection reached the pane");
  }
  // Following, and not merely a stream. A paused pane grows downwards out of
  // sight, so the reader sees the log frozen at the instant they selected it
  // while lines pile up below the fold — the same symptom, one layer down, and
  // the reason this assertion is separate from the one above.
  if (!s.follow) {
    fail("the pane is streaming but not following: lines land below the fold and " +
      "the visible log never moves, which reads as a log that stopped");
  } else {
    ok("the pane is following, so the newest line is the one on screen");
  }
  if (s.kind !== "live") fail(`the banner says "${s.banner}", not that this is live`);
}

/* ── 5. A second build, while the picker has focus ─────────────────────── */
//
// The picker is deliberately not rewritten while it has focus, because rewriting
// a <select> closes its open dropdown with no visible cause. That is right and
// must stay — but the reader is then looking at a list that is missing the build
// they can see running on the row, so the page has to catch up the moment focus
// leaves rather than waiting for anything else.

buildSel().focus();
logs = [{preview_id: PREVIEW, build_id: THIRD, size: 64, state: "building",
         mod_time: new Date().toISOString(), started_at: new Date().toISOString()},
        ...logs];
await apply("building", new Date().toISOString());
await settle(400);

{
  const held = observe();
  console.log("\n5. a further build started while the picker had focus");
  console.log(`   while focused: ${JSON.stringify(held.options)}`);
  if (held.labels.some(l => l.includes(THIRD))) {
    fail("the picker was rewritten while it had focus, which closes an open dropdown");
  }
  buildSel().blur();
  // Short on purpose. The one-second timestamp tick would catch this up on its own,
  // and a wait long enough to include it would assert nothing: the reader is looking
  // at the list *now*, and "it appears a second after you stop touching it" is still
  // a stale picker, not a fix.
  await settle(150);
  const s = observe();
  console.log(`   after blur:    ${JSON.stringify(s.options)}`);
  if (!s.labels.some(l => l.includes(THIRD))) {
    fail(`the picker still does not list ${THIRD} once the dropdown was released`);
  } else {
    ok("the picker caught up as the dropdown was released");
  }
}

/* ── 6. A caller that names the live build by its id ────────────────────── */
//
// The picker spells the live build as an empty value, and index 0 taking that value
// is the only reason step 4 reaches the stream at all. That invariant is written in
// two places — the option values here and the `i === 0 ? "" : id` in
// selectBuildForCommit — and if either drifts, the pane silently reads a file the
// build is still writing. So the routing decision is asserted on the id directly.

// ui.build is set alongside the call, because every real caller sets it: it is the
// record of what the reader asked for, and the reconnect guard in updateBody reads it
// to decide whether a newly building preview should pull the pane back to the live
// stream. A call without it is a call the next status tick is entitled to undo.
{
  ui.build[PREVIEW] = THIRD;
  win.eval(`showBuild(${JSON.stringify(PREVIEW)}, ${JSON.stringify(THIRD)})`);
  await settle(200);
  win.__tail(`TAIL-2 ${THIRD}`);
  await settle(120);
  const s = observe();
  console.log("\n6. showBuild called with the running build's own id");
  console.log(`   banner="${s.banner}" streaming=${s.streaming} follow=${s.followBtn}`);
  if (/SNAPSHOT/.test(s.text)) {
    fail("the running build was downloaded as a file rather than followed");
  }
  if (!/TAIL-2/.test(s.text)) fail("output written after the call never reached the pane");
  else ok("an id naming the running build follows the stream");
  if (!s.follow) fail("the pane is not following the build it is streaming");

  // And the other half, which is why this is not "stream whenever a build is
  // running": every id that is not the live one stays a download. The stream would
  // serve the *current* build's output under this build's label.
  ui.build[PREVIEW] = OLD;
  win.eval(`showBuild(${JSON.stringify(PREVIEW)}, ${JSON.stringify(OLD)})`);
  await settle(200);
  const t = observe();
  if (!/SNAPSHOT/.test(t.text)) {
    fail(`a finished build stopped being a file read: "${t.text.trim().split("\n")[0]}"`);
  } else {
    ok("a finished build is still read as a file, while another build runs");
  }
  if (t.streaming) fail("a finished build left the live stream open behind it");
}

console.log(failures ? `\n${failures} failure(s)` : `\nall good`);
process.exit(failures ? 1 : 0);
