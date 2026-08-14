// Opens a row on a preview whose log is gone, then lets the page poll, and counts what it does.
//
// # What it asserts
//
//   - **The stream is opened once, not once per render.** An empty pane has an empty buffer and
//     no open stream, which is the same state as a log nobody has read yet. Without a record of
//     "there is nothing here", the reopen test fires on every render — a request per poll, each
//     answering with the same sentence, for as long as the row stays open.
//   - **The pane is written once.** Assigning innerHTML with identical content rebuilds the
//     subtree, dropping a selection and cancelling a text drag. A pane that says "nothing here"
//     should be the calmest thing on the page.
//   - **It does ask again eventually.** The answer can change on its own: a queued build starts
//     writing and the pane should fill in without anybody clicking. So the record expires.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node emptypane.mjs
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

// No logs at all, which is what the daemon reports once build.keep_logs has passed: the activity
// entry outlives the file it describes.
let logs = [];
const streams = [];
// Every fetch of the build list, which is the request that was firing once per second per open
// row and repainting the controls beside the picker each time it came back.
let listFetches = 0;

// The preview's state, so a queued row can be polled the way a real one is.
let previewState = "ready";

// What /status says about this caller's right to rebuild. The daemon decides it from the same gate
// the POST goes through, so the page must follow it rather than infer one of its own.
let canRebuild = true;

function statusPayload() {
  return {
    exposer: "test", instance: "harness-1", version: "v9.9.9 · abcdef123456",
    can_rebuild: canRebuild, pending: 0, running: 0,
    previews: [{
      preview_id: PREVIEW, repo: `github:${PROJECT}`, number: 7, branch: "feat/x",
      name: "acme-docs-feat-x", url: "https://example.invalid/", state: previewState,
      updated_at: "2026-07-30T09:00:20.000Z", commit: "old1234",
      pr_url: "https://example.invalid/pr/7",
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
    // Answers the way the daemon does when the log is gone: a done event carrying a reason and
    // no start, no line. That is the path that puts the explanation in the pane.
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        if (!/^\/logs\/.+\/stream$/.test(url)) return;
        streams.push(this);
        setTimeout(() => {
          if (this.readyState !== 1) return;
          this.emit("done", JSON.stringify({reason: "no build log", live: false}));
        }, 10);
      }
      emit(type, data) { for (const fn of this.listeners[type] || []) fn({data}); }
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      if (url === `/logs/${PREVIEW}`) listFetches++;
      const body =
        url === "/status" ? statusPayload() :
        url === `/logs/${PREVIEW}` ? {preview_id: PREVIEW, live: false, logs} :
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
  console.log("FATAL: the page's ui object is unreachable; its script did not run");
  process.exit(1);
}

const settle = (ms = 120) => new Promise(r => setTimeout(r, ms));
const applyStatus = async () => {
  win.eval(`applyStatus(${JSON.stringify(statusPayload())})`);
  await settle();
};
const pane = () => win.document.querySelector(".item.open .term");

// Counts assignments to any pane's innerHTML.
//
// Patched on the prototype rather than on one element, because a render can replace the pane and
// a counter bound to the old instance then sees nothing — which is how this assertion passed
// against the very page it was written to fail.
//
// Watched this way rather than with a MutationObserver because a write of identical content
// produces no mutation record, and an assignment that changes nothing observable is precisely
// what this file is about.
let writes = 0;
let counting = false;
function countPaneWrites() {
  // Element, not HTMLElement: that is where innerHTML is defined, and asking the wrong prototype
  // yields an undefined descriptor whose setter then throws from inside every render.
  const proto = win.Element.prototype;
  const desc = Object.getOwnPropertyDescriptor(proto, "innerHTML");
  if (!desc || !desc.set) {
    fail("innerHTML has no setter to watch on Element.prototype");
    return;
  }
  Object.defineProperty(proto, "innerHTML", {
    configurable: true,
    get() { return desc.get.call(this); },
    set(v) {
      // Counted only when the element already holds exactly this, which is the write that buys
      // nothing. A render legitimately paints a pane it has just created, and forbidding that
      // would be a test against the page's whole rendering design rather than against churn.
      if (counting && this.classList && this.classList.contains("term") &&
          desc.get.call(this) === String(v)) {
        writes++;
      }
      desc.set.call(this, v);
    },
  });
  counting = true;
}

console.log("a row opened on a preview whose log has been pruned");

await applyStatus();
// Clicked the way a reader does, rather than calling toggle() with a project key the harness
// would have to guess the spelling of.
win.document.querySelector(".item .head").click();
await settle(300);

const p = pane();
if (!p) {
  console.log("FATAL: no pane after expanding the row");
  process.exit(1);
}

const text = p.textContent || "";
if (!/No build log was kept/.test(text)) {
  fail(`the pane does not explain itself: ${JSON.stringify(text.slice(0, 90))}`);
} else {
  ok("the pane says the log was not kept");
}
// The commit is known here, so the sentence names it. With no commit the wording has to change,
// or it reads "No build log was kept for ." — which is what a pruned entry produced.
if (/kept for \.\s*$/m.test(text) || /kept for \./.test(text)) {
  fail("the sentence names an empty commit: 'No build log was kept for .'");
} else {
  ok("no empty commit in the sentence");
}

const streamsAfterOpen = streams.length;
if (streamsAfterOpen !== 1) {
  fail(`opening the row made ${streamsAfterOpen} stream requests, want 1`);
} else {
  ok("one stream request to discover there is no log");
}

console.log("");
console.log("then the page polls, the way it does every second");

countPaneWrites();
for (let i = 0; i < 6; i++) await applyStatus();

if (streams.length !== streamsAfterOpen) {
  fail(`six polls opened ${streams.length - streamsAfterOpen} more streams, want 0 ` +
    "— an empty pane must not be mistaken for a log nobody has read");
} else {
  ok("six polls opened no further streams");
}
if (writes !== 0) {
  fail(`six polls rewrote the pane ${writes} times with content it already held, want 0`);
} else {
  ok("six polls never rewrote the pane with what it already said");
}
if (!/No build log was kept/.test(pane()?.textContent || "")) {
  fail("the explanation was lost across the polls");
} else {
  ok("the explanation is still on screen");
}

console.log("");
console.log("the build list decides; the stream is not asked what the list already answered");

// Read through a try, so a page without the mechanism reports a failure rather than crashing the
// harness — which is what running this against the version it was written for has to do.
let recheck = NaN;
try { recheck = Number(win.eval("EMPTY_LOG_RECHECK_MS")); } catch { recheck = NaN; }
if (!Number.isFinite(recheck) || recheck < 1000) {
  fail(`EMPTY_LOG_RECHECK_MS is ${recheck}, want at least 1000 — the page has no recheck window`);
} else {
  ok(`the recheck window is ${recheck}ms, longer than the poll interval`);
}

// The window is moved past rather than waited out, so the file runs in milliseconds. Even then no
// stream opens: `/logs/{preview}` has come back empty, and a preview with no builds and nothing
// running has nothing to stream. Asking anyway is the round trip that produced the flicker.
const beforeWindow = streams.length;
win.eval(`if (ui.empty) ui.empty.checkedAt -= ${(recheck || 5000) + 1000}`);
await applyStatus();
if (streams.length !== beforeWindow) {
  fail(`the window elapsing opened ${streams.length - beforeWindow} stream(s) for a preview the ` +
    "build list says has none — the list already answered that");
} else {
  ok("no stream opened for a preview the list says has no logs");
}

console.log("");
console.log("a build appearing in the list is what reopens it");

// The only thing that can mean there is now something to read — and it arrives with a state
// change, because that is how a build comes into existence. A build appearing while the preview
// sits at the same state is not something the daemon can produce: every build moves the row it is
// attached to through queued and building.
logs = [{
  preview_id: PREVIEW, build_id: "20260805-140000-abc1234", size: 2048, state: "ready",
  seconds: 12, mod_time: "2026-08-05T14:00:12.000Z", started_at: "2026-08-05T14:00:00.000Z",
}];
const beforeBuild = streams.length;
previewState = "building";
await applyStatus();
previewState = "ready";
await applyStatus();
await settle(400);

if (streams.length <= beforeBuild) {
  fail("a build appeared in the list and no stream opened to read it");
} else {
  ok("the stream opens once the list has something in it");
}

console.log("");
console.log("a queued preview does not refetch its build list on every poll");

// Queued means no build has started, so the list cannot have changed. Asking anyway was one
// request per second, and each answer repopulated the picker and repainted the controls beside it.
previewState = "queued";
await applyStatus();
const beforeQueued = listFetches;
for (let i = 0; i < 6; i++) await applyStatus();
const queuedFetches = listFetches - beforeQueued;
if (queuedFetches !== 0) {
  fail(`six polls of a queued preview fetched the build list ${queuedFetches} times, want 0`);
} else {
  ok("six polls of a queued preview fetched nothing");
}

// It is asked again the moment the state moves, because that is when a build exists to list.
previewState = "building";
const beforeBuilding = listFetches;
await applyStatus();
if (listFetches <= beforeBuilding) {
  fail("the preview started building and the build list was not refetched");
} else {
  ok("the list is refetched when the state changes to building");
}

// And while building it is throttled rather than asked on every render.
const beforeThrottle = listFetches;
for (let i = 0; i < 6; i++) await applyStatus();
const throttled = listFetches - beforeThrottle;
if (throttled > 1) {
  fail(`six polls of a running preview fetched the list ${throttled} times, want at most 1`);
} else {
  ok(`six polls of a running preview fetched the list ${throttled} time(s)`);
}
// Back to a settled preview, and applied — the page decides from the last payload it saw, so
// changing the fixture without delivering it leaves the row still reading as building.
previewState = "ready";
await applyStatus();

console.log("");
console.log("Rebuild follows what /status says about this caller");

// A viewer signed in through the read-only tunnel may rebuild, and the daemon says so. The page
// showing the control is the whole point of the field: inferring it from the role plus /api/admin
// hid a working button through the tunnel, where /api/admin 404s by design.
const rebuildBtn = () => win.document.querySelector('.item.open [data-role="rebuild"]');
if (!rebuildBtn()) {
  fail("no Rebuild control on the open row");
} else if (rebuildBtn().hidden) {
  fail("Rebuild is hidden even though /status says this caller may rebuild");
} else {
  ok("Rebuild is offered when the daemon says it is allowed");
}

canRebuild = false;
await applyStatus();
if (rebuildBtn() && !rebuildBtn().hidden) {
  fail("Rebuild is offered to a caller the daemon would refuse — the click 404s");
} else {
  ok("Rebuild is hidden when the daemon says no");
}
canRebuild = true;
await applyStatus();

console.log("");
console.log("the running binary's version is on the page");

// Read from /status rather than written into the markup: the markup is the same file in every
// release, so a version baked into it names the build that produced the page and not the one
// serving it — which is the mistake the corner exists to catch.
const stampEl = win.document.getElementById("buildstamp");
if (!stampEl) {
  fail("no build stamp element on the page");
} else if (stampEl.textContent !== "v9.9.9 · abcdef123456") {
  fail(`the stamp reads ${JSON.stringify(stampEl.textContent)}, want the version from /status`);
} else {
  ok("the corner shows the version the daemon reported");
}

// Absent rather than "unknown". A daemon too old to report one should leave the corner empty,
// not print a word that looks like a version.
if (stampEl) {
  win.eval(`applyStatus(${JSON.stringify({
    ...statusPayload(), version: undefined,
  })})`);
  await settle();
  if (stampEl.textContent !== "") {
    fail(`with no version the stamp reads ${JSON.stringify(stampEl.textContent)}, want empty`);
  } else {
    ok("empty when the daemon reports no version");
  }
}

console.log("");
console.log(failures ? `${failures} failure(s)` : "all empty-pane checks OK");
process.exit(failures ? 1 : 0);
