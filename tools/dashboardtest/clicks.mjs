// Loads the real dashboard in a DOM, clicks every activity entry, and asserts that
// each click does something the reader can see.
//
// # Why this exists
//
// Two cases in the activity feed are easy to get wrong and are not visible in the code
// without tracing state through a render:
//
//   - An entry for a build that is still queued or running names a commit with no
//     log file yet, so a lookup keyed on the log file finds nothing and must fall
//     through to the live stream rather than return silently. That is the row
//     somebody clicks precisely because they want to watch it.
//   - Every preview of one repository shares a project row. An entry naming the
//     build already on screen must still mark itself as selected, or the click is
//     indistinguishable from a dead button even though it worked.
//
// Both are obvious the moment something clicks the rows and diffs the result.
//
// # What it asserts
//
// For every clickable entry: something in the observable state changed, exactly one
// entry carries the selection mark, and the marked entry is the one that was
// clicked. The last is the guarantee that holds even when a click legitimately
// changes nothing else, and it is the reason the mark exists.
//
// # Running it
//
// Not part of `go test` — it needs node and jsdom, and this project ships a single
// Go binary with no node toolchain requirement. Install jsdom here first:
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/clicks.mjs
//
// It fetches live state from a running daemon on 127.0.0.1:8471 when one is there,
// and falls back to the fixture beside it when there is not. Real state is better:
// the fixture cannot contain the history shape nobody thought to invent.
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

import {liveStatus} from "./live.mjs";

const here = dirname(fileURLToPath(import.meta.url));
// Overridable, as in logtail.mjs and for the same reason: a harness that can only load
// the current file cannot tell a real assertion from one that would have passed
// regardless of the change.
const dashboard = process.env.DOCPREVIEW_DASHBOARD ||
  join(here, "..", "..", "internal", "daemon", "dashboard.html");
const daemon = process.env.DOCPREVIEW_URL || "http://127.0.0.1:8471";

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };

// Live state if a daemon is running, the fixture otherwise. An in-flight attempt is
// spliced in either way: a queued and a building entry for a commit with no log
// file, which is the case that was broken and which a captured payload from an idle
// daemon never contains.
async function loadStatus() {
  // Through live.mjs, which knows how to log in. /status is behind the login now, and a
  // 401 body parses as JSON perfectly well — so the previous version handed the page a
  // status object with no previews and the failure surfaced inside the page.
  const {status, source} = await liveStatus(daemon);
  console.log(`state: ${source}`);

  const first = status.previews?.[0];
  if (first) {
    status.previews[0] = {...first, state: "building"};
    const repo = first.repo;
    const base = {repo, preview_id: first.preview_id, number: first.number,
      branch: first.branch};
    status.events.unshift(
      {...base, at: "2099-01-01T00:00:01.000-00:00", kind: "building",
       commit: "inflght", message: "building"},
      {...base, at: "2099-01-01T00:00:00.000-00:00", kind: "queued",
       commit: "inflght", message: "queued"},
      // A skipped attempt. It never wrote a log, which is the case that was broken:
      // clicking it silently substituted the newest build's log, so the picker moved
      // and the pane did not. buildsFor deliberately has no entry for it.
      {...base, at: "2098-01-01T00:00:00.000-00:00", kind: "skipped",
       commit: "skipped", message: "no documentation changes", openable: true},
      // And a build that retention has cleaned up. The daemon reports openable
      // false for it, and the page must render it inert — a click that lands on an
      // empty pane is the failure this replaces.
      {...base, at: "2097-01-01T00:00:00.000-00:00", kind: "ready",
       commit: "pruned0", message: "ready in 20s", openable: false});
  }
  return status;
}

const status = await loadStatus();

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));
vc.on("error", (...a) => fail(`console.error: ${a.map(String).join(" ")}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: `${daemon}/`,
  virtualConsole: vc,
  beforeParse(win) {
    // A working EventSource, not an inert one.
    //
    // A stub that delivers nothing would make every entry resolving to the live stream
    // show an empty pane, indistinguishable from a real bug. A stub that cannot
    // distinguish "the page failed to load the log" from "the stub never sent one" is
    // worse than no stub, because it produces confident wrong answers about which rows
    // work.
    //
    // /events (status) stays silent: state is injected directly. /logs/{id}/stream
    // replays a line naming its preview, the way the daemon replays the current
    // build.
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        const m = url.match(/^\/logs\/([^/]+)\/stream$/);
        if (!m) return;
        const preview = decodeURIComponent(m[1]);
        // Asynchronous, like the real thing: a stream that delivered synchronously
        // inside the constructor would hide every ordering bug this is looking for.
        setTimeout(() => {
          if (this.readyState !== 1) return;
          this.emit("start", JSON.stringify({live: true, build_id: "current"}));
          this.emit("line", `LIVE-OF ${preview}`);
        }, 20);
      }
      emit(type, data) {
        for (const fn of this.listeners[type] || []) fn({data});
      }
      addEventListener(type, fn) {
        (this.listeners[type] ||= []).push(fn);
      }
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      // A stored build's log is a download, and its body is plain text rather than
      // JSON. Serving it as JSON like everything else hid the bug this harness was
      // extended to catch: the assertions passed while the pane stayed empty.
      const dl = url.match(/^\/logs\/([^/]+)\/download\/(.+)$/);
      if (dl) {
        const build = decodeURIComponent(dl[2]);
        const text = `LOG-OF ${build}\nsecond line of ${build}\n`;
        return {ok: true, status: 200, statusText: "OK",
          text: async () => text, json: async () => ({})};
      }
      const body =
        url === "/status" ? status :
        url.startsWith("/logs/") ? buildsFor(url) :
        url === "/api/admin" ? {secrets: true, projects: true} : {};
      return {ok: true, status: 200, statusText: "OK",
        json: async () => body, text: async () => JSON.stringify(body)};
    };
    // jsdom implements none of these, and every browser does. Shimmed rather than
    // avoided in the page, because the page is right to use them.
    win.matchMedia = () => ({matches: false, addEventListener() {}});
    win.HTMLElement.prototype.scrollIntoView = function () {};
    win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
    win.requestAnimationFrame = fn => setTimeout(fn, 0);
    // The rows ignore a click that happens while text is selected, so a click can
    // land on a selection the reader is making rather than opening something. jsdom
    // has getSelection but not always a collapsed one; pin it.
    win.getSelection = () => ({isCollapsed: true});
    if (!win.navigator.clipboard) {
      Object.defineProperty(win.navigator, "clipboard", {
        value: {writeText: async () => {}}, configurable: true,
      });
    }
  },
});

// One build per ready/failed event, named the way the daemon names them:
// "<date>-<time>-<short sha>". Deliberately no entry for the in-flight commit —
// that absence is the bug this reproduces.
function buildsFor(url) {
  const id = decodeURIComponent(url.split("/")[2]);
  const commits = (status.events || [])
    .filter(e => e.preview_id === id && ["ready", "failed"].includes(e.kind))
    .map(e => e.commit);
  return {
    live: false, preview_id: id,
    logs: [...new Set(commits)].map((c, i) => ({
      preview_id: id, build_id: `20260729-1907${String(i).padStart(2, "0")}-${c}`,
      size: 1564, mod_time: new Date(2026, 6, 29).toISOString(), state: "ready",
    })),
  };
}

const win = dom.window;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 300));

// `const ui = {...}` at the top level of a classic script is a lexical binding, not
// a property of window, so it is only reachable by evaluating in the page's scope.
const ui = win.eval("ui");
if (!ui) {
  console.log("FATAL: the page's ui object is unreachable; its script did not run");
  process.exit(1);
}

ui.previews = status.previews;
ui.events = status.events;
ui.status = status;
win.eval("render()");
await new Promise(r => setTimeout(r, 250));

const rows = [...win.document.querySelectorAll("#events .ev")];
// By class, not by tag. The rows are divs with role=button rather than real buttons:
// text inside a button cannot be selected and a button cannot contain a button, so
// the commit could neither be copied by hand nor given a copy control.
const clickable = rows.filter(r => r.classList.contains("linked"));
console.log(`feed: ${rows.length} rows, ${clickable.length} clickable\n`);

// An entry the daemon reports as not openable must be inert. Retention prunes old
// builds, so the feed outlives what it can show, and a row that looks identical to
// its clickable neighbours but does nothing reads as a dead button.
{
  const pruned = (status.events || []).filter(e => e.openable === false).length;
  const inert = rows.length - clickable.length;
  if (pruned && inert < pruned) {
    fail(`${pruned} entries are not openable but only ${inert} rendered inert`);
  }
  if (pruned) console.log(`(${inert} inert, for ${pruned} cleaned-up entries)\n`);
}

if (!clickable.length) {
  console.log("FATAL: no clickable entries; either the feed is empty or every " +
    "preview named in it is gone");
  process.exit(1);
}

const observe = () => {
  const picker = win.document.querySelector(".item.open [data-role=build]");
  const marked = win.document.querySelector("#events .ev.sel");
  const term = win.document.querySelector(".item.open .term");
  return {
    open: ui.open,
    pick: JSON.stringify(ui.pick),
    build: JSON.stringify(ui.build),
    picker: picker ? `${picker.value || "(live)"}` : "none",
    banner: win.document.querySelector(".logstate")?.textContent?.trim() || "",
    marked: marked?.dataset.at || "none",
    // The log pane's first line. The reason this harness exists a second time: the
    // picker and the banner both changed correctly while the pane stayed empty, so
    // every assertion passed and the feature was still broken.
    log: term ? (term.textContent.split("\n")[0] || "(empty)") : "no pane",
  };
};

for (const row of clickable) {
  const what = `${row.querySelector(".k")?.textContent} ${row.dataset.commit}`;
  const before = observe();

  row.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await new Promise(r => setTimeout(r, 200));

  const after = observe();
  const changed = Object.keys(before).filter(k => before[k] !== after[k]);
  console.log(`${what.padEnd(20)} picker=${after.picker.padEnd(24)} log=${after.log}`);

  if (!changed.length) fail(`clicking "${what}" changed nothing observable`);

  // An entry whose build kept no log must say so, not quietly show a different
  // build's. Recognised by kind: a queued or building entry has no log *yet* and
  // the live stream is that build, which is a different situation.
  const noLog = !["queued", "building"].includes(row.dataset.kind) &&
    !(ui.builds[row.dataset.id] || []).some(b => b.build_id.endsWith("-" + row.dataset.commit));
  if (noLog) {
    // The banner names the situation, and the *pane* explains it. "No build log" above
    // an empty black rectangle 15rem tall would give a skipped build every signal
    // except the one anybody wants: why it skipped. An empty pane here is the failure.
    if (!/skipped|no log/i.test(after.banner)) {
      fail(`"${what}" has no log, but the banner says "${after.banner}"`);
    }
    if (after.log === "(empty)") {
      fail(`"${what}" has no log and the pane is blank; it should say why`);
    }
    if (row.dataset.kind === "skipped" && !/skipped/i.test(after.log)) {
      fail(`a skipped build's pane does not say so: "${after.log}"`);
    }
    // The remaining assertions are about a build that exists.
    win.eval("render()");
    await new Promise(r => setTimeout(r, 120));
    continue;
  }

  if (after.marked !== row.dataset.at) {
    fail(`clicking "${what}" marked ${after.marked}, not the clicked entry`);
  }
  const n = win.document.querySelectorAll("#events .ev.sel").length;
  if (n !== 1) fail(`${n} entries marked after clicking "${what}", want 1`);

  // The whole point of clicking an entry: the pane shows that build's log. A picker
  // that moved while the pane did not is the bug this exists to catch.
  if (after.log === "(empty)" || after.log === "no pane") {
    fail(`clicking "${what}" left the log pane empty (picker says ${after.picker})`);
  } else if (after.picker === "(live)") {
    // The live stream replays the current build, and its stub line names the
    // preview. Anything else means the pane is still showing the build clicked
    // before this one.
    if (!after.log.startsWith("LIVE-OF")) {
      fail(`clicking "${what}" selected the live stream but the pane still shows "${after.log}"`);
    }
  } else if (!after.log.includes(after.picker)) {
    fail(`the picker says ${after.picker} but the pane shows "${after.log}"`);
  }

  // Then a re-render, which is not hypothetical: the page calls renderList every
  // five seconds to keep relative timestamps honest, and render on every status
  // event. A selection that survives the click but not the next tick is a pane that
  // goes blank or reverts while somebody is reading it — and from the reader's side
  // that is indistinguishable from the click never having worked.
  win.eval("render()");
  await new Promise(r => setTimeout(r, 120));

  const ticked = observe();
  if (ticked.picker !== after.picker) {
    fail(`a re-render moved the picker from ${after.picker} to ${ticked.picker}`);
  }
  if (ticked.log !== after.log) {
    fail(`a re-render changed the pane from "${after.log}" to "${ticked.log}"`);
  }
  if (ticked.marked !== after.marked) {
    fail(`a re-render lost the selection mark (${after.marked} -> ${ticked.marked})`);
  }
}

console.log(failures ? `\n${failures} failure(s)` : `\nall ${clickable.length} clicks OK`);
process.exit(failures ? 1 : 0);
