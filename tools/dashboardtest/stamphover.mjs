// Hovers a log line's timestamp and checks what the tooltip says.
//
// # What it asserts
//
//   - **The stamp is a separate element, and the rest of the line is classified without it.**
//     Every line the daemon writes is prefixed `18:31:18.128 `, so a `$ ` test against the whole
//     line never matched and no command line was ever highlighted.
//   - **Nothing is written until somebody hovers.** A title per line is thousands of attributes per
//     build, and three of the five values are relative to lines that have not arrived yet.
//   - **The five rows are right.** Local zone, UTC, an age, distance from the first line, and the
//     gap from the line above — the last two computed from the stamps, so they hold for a log with
//     no build list behind it.
//   - **A missing build list removes the absolute rows rather than inventing them.** The stamps
//     carry no date and no zone; the build's started_at is where both come from.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node stamphover.mjs
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
const BUILD = "20260810-182600-7127614";

// The build the pane is tailing. Its started_at is the only carrier of the date and the zone the
// stamps below belong to.
const STARTED = "2026-08-10T18:26:00.000Z";

// Four lines, chosen for the gaps between them: 1.4s, then 112ms, then 49s.
const LINES = [
  "18:26:00.100 $ docker create node:24-bookworm  181d5778a228",
  "18:26:01.500 yarn install v1.22.22",
  "18:26:01.612 [1/5] Validating package.json...",
  "18:26:50.612 ERROR something went wrong",
];

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));
vc.on("error", (...a) => fail(`console.error: ${a.map(String).join(" ")}`));

function statusPayload() {
  return {
    exposer: "test", instance: "harness-1", version: "v9.9.9 · abcdef123456",
    // The daemon is in UTC, which is what docprev runs as — and deliberately not the zone this
    // harness runs in, since reading the stamp as local time is the bug being guarded against.
    stamp_offset_minutes: 0,
    can_rebuild: true, pending: 0, running: 1,
    previews: [{
      preview_id: PREVIEW, repo: `github:${PROJECT}`, number: 7, branch: "feat/x",
      name: "acme-docs-feat-x", url: "", state: "building",
      updated_at: "2026-08-10T18:26:05.000Z", commit: "7127614",
      pr_url: "https://example.invalid/pr/7",
    }],
    events: [{
      repo: `github:${PROJECT}`, preview_id: PREVIEW, number: 7, branch: "feat/x",
      at: "2026-08-10T18:26:00.000Z", kind: "building", commit: "7127614", message: "building",
    }],
  };
}

let serveBuilds = true;

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/",
  virtualConsole: vc,
  beforeParse(win) {
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        if (!/^\/logs\/.+\/stream/.test(url)) return;
        setTimeout(() => {
          if (this.readyState !== 1) return;
          this.emit("start", JSON.stringify({build_id: BUILD, live: true}));
          // Raw text, not JSON: the daemon writes each line as the `data:` of a `line` event, so
          // the page's handler receives the line itself. Wrapping it in an object here would test a
          // protocol the server does not speak.
          for (const line of LINES) this.emit("line", line);
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
          preview_id: PREVIEW, live: true,
          logs: serveBuilds ? [{
            preview_id: PREVIEW, build_id: BUILD, size: 512, state: "building", seconds: 0,
            mod_time: "2026-08-10T18:26:50.000Z", started_at: STARTED, commit: "7127614",
          }] : [],
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
await new Promise(r => setTimeout(r, 200));

const settle = (ms = 250) => new Promise(r => setTimeout(r, ms));
win.eval(`applyStatus(${JSON.stringify(statusPayload())})`);
await settle();

win.document.querySelector(".item .head").click();
await settle(400);

const pane = win.document.querySelector(".item.open .term");
if (!pane) {
  console.log("FATAL: no pane after expanding the row");
  process.exit(1);
}

const stamps = [...pane.querySelectorAll(".lts")];
console.log(`${LINES.length} lines streamed into the pane`);

if (stamps.length !== LINES.length) {
  fail(`${stamps.length} stamps are separate elements, want ${LINES.length}`);
} else {
  ok("every line's stamp is its own element");
}
if (stamps.length && stamps[0].textContent !== "18:26:00.100") {
  fail(`the first stamp reads ${JSON.stringify(stamps[0].textContent)}`);
}

// The classification runs on the body, which is the half that was broken: the `$ ` and the ERROR
// are at the start of the text *after* the stamp.
if (!pane.querySelector(".cmd")) {
  fail("the '$ docker create' line is not marked as a command — the stamp is still in the way");
} else {
  ok("a command line is highlighted");
}
if (!pane.querySelector(".err")) {
  fail("the ERROR line is not marked as an error");
} else {
  ok("an error line is highlighted");
}

// Nothing written before a hover.
if (stamps.some(s => s.hasAttribute("title"))) {
  fail("a stamp carries a title before anyone hovered it");
} else {
  ok("no titles written until hover");
}

function hover(el) {
  el.dispatchEvent(new win.MouseEvent("mouseover", {bubbles: true}));
  return el.getAttribute("title") || "";
}

console.log("");
console.log("hovering the third line, 112ms after the second and 1.5s after the first");

const title = hover(stamps[2]);
if (!title) {
  fail("hovering wrote no title");
} else {
  ok("hovering fills the title in");
}

const rows = title.split("\n");
const row = label => (rows.find(r => r.startsWith(label)) || "").replace(label, "").trim();

for (const [label, want] of [
  ["Relative to start", "1.5s"],
  ["Relative to previous", "112ms"],
]) {
  const got = row(label);
  if (got !== want) {
    fail(`${label} = ${JSON.stringify(got)}, want ${want}`);
  } else {
    ok(`${label} = ${got}`);
  }
}

// The daemon stamps in UTC, so the stamp is the UTC time. Reading it as the harness's local zone
// would shift this by the machine's own offset — which is the whole reason the offset is in
// /status.
if (row("UTC") !== "2026-08-10 18:26:01.612 UTC") {
  fail(`UTC = ${JSON.stringify(row("UTC"))}, want 2026-08-10 18:26:01.612 UTC — ` +
    "the stamp was read in the wrong zone");
} else {
  ok(`UTC = ${row("UTC")}`);
}
if (!/ago|just now/.test(title)) {
  fail("no relative-to-now row");
} else {
  ok("a relative-to-now row is present");
}

console.log("");
console.log("the first line has nothing above it, so it gets no relative rows");
const firstTitle = hover(stamps[0]);
if (/Relative to (start|previous)/.test(firstTitle)) {
  fail(`the first line claims a gap:\n${firstTitle}`);
} else {
  ok("the first line reports no gap from itself");
}

console.log("");
console.log("with no build list there is no date or zone, and those rows are omitted");
win.eval(`forgetBuildLists && forgetBuildLists()`);
win.eval(`ui.builds = {}`);
const bare = hover(stamps[2]);
if (/UTC/.test(bare)) {
  fail(`an absolute time was rendered with no build to date it:\n${bare}`);
} else {
  ok("no absolute rows without a build list");
}
if (!/Relative to previous/.test(bare)) {
  fail(`the stamp-only rows were lost too:\n${bare}`);
} else {
  ok("the gaps still work, since they come from the stamps");
}

console.log("");
console.log(`${failures} failure(s)`);
process.exit(failures ? 1 : 0);
