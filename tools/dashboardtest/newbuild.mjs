// A build starts while somebody is watching, and the pane has to follow it — on its own.
//
// # Why this exists when logtail.mjs already opens a row and starts a build under it
//
// logtail asserts the *picker* learns about the new build, and stops there. This file asserts
// the *pane*: that it starts tailing the new build without a reload. A bug that a reload fixes
// is state the render path fails to recompute, which reading the code does not reveal — so this
// file reproduces the sequence instead, in the three shapes it actually happens in:
//
//   A. The row is open on the previous build's replay — the picker says "(live)" — and a push
//      arrives. The pane must switch to the new build and follow it.
//   B. The row is open on a *chosen* older build. The pane must NOT be yanked away; the picker
//      must still gain the new build, so the reader can go to it.
//   C. Two builds in a row, the second arriving while the first is still streaming. The pane must
//      end on the second.
//
// # The one thing to be careful of in this file
//
// The daemon announces a new build through a **status event**, not through anything the page
// asked for. So every step here goes through `applyStatus`, the same function the EventSource
// handler calls, and never through a click. A test that clicks is testing something that already
// works.
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node newbuild.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = process.env.DOCPREVIEW_DASHBOARD ||
  join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

const PREVIEW = "abc123def456";
const PROJECT = "acme/docs";
const OLD = "20260730-090000-old1234";
const NEW = "20260731-120000-new5678";
const THIRD = "20260731-130000-thr9012";

// The server's view, mutated as the timeline advances. Newest first, which is what
// buildlog.Store.List guarantees.
let logs = [
  {preview_id: PREVIEW, build_id: OLD, size: 4096, state: "ready", seconds: 20,
   mod_time: "2026-07-30T09:00:20.000Z", started_at: "2026-07-30T09:00:00.000Z"},
];
let liveNow = false;
let liveBuild = "";
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
      at, kind: state === "building" ? "building" : "ready",
      commit: state === "building" ? liveBuild.slice(-7) : "old1234",
      message: "", openable: true,
    }],
  };
}

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/",
  virtualConsole: vc,
  beforeParse(win) {
    // The log stream, behaving as the daemon's does: a *stored* build replays and ends, the
    // live one stays open. Which of the two it is decides everything the pane does, and it is
    // announced by the `start` event rather than inferred.
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        if (!/^\/logs\/.+\/stream$/.test(url)) return;
        streams.push(this);
        setTimeout(() => {
          if (this.readyState !== 1) return;
          if (liveNow) {
            this.emit("start", JSON.stringify({live: true, build_id: liveBuild}));
            this.emit("line", `STREAM-HEAD ${liveBuild}`);
          } else {
            const b = logs[0]?.build_id || "";
            this.emit("start", JSON.stringify({live: false, build_id: b, seconds: 20}));
            this.emit("line", `REPLAY ${b}`);
            this.emit("done", JSON.stringify({live: false}));
          }
        }, 15);
      }
      emit(type, data) { for (const fn of this.listeners[type] || []) fn({data}); }
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      const dl = url.match(/^\/logs\/([^/]+)\/download\/(.+)$/);
      if (dl) {
        const build = decodeURIComponent(dl[2]);
        return {ok: true, status: 200, statusText: "OK",
          text: async () => `SNAPSHOT ${build}\n`, json: async () => ({})};
      }
      const body =
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
const ui = win.eval("ui");
if (!ui) {
  console.log("FATAL: the page's ui object is unreachable");
  process.exit(1);
}

const settle = (ms = 250) => new Promise(r => setTimeout(r, ms));
const apply = async (state, at) => {
  // Through applyStatus, because that is what the EventSource handler calls. Clicking would
  // test a path that already works.
  win.eval(`applyStatus(${JSON.stringify(statusPayload(state, at))})`);
  await settle();
};

const buildSel = () => win.document.querySelector('.item.open [data-role="build"]');
const pane = () => win.document.querySelector(".item.open .term");
const observe = () => ({
  selected: buildSel() ? (buildSel().value || "(live)") : "no picker",
  options: buildSel() ? [...buildSel().options].map(o => o.value || "(live)") : [],
  banner: win.document.querySelector(".logstate")?.textContent?.trim() || "",
  kind: ui.banner?.kind || "",
  streaming: !!ui.logSource,
  follow: ui.follow,
  head: (pane()?.textContent || "").trim().split("\n")[0] || "(empty)",
  last: (pane()?.textContent || "").trim().split("\n").pop() || "(empty)",
});

// startBuild advances the server's view the way a push does: a new log entry, live, and a
// status event announcing the preview as building.
async function startBuild(id) {
  logs = [{preview_id: PREVIEW, build_id: id, size: 0, state: "building",
           mod_time: new Date().toISOString(), started_at: new Date().toISOString()},
          ...logs];
  liveNow = true;
  liveBuild = id;
  await apply("building", new Date().toISOString());
  await settle(400);
}

console.log("A. the row is open on the last build, and a push arrives");
{
  await apply("ready", "2026-07-30T09:00:20.000Z");
  win.document.querySelector(".item .head").click();
  await settle(300);

  const before = observe();
  if (!/REPLAY/.test(before.head)) {
    fail(`the row did not open on the previous build: "${before.head}"`);
  } else ok(`open on the previous build: "${before.head}"`);

  await startBuild(NEW);

  // The three things a reader checks, in the order they look at them.
  const s = observe();
  if (!s.options.includes(NEW) && s.selected !== "(live)") {
    fail(`the picker never learned about ${NEW}: ${JSON.stringify(s.options)}`);
  } else ok("the picker knows about the new build");

  if (!s.streaming) {
    fail("no stream is open, so nothing is being tailed");
  } else ok("a stream is open");

  // The pane must show the *new* build without anything being clicked.
  if (!s.head.includes(NEW) && !s.last.includes(NEW)) {
    fail(`the pane is still showing "${s.head}" — it never switched to ${NEW}`);
  } else ok(`the pane switched to ${NEW} on its own`);

  if (s.follow !== true) {
    fail(`Following is ${s.follow}, so new lines will not scroll into view`);
  } else ok("and it is following");
}

console.log("\nB. the reader chose an older build, and a push arrives");
{
  // The opposite requirement, and the reason A cannot simply always reconnect: a reader
  // part-way through an older build's log must not be yanked off it. What they need is for the
  // picker to gain the new build so they can go to it when they are ready.
  logs = [{preview_id: PREVIEW, build_id: OLD, size: 4096, state: "ready", seconds: 20,
           mod_time: "2026-07-30T09:00:20.000Z", started_at: "2026-07-30T09:00:00.000Z"}];
  liveNow = false;
  liveBuild = "";
  ui.build = {};
  await apply("ready", "2026-07-30T09:00:20.000Z");
  await settle(300);

  // Choose the stored build by name, the way a reader does.
  const sel = buildSel();
  const opt = [...sel.options].find(o => o.textContent.includes(OLD));
  if (!opt) {
    fail(`the picker has no option naming ${OLD}`);
  } else {
    sel.value = opt.value;
    sel.dispatchEvent(new win.Event("change", {bubbles: true}));
    await settle(300);

    const chosen = observe();
    await startBuild(THIRD);
    const s = observe();

    if (s.selected !== chosen.selected) {
      fail(`the reader was moved from ${chosen.selected} to ${s.selected}`);
    } else ok("the chosen build is still selected");
    if (!s.options.includes(THIRD) && !s.options.includes("(live)")) {
      fail(`no way to reach the new build: ${JSON.stringify(s.options)}`);
    } else ok("the new build is reachable from the picker");
  }
}

console.log("\nC. the real transition: ready, then queued, then building");
{
  // A push does not go straight to building. The daemon queues it, and the row shows Queued for
  // as long as a worker is busy — which on a loaded daemon is most of the time. Every earlier
  // reproduction skipped that state, and a guard that fires on a transition can be spent by the
  // wrong one.
  logs = [{preview_id: PREVIEW, build_id: OLD, size: 4096, state: "ready", seconds: 20,
           mod_time: "2026-07-30T09:00:20.000Z", started_at: "2026-07-30T09:00:00.000Z"}];
  liveNow = false;
  liveBuild = "";
  ui.build = {};
  ui.open = null;
  win.eval("closeLog()");
  await apply("ready", "2026-07-30T09:00:20.000Z");
  win.document.querySelector(".item .head").click();
  await settle(300);

  await apply("queued", new Date().toISOString());
  await startBuild(NEW);

  const s = observe();
  if (!s.streaming) {
    fail("no stream after queued → building");
  } else ok("a stream is open");
  if (!s.head.includes(NEW) && !s.last.includes(NEW)) {
    fail(`the pane shows "${s.head}" after queued → building, not ${NEW}`);
  } else ok("the pane followed through the queued state");
}

console.log("\nD. the preview's first build, with no previous log to open");
{
  // The case a fixture never produces and every real repository starts in: a pull request whose
  // preview exists and has never built. The row opens on nothing — there is no log to replay —
  // and then a build starts.
  //
  // This is the one that matters most: it is every new pull request, and it is the first
  // impression of the whole product.
  logs = [];
  liveNow = false;
  liveBuild = "";
  ui.build = {};
  ui.open = null;
  win.eval("closeLog()");
  await apply("queued", new Date().toISOString());
  win.document.querySelector(".item .head").click();
  await settle(300);

  const before = observe();
  console.log(`   opened with pane "${before.head}", logFor=${ui.logFor || "(none)"}`);

  await startBuild(NEW);
  const s = observe();

  if (!s.streaming) {
    fail("no stream, so the first build of a preview is never tailed");
  } else ok("a stream is open");
  if (!s.head.includes(NEW) && !s.last.includes(NEW)) {
    fail(`the pane shows "${s.head}" — the first build was never tailed`);
  } else ok("the first build is tailed");
}

console.log("\nE. one stream, two builds: the server switches it from replay to live");
{
  // The seam no harness has touched, and the strongest suspect.
  //
  // `/logs/{id}/stream` does not close after replaying a finished build if the daemon is
  // *expecting* another one — it replays, sends `done`, waits in `awaitLive`, and then sends a
  // **second `start`, this time live**, on the same connection. That is deliberate and the
  // comment in stream.go explains why: the page cannot win the race, the server can.
  //
  // So the page has to handle a start-after-done on a stream it already thinks is finished:
  // clear the previous build's lines, re-arm Following, and change the banner. Every previous
  // harness sent exactly one `start` per stream, so none of that was ever exercised — and this
  // is the sequence a reader hits by opening a log and then pushing, or by pressing Rebuild.
  logs = [{preview_id: PREVIEW, build_id: OLD, size: 4096, state: "ready", seconds: 20,
           mod_time: "2026-07-30T09:00:20.000Z", started_at: "2026-07-30T09:00:00.000Z"}];
  liveNow = false;
  liveBuild = "";
  ui.build = {};
  ui.open = null;
  win.eval("closeLog()");
  await apply("ready", "2026-07-30T09:00:20.000Z");
  win.document.querySelector(".item .head").click();
  await settle(300);

  const opened = streams[streams.length - 1];
  if (!opened) {
    fail("no log stream was opened when the row expanded");
  } else {
    const before = observe();
    if (!/REPLAY/.test(before.head)) {
      fail(`expected the replay first, got "${before.head}"`);
    }

    // The server's switch, on the same connection.
    opened.emit("start", JSON.stringify({live: true}));
    opened.emit("line", `STREAM-HEAD ${NEW}`);
    await settle(200);
    opened.emit("line", `building ${NEW} …`);
    await settle(250);

    const s = observe();
    console.log(`   pane head="${s.head}" last="${s.last}" follow=${s.follow} kind=${s.kind}`);

    // The previous build's output must be gone. Left in place, the new build's lines land
    // *below* a screen of the old one — which is a pane that looks stuck at the top.
    if (/REPLAY/.test(s.head)) {
      fail("the previous build's replay is still on screen under the live output");
    } else ok("the replayed build was cleared");

    if (!s.last.includes(NEW)) {
      fail(`the newest line is "${s.last}", not the live build's`);
    } else ok("the live output is in the pane");

    // The one a reader actually notices: lines arriving while the viewport does not move is
    // indistinguishable from lines not arriving.
    if (s.follow !== true) {
      fail("Following is off, so the pane does not scroll as lines arrive — this is the bug");
    } else ok("it is following");

    if (s.kind !== "live") {
      fail(`the banner says ${JSON.stringify(s.banner)} rather than announcing a live build`);
    } else ok(`the banner reads "${s.banner}"`);
  }
}

console.log(failures === 0 ? "\nall new-build checks OK" : `\n${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
