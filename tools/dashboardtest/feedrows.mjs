// Two rules about what the operations dashboard shows, and both were reported as noise.
//
// 1. **One activity entry per attempt**, changing state in place. The feed used to keep a
//    row per terminal event, so a commit that failed and was rebuilt kept both, and
//    re-attempting anything grew the rail. Reported as "i don't want n entries for queued,
//    building, success|fail".
//
// 2. **A finished branch preview is not in the previews list.** It is permanent by design,
//    which makes it the one row nobody needs: always green, always there. It lives on its
//    project card instead. While it is queued or building it *does* appear, because the log
//    pane opens from a row and without one there would be nowhere to watch a branch build.
//
// Both are properties of a render over a given payload, which is why they are tested here
// rather than reasoned about — see the note in internal/daemon/CLAUDE.md about three
// consecutive wrong diagnoses of one feed bug.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/feedrows.mjs
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

const REPO = "github:acme/docs";

// One commit's whole life, plus a second attempt of the same commit that failed first.
// Newest first, which is the order the daemon sends.
const events = [
  {at: "2026-07-31T09:00:40Z", kind: "ready", repo: REPO, preview_id: "aaa", number: 7,
   branch: "add-guide", commit: "85912e2", message: "ready in 24s", openable: true},
  {at: "2026-07-31T09:00:16Z", kind: "building", repo: REPO, preview_id: "aaa", number: 7,
   branch: "add-guide", commit: "85912e2", message: "building", openable: true},
  {at: "2026-07-31T09:00:00Z", kind: "queued", repo: REPO, preview_id: "aaa", number: 7,
   branch: "add-guide", commit: "85912e2", message: "queued", openable: true},
  {at: "2026-07-31T08:50:00Z", kind: "failed", repo: REPO, preview_id: "aaa", number: 7,
   branch: "add-guide", commit: "85912e2", message: "the build exited 1", openable: true},
  // A different commit: its own row, always.
  {at: "2026-07-31T08:40:00Z", kind: "ready", repo: REPO, preview_id: "aaa", number: 7,
   branch: "add-guide", commit: "0faa113", message: "ready in 31s", openable: true},
  // A branch preview's build, which is still feed-worthy.
  {at: "2026-07-31T08:30:00Z", kind: "ready", repo: REPO, preview_id: "bbb", number: 0,
   branch: "main", commit: "cf9f37d", message: "ready in 58s", openable: true},
];

const previews = [
  {preview_id: "aaa", repo: REPO, number: 7, branch: "add-guide", name: "add-guide",
   url: "https://add-guide.example/", state: "ready", commit: "85912e2",
   updated_at: "2026-07-31T09:00:40Z"},
  // The permanent one. Finished, so it belongs on the project card and not here.
  {preview_id: "bbb", repo: REPO, number: 0, branch: "main", name: "docs-main",
   url: "https://docs-main.example/", state: "ready", commit: "cf9f37d",
   updated_at: "2026-07-31T08:30:00Z"},
];

const status = {
  exposer: "zrok2", instance: "test", pending: 0, running: 0,
  projects: [{key: REPO, label: "docs", avatar: ""}],
  previews, events,
};

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/",
  virtualConsole: vc,
  pretendToBeVisual: true,
  beforeParse(win) {
    win.EventSource = class {
      constructor() { this.readyState = 1; }
      addEventListener() {}
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      const body = url === "/status" ? status
        : url === "/api/admin" ? {secrets: true, projects: true} : {};
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
const doc = win.document;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 200));

const $ = s => doc.querySelector(s);
const $$ = s => [...doc.querySelectorAll(s)];
const settle = () => new Promise(r => setTimeout(r, 80));

// State arrives over the stream in production; inject it and render.
const apply = async s => {
  const ui = win.eval("ui");
  ui.previews = s.previews;
  ui.events = s.events;
  ui.status = s;
  win.eval("render()");
  await settle();
};
await apply(status);

const rows = () => $$(".ev");
const rowText = () => rows().map(r => r.textContent.replace(/\s+/g, " ").trim());

console.log("one activity entry per attempt");
{
  const text = rowText();
  // Four events for commit 85912e2 — queued, building, ready, and an earlier failure —
  // must be one row, showing where it got to.
  const forCommit = text.filter(t => t.includes("85912e2"));
  if (forCommit.length !== 1) {
    fail(`${forCommit.length} rows for one commit:\n     ${forCommit.join("\n     ")}`);
  } else if (!forCommit[0].includes("Ready")) {
    fail(`the surviving row is ${JSON.stringify(forCommit[0])}, want the newest state`);
  } else ok(`one row, reading ${JSON.stringify(forCommit[0])}`);

  // A different commit keeps its own row: "which push was this" is the question the rail
  // exists to answer.
  if (!text.some(t => t.includes("0faa113"))) {
    fail("a second commit lost its row, so the feed no longer distinguishes pushes");
  } else ok("a different commit is a different row");

  if (rows().length !== 3) {
    fail(`${rows().length} rows total, want 3 — one per attempt:\n     ${text.join("\n     ")}`);
  } else ok("3 rows for 6 events");
}

console.log("\nthe row changes state in place");
{
  // Same attempt, one poll later: still one row, now further along. This is the behaviour
  // the report asked for — not a new row per transition.
  const queuedFirst = {
    ...status,
    events: [{at: "2026-07-31T10:00:00Z", kind: "queued", repo: REPO, preview_id: "aaa",
              number: 7, branch: "add-guide", commit: "7cf20bc", message: "queued",
              openable: true}, ...events],
  };
  await apply(queuedFirst);
  let mine = rowText().filter(t => t.includes("7cf20bc"));
  if (mine.length !== 1 || !mine[0].includes("Queued")) {
    fail(`after queueing: ${JSON.stringify(mine)}`);
  } else ok(`queued: ${JSON.stringify(mine[0])}`);

  const nowBuilding = {
    ...status,
    events: [{at: "2026-07-31T10:00:12Z", kind: "building", repo: REPO, preview_id: "aaa",
              number: 7, branch: "add-guide", commit: "7cf20bc", message: "building",
              openable: true}, ...queuedFirst.events],
  };
  await apply(nowBuilding);
  mine = rowText().filter(t => t.includes("7cf20bc"));
  if (mine.length !== 1) {
    fail(`building added a row instead of changing one: ${JSON.stringify(mine)}`);
  } else if (!mine[0].includes("Building")) {
    fail(`the row still reads ${JSON.stringify(mine[0])}`);
  } else ok(`became: ${JSON.stringify(mine[0])}`);

  await apply(status);
}

console.log("\nrows are newest first, whatever order the daemon sent");
{
  // The feed is a ring buffer, so its own order is *insertion* — the same thing as time only
  // while every event arrives as it happens. It does not: the history is reloaded from the
  // builds table at startup and a startup scan appends builds of its own, which put an event
  // from 07:19 above one from 07:48 on a real dashboard.
  const shuffled = {
    ...status,
    events: [events[4], events[0], events[5], events[2]], // deliberately not time-ordered
  };
  await apply(shuffled);

  const times = rows().map(r => r.querySelector(".t").textContent.trim());
  const sorted = [...times].sort().reverse();
  if (times.join(",") !== sorted.join(",")) {
    fail(`rows are out of order: ${times.join(", ")}`);
  } else ok(`newest first: ${times.join(", ")}`);

  await apply(status);
}

console.log("\na branch preview is not written as #0");
{
  // Number 0 means "no pull request" — see model.PullRequest.IsBranch — and rendering it as
  // "#0" reads as a bug rather than as a branch. That is exactly how it was reported.
  const text = rowText().join(" ");
  if (text.includes("#0")) {
    fail(`a branch build renders as #0: ${JSON.stringify(text)}`);
  } else ok("no #0 anywhere");
  if (!text.includes("@main")) {
    fail(`a branch build does not name its branch: ${JSON.stringify(text)}`);
  } else ok("written as @main");

  // And named once. The label is already the branch, so printing the branch again beside
  // it read "docusaurus-shared@main main · c251f11" — the same word twice in the widest
  // element of a 21rem rail. A pull request keeps its branch, because "#21" does not say
  // what branch that is; asserted below so this does not turn into "drop it everywhere".
  for (const row of rowText()) {
    const m = /@(\S+)/.exec(row);
    if (m && row.includes(`· ${m[1]}`)) {
      fail(`a branch row names its branch twice: ${JSON.stringify(row)}`);
    }
  }
  const pr = rowText().find(r => /#\d/.test(r));
  if (pr && !/·/.test(pr)) {
    fail(`a pull request row lost its branch: ${JSON.stringify(pr)}`);
  } else ok("named once for a branch, still shown for a pull request");
}

console.log("\nthe message is not in the row");
{
  // "ready in 24s" and "the build exited 1" were the widest thing in a 21rem rail and
  // nearly never what anybody needed. They are the tooltip now.
  const text = rowText().join(" ");
  for (const noise of ["ready in", "exited 1", "queued"]) {
    if (text.toLowerCase().includes(noise)) {
      fail(`the row still carries the message: ${JSON.stringify(noise)}`);
    }
  }
  if (!failures) ok("no message text in the rows");

  const titled = rows().find(r => (r.getAttribute("title") || "").includes("ready in 24s"));
  if (!titled) fail("the message was dropped rather than moved to the tooltip");
  else ok("kept as the row's tooltip");
}

console.log("\na finished branch preview is not in the previews list");
{
  // Asserted against the *preview picker*, not the row headers — which is where a branch
  // preview actually lived, and the first version of this test got that wrong and passed
  // against the unfixed page. Rows are one per project, carrying whichever preview ranks
  // highest, so a second preview of one project was never its own row: it was an option in
  // that row's dropdown, plus a number in the counters.
  const head = $$(".item .head").find(h => h.textContent.includes("docs"));
  if (head) head.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();

  const options = $$('.item.open [data-role="pick"] option')
    .map(o => o.textContent.replace(/\s+/g, " ").trim());
  if (!options.length) {
    fail("no preview picker to check");
  } else if (options.some(o => o.includes("main"))) {
    fail(`the permanent branch preview is still offered: ${JSON.stringify(options)}`);
  } else ok(`the picker offers only ${JSON.stringify(options)}`);

  // And it must not be counted either — a counter disagreeing with the list is the page
  // arguing with itself.
  const all = $("#counters button .n");
  if (all && all.textContent.trim() !== "1") {
    fail(`the All counter says ${all.textContent.trim()}, want 1 — the branch preview is counted`);
  } else ok("not counted");

  // Its build still appears in the activity feed: that history is not the thing being
  // hidden, the permanent row is.
  if (!rowText().some(t => t.includes("cf9f37d"))) {
    fail("the branch build vanished from the activity feed too");
  } else ok("its builds are still in the feed");
}

console.log("\na branch preview that is building does appear");
{
  // The exception that stops the removal creating a dead end: the log pane opens from a
  // row, so a branch build with no row could not be watched or revealed from the feed.
  const building = {
    ...status,
    previews: [previews[0], {...previews[1], state: "building", url: ""}],
  };
  // Clear the pin the previous section's click left behind. A row shows whichever preview
  // ranks highest *unless* somebody has chosen one, and `ui.pick` outlives a render — so
  // without this the assertion below reads a selection this test made rather than what the
  // page would show on its own.
  const ui = win.eval("ui");
  ui.pick = {};
  ui.open = null;
  await apply(building);

  const heads = $$(".item .head").map(h => h.textContent.replace(/\s+/g, " ").trim());
  if (!heads.some(h => h.includes("main"))) {
    fail("a building branch preview has no row, so there is nowhere to watch it");
  } else ok("present while building");

  await apply(status);
}

console.log(failures ? `\n${failures} failure(s)` : `\nall feed and row checks OK`);
dom.window.close();
process.exit(failures ? 1 : 0);
