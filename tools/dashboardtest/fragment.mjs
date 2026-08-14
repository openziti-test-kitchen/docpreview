// Loads the page at `#preview=<id>` and checks that the deep link opens that preview's log.
//
// # Why this file exists
//
// The pull request comment links its status word to `<dashboard>/#preview=<id>` while the build is
// running, so somebody who has just pushed can watch the log arrive. Three things have to line up
// for that link to work and none of them is visible from reading one file: the daemon has to put a
// DetailURL on a building report, the comment has to render it, and the page has to act on the
// fragment for a preview that is *building* rather than finished.
//
// This covers the third. A preview in flight has no build in the list yet — the log is being
// written as the page loads — which is exactly the state the empty-pane record was added for, and
// the failure mode is a link that opens a row saying "no build log was kept".
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node fragment.mjs
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

const PREVIEW = "e13c067994e2";
const PROJECT = "acme/docs";
const BUILD = "20260810-140000-a4fd6c9";

const streams = [];

// A build in flight: the row is building and the build list already carries the row the daemon
// wrote before starting, which is what the pane tails.
function statusPayload() {
  return {
    exposer: "test", instance: "harness-1", version: "v9.9.9 · abcdef123456",
    can_rebuild: true, pending: 0, running: 1,
    previews: [{
      preview_id: PREVIEW, repo: `github:${PROJECT}`, number: 146, branch: "feature/roles",
      name: "a-acme-docs-feature-roles", url: "", state: "building",
      updated_at: "2026-08-10T14:00:00.000Z", commit: "a4fd6c9",
      pr_url: "https://example.invalid/pr/146",
    }],
    events: [{
      repo: `github:${PROJECT}`, preview_id: PREVIEW, number: 146, branch: "feature/roles",
      at: "2026-08-10T14:00:00.000Z", kind: "building", commit: "a4fd6c9",
      message: "building", openable: false,
    }],
  };
}

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));
vc.on("error", (...a) => fail(`console.error: ${a.map(String).join(" ")}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  // The whole point of the file: the page is entered at the fragment, the way a link from a pull
  // request enters it.
  url: `http://127.0.0.1:8471/#preview=${PREVIEW}`,
  virtualConsole: vc,
  beforeParse(win) {
    // A live build: a start, a line, and no done — the stream stays open, which is what makes the
    // link worth following while the build runs.
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        if (!/^\/logs\/.+\/stream/.test(url)) return;
        streams.push(this);
        setTimeout(() => {
          if (this.readyState !== 1) return;
          this.emit("start", JSON.stringify({build_id: BUILD, live: true}));
          this.emit("line", JSON.stringify({text: "$ npm ci --no-audit --no-fund"}));
        }, 10);
      }
      emit(type, data) { for (const fn of this.listeners[type] || []) fn({data}); }
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      const body =
        url === "/status" ? statusPayload() :
        url === `/logs/${PREVIEW}` ? {
          preview_id: PREVIEW, live: true, logs: [{
            preview_id: PREVIEW, build_id: BUILD, size: 512, state: "building",
            seconds: 0, mod_time: "2026-08-10T14:00:05.000Z",
            started_at: "2026-08-10T14:00:00.000Z", commit: "a4fd6c9",
          }],
        } :
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
await new Promise(r => setTimeout(r, 400));

if (!win.eval("ui")) {
  console.log("FATAL: the page's ui object is unreachable; its script did not run");
  process.exit(1);
}

// The first status, delivered the way the live page gets it — over the event stream rather than a
// fetch, which is why this is driven rather than waited for. The fragment cannot be acted on before
// it arrives: the preview it names has to exist on the page first.
win.eval(`applyStatus(${JSON.stringify(statusPayload())})`);
await new Promise(r => setTimeout(r, 400));

console.log(`the page was entered at #preview=${PREVIEW}, on a preview that is building`);

const open = win.document.querySelector(".item.open");
if (!open) {
  fail("no row is open — the fragment was ignored, so the link from the comment lands on a list");
} else {
  ok("the row named in the fragment is open");
}

const pane = win.document.querySelector(".item.open .term");
const text = pane?.textContent || "";

if (!open) {
  // Everything below reads the open row. Nothing more to say.
} else if (/No build log was kept/.test(text)) {
  fail("the pane says no log was kept, for a build that is running right now");
} else if (!/npm ci/.test(text)) {
  fail(`the pane holds no build output: ${JSON.stringify(text.slice(0, 90))}`);
} else {
  ok("the pane holds the live build output");
}

// Two at most, and the second replaces the first.
//
// Following the link opens the pane on the live stream, and then the build picker resolves which
// build the commit names and points the pane at it — which for a build in flight is the live stream
// again. That is one redundant round trip on the page's own load path, not a leak: the first is
// closed. More than two would mean a stream per render, which is the churn the empty-pane record
// exists to prevent.
if (streams.length === 0) {
  fail("no stream — the row is open on a log nobody asked for");
} else if (streams.length > 2) {
  fail(`${streams.length} stream requests to open one log, want at most 2`);
} else {
  ok(`${streams.length} stream request(s) to open the log`);
}

const openStreams = streams.filter(s => s.readyState === 1);
if (streams.length && openStreams.length !== 1) {
  fail(`${openStreams.length} streams are open, want exactly 1 — a build in flight has more to ` +
    "say, and two open tails write the same lines into the pane twice");
} else if (streams.length) {
  ok("exactly one stream is open, tailing the build");
}

console.log("");
console.log(`${failures} failure(s)`);
process.exit(failures ? 1 : 0);
