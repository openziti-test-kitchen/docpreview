// Drives the project switcher on the operations dashboard: open it, filter it, choose a
// project, and check what the trigger then says.
//
// # Why this exists
//
// Not a <select>: a select gets the browser's behaviour for free — keyboard, type-ahead,
// click-away, one open at a time. A hand-built panel has none of that unless something
// here provides it, and every one of those is invisible in a screenshot.
//
// It also covers the join this page performs: previews say which projects have activity,
// `status.projects` says what each is called and what badge it carries, and a project
// with no previews at all must still be listed.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/switcher.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

// Three projects: two with previews, and one configured with none — the case a join
// keyed only on previews would drop. One carries an uploaded badge, one a display
// name, one neither.
const status = {
  exposer: "zrok2", instance: "test", pending: 0, running: 0,
  projects: [
    {key: "github:netfoundry/unified-doc", label: "Unified Doc",
     avatar: "data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%2F%3E"},
    {key: "github:openziti-test-kitchen/docpreview", label: "docpreview", avatar: ""},
    {key: "bitbucket:netfoundry/customer-connect-docs", label: "Customer Connect", avatar: "🔌"},
  ],
  previews: [
    {preview_id: "aaa", repo: "github:netfoundry/unified-doc", number: 1, branch: "a",
     name: "u-a", url: "https://u-a.example/", state: "ready",
     updated_at: "2026-07-29T20:00:00-04:00", commit: "1111111"},
    {preview_id: "bbb", repo: "github:openziti-test-kitchen/docpreview", number: 2,
     branch: "b", name: "d-b", url: "https://d-b.example/", state: "ready",
     updated_at: "2026-07-29T20:01:00-04:00", commit: "2222222"},
    {preview_id: "ccc", repo: "github:openziti-test-kitchen/docpreview", number: 3,
     branch: "c", name: "d-c", url: "https://d-c.example/", state: "failed",
     updated_at: "2026-07-29T20:02:00-04:00", commit: "3333333"},
  ],
  events: [],
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
const click = async el => {
  el.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
};
const type = async (el, v) => {
  el.value = v;
  el.dispatchEvent(new win.Event("input", {bubbles: true}));
  await settle();
};
const rows = () => $$("#projpick-list [data-pick-project]").filter(r => !r.hidden);

// The page's state comes from the stream in production; inject it and render.
const ui = win.eval("ui");
ui.previews = status.previews;
ui.events = status.events;
ui.status = status;
win.eval("render()");
await settle();

console.log("closed until asked for");
{
  if (!$("#projpick-panel").hidden) fail("the panel is open before anything was clicked");
  const label = $("#projpick-btn .pick-label").textContent.trim();
  if (!label.startsWith("All projects")) fail(`the trigger says ${JSON.stringify(label)}`);
  else ok(`trigger reads ${JSON.stringify(label)}`);
}

console.log("\nthe list");
{
  await click($("#projpick-btn"));
  if ($("#projpick-panel").hidden) fail("clicking the trigger did not open the panel");

  const labels = rows().map(r => r.textContent.replace(/\s+/g, " ").trim());
  // Every configured project, including the one with no previews, plus "All projects".
  for (const want of ["Unified Doc", "docpreview", "Customer Connect"]) {
    if (!labels.some(l => l.includes(want))) {
      fail(`${want} is missing from the list: ${JSON.stringify(labels)}`);
    }
  }
  if (!labels.some(l => l.startsWith("All projects"))) fail("no All projects entry");
  ok(`${labels.length} entries, including a project with no previews`);

  // Counts come from the previews: docpreview has two, unified-doc one, and the third
  // has none and must not claim a zero.
  const dp = rows().find(r => r.dataset.pickProject === "docpreview");
  if (dp?.querySelector(".count")?.textContent.trim() !== "2") {
    fail(`docpreview count = ${JSON.stringify(dp?.querySelector(".count")?.textContent)}`);
  } else ok("counts come from the previews");

  // Badges: an uploaded image renders as an <img>, an emoji as text, neither as initials.
  const ud = rows().find(r => r.dataset.pickProject === "unified-doc");
  if (!ud?.querySelector(".ava img")) fail("an uploaded badge is not rendered as an image");
  const cc = rows().find(r => r.dataset.pickProject === "customer-connect-docs");
  if (cc?.querySelector(".ava")?.textContent.trim() !== "🔌") fail("an emoji badge is missing");
  if (dp?.querySelector(".ava")?.textContent.trim() !== "DO") {
    fail(`docpreview's derived badge is ${JSON.stringify(dp?.querySelector(".ava")?.textContent)}`);
  } else ok("image, emoji and derived badges all render");

  // Nothing fetched from the network: a remote badge would announce every project to
  // whoever hosts the image on every page load.
  for (const img of $$("#projpick-list img")) {
    if (!img.getAttribute("src").startsWith("data:")) fail(`badge fetches ${img.src}`);
  }

  // The way to add one is in the list, where you notice it is missing.
  if (!$("#projpick-list .pick-row.add")) fail("no New project entry");
}

console.log("\nfiltering");
{
  const find = $("#projpick-find");
  await type(find, "customer");
  const shown = rows().map(r => r.dataset.pickProject);
  if (shown.length !== 1 || shown[0] !== "customer-connect-docs") {
    fail(`filtering by label gave ${JSON.stringify(shown)}`);
  } else ok("matches a display name");

  // The repository name is also a valid thing to type, and it is not the label here.
  await type(find, "unified-doc");
  if (rows().map(r => r.dataset.pickProject).join() !== "unified-doc") {
    fail("filtering by repository name does not match");
  } else ok("matches a repository name");

  await type(find, "zzz");
  if (rows().length) fail("a non-matching filter still shows rows");
  if (!$("#projpick-list .pick-empty")) fail("no message when nothing matches");
  else ok("says so when nothing matches");

  // Escape closes without choosing.
  await type(find, "");
  find.dispatchEvent(new win.KeyboardEvent("keydown", {key: "Escape", bubbles: true}));
  await settle();
  if (!$("#projpick-panel").hidden) fail("Escape did not close the panel");
  else ok("Escape closes it");
  if (ui.project !== "") fail(`Escape changed the scope to ${JSON.stringify(ui.project)}`);
}

console.log("\nchoosing");
{
  await click($("#projpick-btn"));
  await click(rows().find(r => r.dataset.pickProject === "docpreview"));

  if (ui.project !== "docpreview") fail(`scope is ${JSON.stringify(ui.project)}`);
  if (!$("#projpick-panel").hidden) fail("the panel stayed open after choosing");
  const label = $("#projpick-btn .pick-label").textContent.trim();
  if (!label.includes("docpreview")) fail(`the trigger says ${JSON.stringify(label)}`);
  else ok(`chose docpreview, trigger reads ${JSON.stringify(label)}`);

  // And the list narrowed to that project.
  const names = $$(".list .item").map(i => i.dataset.project);
  if (names.join() !== "docpreview") fail(`the preview list shows ${JSON.stringify(names)}`);
  else ok("the preview list narrowed to it");
}

console.log("\nEnter takes the first match");
{
  await click($("#projpick-btn"));
  await type($("#projpick-find"), "unified");
  $("#projpick-find").dispatchEvent(
    new win.KeyboardEvent("keydown", {key: "Enter", bubbles: true}));
  await settle();
  if (ui.project !== "unified-doc") fail(`Enter selected ${JSON.stringify(ui.project)}`);
  else ok("filter then Enter is one gesture");
}

console.log("\na click elsewhere closes it");
{
  await click($("#projpick-btn"));
  if ($("#projpick-panel").hidden) fail("the panel did not open");
  await click($("#list") || doc.body);
  if (!$("#projpick-panel").hidden) {
    fail("the panel is still open after a click outside it, covering the page");
  } else ok("closes on a click outside");
}

console.log("\nsurviving a status tick");
{
  await click($("#projpick-btn"));
  await type($("#projpick-find"), "doc");
  const before = $("#projpick-find").value;
  win.eval("render()");
  await settle();
  if ($("#projpick-panel").hidden) fail("a status tick closed the open panel");
  if ($("#projpick-find").value !== before) fail("a status tick cleared the filter text");
  else ok("an open panel survives a re-render");
}

console.log(failures ? `\n${failures} failure(s)` : `\nall switcher checks OK`);
process.exit(failures ? 1 : 0);
