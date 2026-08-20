// The switcher's "+ New project" row: where it points, and when it is offered at all.
//
// # Why
//
// Two failures, both live:
//
//   - It linked to /projects, which is not published through the public dashboard share — so on the
//     zrok URL the row led to a 404. `/api/admin` is the signal for whether that page exists, and
//     it is the same one the header's Projects and Settings links use.
//   - It linked to the *list*, so following it meant finding the New project button a second time.
//     The form has its own address now, and the row goes straight there.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node newprojectlink.mjs
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

function payload(canWrite) {
  return {
    can_write: canWrite,
    read_only_why: canWrite ? "" : "this request did not come from the machine running docpreview",
    defaults: {
      driver: "docker", image: "node:24-bookworm", images: ["node:24-bookworm"],
      framework: "docusaurus",
      frameworks: [{id: "docusaurus", label: "Docusaurus", build_command: "npm run build",
                    output: "build", dir: ""}],
      allow_local_driver: false,
    },
    projects: [{
      platform: "github", owner: "acme", repo: "docs", enabled: true,
      build_dir: "www", build_command: "npm run build", driver: "docker", secrets: [],
    }],
  };
}

// load opens the page at a URL and returns the jsdom window once it has settled.
//
// adminProjects is what /api/admin reports for the projects page: true on the machine running the
// daemon, false through the public dashboard share, where that page is not published.
async function load(url, canWrite, adminProjects = true) {
  const vc = new VirtualConsole();
  vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

  const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    url,
    virtualConsole: vc,
    pretendToBeVisual: true,
    beforeParse(win) {
      win.fetch = async u => {
        const body = u === "/api/admin"
          ? {secrets: adminProjects, projects: adminProjects}
          : payload(canWrite);
        return {ok: true, status: 200, statusText: "OK",
          json: async () => body, text: async () => JSON.stringify(body)};
      };
      // The operations page opens a stream on load. Never emits: what is under test is the
      // switcher's markup and the projects page, not the feed.
      win.EventSource = class {
        constructor() { this.readyState = 1; }
        addEventListener() {}
        close() { this.readyState = 2; }
      };
      win.matchMedia = () => ({matches: false, addEventListener() {}});
      win.HTMLElement.prototype.scrollIntoView = function () {};
      win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
      win.requestAnimationFrame = fn => setTimeout(fn, 0);
      win.getSelection = () => ({isCollapsed: true});
      win.confirm = () => true;
    },
  });

  const win = dom.window;
  await new Promise(r => win.addEventListener("load", r));
  await new Promise(r => setTimeout(r, 300));
  return win;
}

// openSwitcher loads the operations page, feeds it one project, and opens the picker.
async function openSwitcher(adminProjects) {
  const home = await load("http://127.0.0.1:8471/", true, adminProjects);
  home.eval(`applyStatus(${JSON.stringify({
    exposer: "test", instance: "t", pending: 0, running: 0, previews: [], events: [],
    projects: [{key: "github:acme/docs", label: "docs", avatar: ""}],
  })})`);
  await new Promise(r => setTimeout(r, 250));
  home.document.getElementById("projpick-btn").click();
  await new Promise(r => setTimeout(r, 150));
  return home;
}

console.log("the switcher links straight to the form's own page");
{
  const home = await openSwitcher(true);

  const add = home.document.querySelector("#projpick-list .pick-row.add");
  if (!add) {
    fail("the switcher has no + New project row");
  } else if (add.getAttribute("href") !== "/projects/new") {
    fail(`the link is ${JSON.stringify(add.getAttribute("href"))}, want /projects/new`);
  } else {
    ok("the link is /projects/new");
  }
}

console.log("");
console.log("and it is absent where /projects is not published");
{
  /* Through the public dashboard share the projects page is not served at all, so the row led to a
     404. The fixture answers `projects: false`, which is what /api/admin reports through the
     share. */
  const home = await openSwitcher(false);

  if (home.document.querySelector("#projpick-list .pick-row.add")) {
    fail("the add row is offered where /projects 404s");
  } else {
    ok("no add row");
  }
  // The rest of the switcher still works: this is one row hidden, not a broken picker.
  if (!home.document.querySelector("#projpick-list [data-pick-project]")) {
    fail("the project rows went with it");
  } else {
    ok("the project rows are still there");
  }
}

console.log("");
console.log("the list's own New project button goes to the same place");
{
  const win = await load("http://127.0.0.1:8471/projects", true);

  const btn = win.document.getElementById("p-new-top");
  if (!btn) {
    fail("the New project button is missing");
  } else if (btn.hidden) {
    fail("the button is hidden on a page that can write");
  } else {
    ok("the button is there, in the action row");
  }
  // And the list renders no form of its own: that was the panel this replaced.
  if (win.document.querySelector("#p-url") || win.document.querySelector(".grid-form")) {
    fail("the list still renders the project form");
  } else {
    ok("no form on the list");
  }
}

console.log("");
console.log("a read-only page offers neither");
{
  const win = await load("http://127.0.0.1:8471/projects", false);

  const btn = win.document.getElementById("p-new-top");
  if (btn && !btn.hidden) {
    fail("the New project button is offered on a read-only page");
  } else {
    ok("no New project button");
  }
  if (!win.document.querySelector(".notice.warn")) {
    fail("the read-only banner is missing, so the fixture proves nothing");
  } else {
    ok("the page says why it is read-only");
  }
}

console.log("");
console.log(`${failures} failure(s)`);
process.exit(failures ? 1 : 0);
