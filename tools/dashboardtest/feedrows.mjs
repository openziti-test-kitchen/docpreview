// Two rules about what the operations dashboard shows.
//
// 1. **One activity entry per attempt**, changing state in place. Keeping a row per
//    terminal event would mean a commit that failed and was rebuilt keeps both, and
//    re-attempting anything grows the rail: the row is meant to change from queued to
//    building to its outcome, not accumulate one row per state.
//
// 2. **A branch preview is always in the previews list**, finished or not: one `main` row
//    per project, which the next build of `main` replaces. Hiding it once it went Ready made
//    a project whose only preview is `main` vanish from PROJECTS on the live update.
//
// Both are properties of a render over a given payload, which is why they are tested here
// rather than reasoned about.
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
  // The permanent one. Finished, and listed all the same.
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
  // Same attempt, one poll later: still one row, now further along, not a new row per
  // transition.
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
  // "#0" would read as a bug rather than as a branch.
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

console.log("\na finished branch preview stays in the previews list");
{
  // clint's rule: once a project is added its `main` builds, and `main` stays in the list
  // until another build replaces it. Asserted against the *preview picker*, not the row
  // headers: rows are one per project, carrying whichever preview ranks highest, so a second
  // preview of one project is an option in that row's dropdown, plus a number in the counters.
  const head = $$(".item .head").find(h => h.textContent.includes("docs"));
  if (head) head.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();

  const options = $$('.item.open [data-role="pick"] option')
    .map(o => o.textContent.replace(/\s+/g, " ").trim());
  if (!options.length) {
    fail("no preview picker to check");
  } else if (!options.some(o => o.includes("main"))) {
    fail(`the finished branch preview is not offered: ${JSON.stringify(options)}`);
  } else ok(`the picker offers ${JSON.stringify(options)}`);
  if (options.some(o => o.includes("#0"))) {
    fail(`the branch preview is labelled #0 in the picker: ${JSON.stringify(options)}`);
  }

  // Counted too, over the same set the list shows: a counter disagreeing with the list is
  // the page arguing with itself.
  const all = $("#counters button .n");
  if (all && all.textContent.trim() !== "2") {
    fail(`the All counter says ${all.textContent.trim()}, want 2 — the branch preview is not counted`);
  } else ok("counted");

  if (!rowText().some(t => t.includes("cf9f37d"))) {
    fail("the branch build is missing from the activity feed");
  } else ok("its builds are in the feed");

  const ui = win.eval("ui");
  ui.pick = {};
  ui.open = null;
  await apply(status);
}

console.log("\na project whose only preview is main keeps its row through a live update");
{
  // The bug this pins. A newly added project's only preview is its branch preview. It was
  // listed while queued and building, then dropped out of PROJECTS the moment it went Ready —
  // the live update re-rendered the list correctly, over a set that excluded it — and came
  // back only when somebody clicked its activity entry, which pinned it. Driven through
  // applyStatus, the function every stream message and poll goes through, with no click.
  const K8S = "bitbucket:netfoundry/k8s-on-prem-installations";
  const k8sMain = {preview_id: "kkk", repo: K8S, number: 0, branch: "main",
    name: "k8s-on-prem-installations-main", url: "", state: "building", commit: "4e1d2c0",
    updated_at: "2026-07-31T09:10:00Z"};
  const k8sEvent = kind => ({at: kind === "ready" ? "2026-07-31T09:11:00Z" : "2026-07-31T09:10:00Z",
    kind, repo: K8S, preview_id: "kkk", number: 0, branch: "main", commit: "4e1d2c0",
    message: kind, openable: true});
  const payload = (state, kind) => ({
    ...status,
    projects: [...status.projects, {key: K8S, label: "k8s-on-prem-installations", avatar: ""}],
    previews: [...previews, {...k8sMain, state,
      url: state === "ready" ? "https://k8s-main.example/" : ""}],
    events: [k8sEvent(kind), ...events],
  });
  const ui = win.eval("ui");
  ui.pick = {};
  ui.open = null;
  const applyStatus = win.eval("applyStatus");
  const k8sRow = () => $$(".item .head")
    .find(h => h.textContent.includes("k8s-on-prem-installations"));
  // The list's own heading: the project switcher also says "All projects (n)", counted from
  // the configured projects rather than the rows, so it would pass with the row missing.
  const projectsCount = () =>
    ($("#list-heading")?.textContent.match(/^Projects \((\d+)\)/) || [])[1];

  applyStatus(payload("building", "building"));
  await settle();
  if (!k8sRow()) fail("the building branch preview has no row");
  else ok("listed while building");

  applyStatus(payload("ready", "ready"));
  await settle();
  if (!k8sRow()) {
    fail("the project vanished from PROJECTS when its main went Ready");
  } else ok("still listed once Ready, with nothing clicked");
  if (projectsCount() !== "2") {
    fail(`the list heading says Projects (${projectsCount()}), want 2`);
  } else ok("Projects (2)");
  const ready = $$("#counters button").find(b => b.textContent.includes("Ready"));
  const readyN = ready?.querySelector(".n")?.textContent.trim();
  if (readyN !== "3") fail(`the Ready counter says ${readyN}, want 3`);
  else ok("the Ready counter agrees with the list: 3");
  if (Object.values(ui.pick).includes("kkk")) {
    fail("the row is there only because something pinned it");
  }

  // Opened, it offers no Unlink: there is no pull request to stop rediscovering, and main is
  // meant to stay until its next build replaces it.
  k8sRow()?.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
  const unlink = $('.item.open [data-role="unlink"]');
  if (!unlink) fail("the opened main row has no actions to check");
  else if (!unlink.hidden) fail("a branch preview offers Unlink");
  else ok("no Unlink on a branch preview");
  ui.open = null;

  // The next main build replaces the row rather than adding one: a branch preview keeps one
  // id for its branch, so a rebuild is the same preview in a new state.
  applyStatus({...payload("building", "building"),
    previews: [...previews, {...k8sMain, commit: "9a0b1c2", updated_at: "2026-07-31T09:30:00Z"}]});
  await settle();
  const k8sRows = $$(".item .head")
    .filter(h => h.textContent.includes("k8s-on-prem-installations"));
  if (k8sRows.length !== 1) fail(`${k8sRows.length} rows for one project's main, want 1`);
  else ok("a rebuild replaces the main row in place");

  ui.pick = {};
  ui.open = null;
  await apply(status);
}

console.log("\na selected branch preview survives the next render");
{
  // Clicking an activity entry for a finished branch build pins that preview. The pin has to
  // survive the next render rather than resetting to the project's newest and jumping the
  // pane to a different preview's log while somebody is reading it.
  const ui = win.eval("ui");
  ui.pick = {};
  ui.open = null;
  await apply(status);

  const branch = status.previews.find(p => p.number === 0);
  if (!branch) {
    fail("the fixture has no finished branch preview to pin");
  } else {
    const name = win.eval(`project(${JSON.stringify(branch)})`);
    ui.open = name;
    ui.pick[name] = branch.preview_id;
    win.eval("render()");
    await settle();

    if (ui.pick[name] !== branch.preview_id) {
      fail(`the render reset the pin to ${ui.pick[name]}`);
    } else ok("the pin survives a render");

    const options = $$('.item.open [data-role="pick"] option').map(o => o.value);
    if (!options.includes(branch.preview_id)) {
      fail(`the selected preview is not in its own picker: ${JSON.stringify(options)}`);
    } else ok("and the row offers it");

    // Deselecting no longer makes it leave: it is listed whether or not anybody picked it.
    delete ui.pick[name];
    ui.open = null;
    win.eval("render()");
    await settle();
    const head = $$(".item .head").find(h => h.textContent.includes("docs"));
    if (head) head.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
    await settle();
    const after = $$('.item.open [data-role="pick"] option').map(o => o.value);
    if (!after.includes(branch.preview_id)) {
      fail(`the branch preview left the list after being deselected: ${JSON.stringify(after)}`);
    } else ok("it stays when deselected");
  }
}

console.log(failures ? `\n${failures} failure(s)` : `\nall feed and row checks OK`);
dom.window.close();
process.exit(failures ? 1 : 0);
