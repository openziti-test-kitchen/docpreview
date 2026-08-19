// One project's settings, on its own page: /projects/new and /projects/<platform>/<owner>/<repo>.
//
// # Why the page exists
//
// The form was a panel that expanded inside the project list. Nineteen fields are taller than the
// list they open in, so editing the first project pushed the fourth off the screen — and the reader
// lost the row they were comparing against. A page has no position to get wrong, it is linkable,
// and the browser's back button is the way out.
//
// # What this covers
//
// The form's behaviour, which used to live in projects.mjs: the fields, pasting a repository URL,
// the framework preset, the credential boxes, what a save actually sends, and the guard on leaving
// with unsaved edits. Plus the two things the page itself has to get right — an address for a
// project that does not exist, and a read-only daemon.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node projectedit.mjs
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

// The same two projects projects.mjs uses: one that states most of its build and holds its own
// variables, and one that defers entirely and is disabled, private, with a stored access token.
const state = canWrite => ({
  can_write: canWrite,
  read_only_why: canWrite ? "" : "this request did not come from the machine running docpreview",
  secrets_available: true,
  vault_locked: false,
  global_secrets: ["SHARED_TOKEN"],
  defaults: {
    driver: "docker", image: "node:24-bookworm-slim",
    docker_available: true, allow_local_driver: false,
    images: ["node:24-bookworm-slim", "node:24-bookworm", "node:24-alpine"],
    frameworks: [
      {id: "", label: "None — the repository decides"},
      {id: "docusaurus", label: "Docusaurus (v2+)",
       build_command: "npm run build", output: "build"},
      {id: "mkdocs", label: "MkDocs",
       build_command: "mkdocs build", output: "site", needs_tool: "mkdocs"},
    ],
    framework: "docusaurus",
    timeout: "15m0s",
  },
  projects: [
    {
      platform: "github", owner: "netfoundry", repo: "unified-doc", enabled: true,
      driver: "docker", build_dir: "www", build_command: "npm run build",
      build_output: "build", base_url: "/", detect_script: "", image: "",
      notes: "the big one", display_name: "Unified Doc", avatar: "",
      secrets: ["BB_REPO_TOKEN_ONPREM"],
    },
    {
      platform: "bitbucket", owner: "netfoundry", repo: "customer-connect-docs",
      enabled: false, driver: "", build_dir: "", build_command: "", build_output: "",
      base_url: "", detect_script: "", image: "", notes: "", secrets: [],
      scm: ["scm.access_token"], private: true,
      branch: {
        name: "master", preview_id: "b400e0aa1234", state: "ready",
        url: "https://customer-connect-docs-master.shares.zrok.io/",
        commit: "cf9f37d25cf7515f8c7e531afbe97cc6ee4238f3",
        updated_at: "2026-07-30T18:26:23Z",
      },
    },
  ],
});

// open loads one of the edit addresses and returns the window, the document, and the recorded calls.
async function open(path, {canWrite = true, confirmAnswer = true} = {}) {
  const calls = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", e => {
    // jsdom refuses to navigate, which is exactly what Save and Cancel do. Recorded rather than
    // failed: the assertion is that they tried.
    if (/Not implemented: navigation/.test(e.message)) {
      calls.push({method: "NAV", url: "(navigation)"});
      return;
    }
    fail(`page threw: ${e.message}`);
  });

  const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    url: `http://127.0.0.1:8471${path}`,
    virtualConsole: vc,
    pretendToBeVisual: true,
    beforeParse(win) {
      win.fetch = async (url, init) => {
        const method = init?.method || "GET";
        calls.push({method, url, body: init?.body ? JSON.parse(init.body) : null});
        const body = url === "/api/admin" ? {secrets: true, projects: true} : state(canWrite);
        return {ok: true, status: 200, statusText: "OK",
          json: async () => body, text: async () => JSON.stringify(body)};
      };
      win.matchMedia = () => ({matches: false, addEventListener() {}});
      win.HTMLElement.prototype.scrollIntoView = function () {};
      win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
      win.requestAnimationFrame = fn => setTimeout(fn, 0);
      win.getSelection = () => ({isCollapsed: true});
      win.confirm = () => confirmAnswer;
      win.alert = msg => { calls.push({method: "ALERT", url: msg}); };
    },
  });

  const win = dom.window;
  await new Promise(r => win.addEventListener("load", r));
  await new Promise(r => setTimeout(r, 300));
  return {win, doc: win.document, calls};
}

const settle = () => new Promise(r => setTimeout(r, 120));

console.log("/projects/new is a page, not a dialog over the list");
{
  const {doc} = await open("/projects/new");

  // The add dialog specifically. `.modal` alone also matches the boot-report container that lives
  // in the document on every page.
  if (doc.getElementById("p-new-modal")) fail("the form is still in a modal");
  if (!doc.getElementById("projedit-body")?.textContent.trim()) {
    fail("the edit page rendered nothing");
  } else {
    ok("rendered on its own page");
  }
  // The list is not underneath it: this is a different page, so there are no project rows.
  if (doc.querySelector(".pcard")) fail("the project list is rendered under the form");
  else ok("no project list behind it");

  if (!doc.getElementById("p-url")) fail("no repository URL field");
  else ok("the first field is the repository URL");

  // The way back, because the browser's button is not the only way out anybody looks for.
  const back = [...doc.querySelectorAll("a")].find(a => a.getAttribute("href") === "/projects");
  if (!back) fail("no link back to the project list");
  else ok("a link back to the list");
}

console.log("");
console.log("the fields are grouped, and the groups say what they decide");
{
  const {doc} = await open("/projects/new");

  const groups = [...doc.querySelectorAll(".fgroup")];
  if (groups.length < 4) {
    fail(`${groups.length} field groups, want at least 4 — the form is one undivided column again`);
  } else {
    ok(`${groups.length} groups`);
  }
  // Each one has a heading and a sentence. A caption alone is what the previous arrangement had,
  // and it was the "no logical groupings, mushy screen" complaint.
  for (const g of groups) {
    const h = g.querySelector(".fgroup-head h4");
    const why = g.querySelector(".fgroup-head p");
    if (!h?.textContent.trim()) fail("a group has no heading");
    else if (!why?.textContent.trim()) fail(`the "${h.textContent.trim()}" group says nothing about itself`);
  }
  ok("every group has a heading and a purpose");

  // Every field must be inside a group. A field left outside one is the mush coming back.
  const loose = [...doc.querySelectorAll(".grid-form > .field")];
  if (loose.length) {
    fail(`${loose.length} field(s) outside any group`);
  } else {
    ok("no fields outside a group");
  }
}

console.log("");
console.log("pasting a repository URL fills the identity fields");
{
  const {doc, win} = await open("/projects/new");
  const url = doc.getElementById("p-url");

  for (const [given, want] of [
    ["https://github.com/acme/docs", ["github", "acme", "docs"]],
    ["git@github.com:acme/docs.git", ["github", "acme", "docs"]],
    ["https://bitbucket.org/netfoundry/customer-connect-docs",
     ["bitbucket", "netfoundry", "customer-connect-docs"]],
  ]) {
    url.value = given;
    url.dispatchEvent(new win.Event("input", {bubbles: true}));
    await settle();
    const got = [
      doc.getElementById("p-platform").value,
      doc.getElementById("p-owner").value,
      doc.getElementById("p-repo").value,
    ];
    if (got.join("/") !== want.join("/")) {
      fail(`${given} parsed as ${got.join("/")}, want ${want.join("/")}`);
    }
  }
  ok("three URL forms parsed into platform/owner/repo");
}

console.log("");
console.log("editing an existing project shows what is stored");
{
  const {doc} = await open("/projects/github/netfoundry/unified-doc");

  // The heading is the project, with its identity under it — the same shape as its row in the list.
  const head = doc.querySelector(".pedit-head");
  if (!head) {
    fail("no header identifying which project this is");
  } else {
    const text = head.textContent.replace(/\s+/g, " ");
    if (!text.includes("Unified Doc")) fail(`the header does not name the project: ${text}`);
    else if (!text.includes("netfoundry/unified-doc")) fail("the header omits owner/repo");
    else ok("the header names the project and its repository");
  }

  // The stored values are in the fields, not placeholders. A form that renders a project's settings
  // as greyed hints looks identical to one that lost them.
  const key = "github/netfoundry/unified-doc";
  for (const [f, want] of [["dir", "www"], ["command", "npm run build"], ["output", "build"]]) {
    const el = doc.getElementById(`p-${f}-${key}`);
    if (!el) fail(`no ${f} field`);
    else if (el.value !== want) fail(`${f} reads ${JSON.stringify(el.value)}, want ${want}`);
  }
  ok("the stored build settings are in the fields");

  // Identity is not editable here: it is the row's primary key, and what a webhook is matched
  // against. Changing it would be a different project.
  if (doc.getElementById("p-url")) fail("the repository URL field is offered when editing");
  else ok("no identity fields when editing");
}

console.log("");
console.log("a save sends the whole row, and then leaves the page");
{
  const {doc, calls} = await open("/projects/github/netfoundry/unified-doc");
  const key = "github/netfoundry/unified-doc";

  doc.getElementById(`p-command-${key}`).value = "npm run docs:build";
  calls.length = 0;
  doc.querySelector("[data-save]").dispatchEvent(
    new doc.defaultView.MouseEvent("click", {bubbles: true}));
  await new Promise(r => setTimeout(r, 400));

  const put = calls.find(c => c.method === "PUT" && c.url.includes("/api/projects/"));
  if (!put) {
    fail(`nothing was PUT: ${JSON.stringify(calls.map(c => `${c.method} ${c.url}`))}`);
  } else {
    if (put.url !== "/api/projects/github/netfoundry/unified-doc") fail(`PUT ${put.url}`);
    if (put.body.build_command !== "npm run docs:build") {
      fail(`it sent ${JSON.stringify(put.body.build_command)}`);
    }
    // A whole-row upsert: a field the form did not change still has to travel, or saving one
    // clears the others.
    if (put.body.build_dir !== "www") fail("the save dropped the build directory");
    if (put.body.display_name !== "Unified Doc") fail("the save dropped the display name");
    ok("PUT carries the whole row");
  }

  // And it goes back to the list, where the result is visible. Staying put on a page whose whole
  // content is the form would look exactly like the form before the click.
  if (!calls.some(c => c.method === "NAV")) {
    fail("saving did not navigate away from the form");
  } else {
    ok("navigates back after saving");
  }
}

console.log("");
console.log("Disable preserves the rest of the row");
{
  const {doc, calls} = await open("/projects/github/netfoundry/unified-doc");

  calls.length = 0;
  const toggle = [...doc.querySelectorAll("[data-toggle-enabled]")][0];
  if (!toggle) {
    fail("no Disable control on the form");
  } else {
    toggle.dispatchEvent(new doc.defaultView.MouseEvent("click", {bubbles: true}));
    await new Promise(r => setTimeout(r, 300));
    const put = calls.find(c => c.method === "PUT");
    if (!put) fail("Disable sent no request");
    else if (put.body.enabled !== false) fail(`enabled = ${put.body.enabled}, want false`);
    else if (put.body.build_command !== "npm run build") {
      fail("disabling dropped the build command");
    } else ok("PUT enabled:false with the row intact");
  }
}

console.log("");
console.log("the credential boxes are empty, and only what was typed is sent");
{
  const {doc, calls} = await open("/projects/bitbucket/netfoundry/customer-connect-docs");
  const key = "bitbucket/netfoundry/customer-connect-docs";

  // A stored token is reported as set and never rendered. Nothing on this page returns a value.
  const box = doc.getElementById(`p-scmtoken-${key}`);
  if (!box) {
    fail("no access-token field for a Bitbucket project");
  } else if (box.value !== "") {
    fail("the stored token is rendered into the form");
  } else {
    ok("the token field is empty");
  }

  // Saving with the box untouched must not send the credential route at all: an empty box means
  // "leave what is stored alone", and the alternative is that saving any other field clears it.
  calls.length = 0;
  doc.querySelector("[data-save]").dispatchEvent(
    new doc.defaultView.MouseEvent("click", {bubbles: true}));
  await new Promise(r => setTimeout(r, 400));
  if (calls.some(c => c.url.includes("/scm/"))) {
    fail("an untouched credential box was still sent");
  } else {
    ok("an untouched box sends nothing");
  }
}

console.log("");
console.log("leaving with unsaved edits is guarded");
{
  const {doc, win, calls} = await open("/projects/github/netfoundry/unified-doc",
    {confirmAnswer: false});
  const key = "github/netfoundry/unified-doc";

  // Typing is the signal, as it is in the panel version: comparing values would have to know what
  // "unchanged" means for a select whose default moved.
  const cmd = doc.getElementById(`p-command-${key}`);
  cmd.value = "something else";
  cmd.dispatchEvent(new win.Event("input", {bubbles: true}));
  await settle();

  calls.length = 0;
  const cancel = [...doc.querySelectorAll("[data-close-panel]")][0];
  if (!cancel) {
    fail("no Cancel control on the form");
  } else {
    cancel.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
    await settle();
    // confirm answered no, so nothing may navigate.
    if (calls.some(c => c.method === "NAV")) {
      fail("Cancel left the page despite unsaved edits and a declined confirm");
    } else {
      ok("declining keeps the form");
    }
    if (cmd.value !== "something else") fail("the edit was discarded anyway");
  }

  // A beforeunload guard is registered, which is what covers the back button and a typed address —
  // the two ways out that no click handler sees.
  if (typeof win.onbeforeunload !== "function") {
    fail("nothing guards the browser's own navigation");
  } else {
    ok("beforeunload is wired");
  }
}

console.log("");
console.log("an address for a project that is not here says so");
{
  const {doc} = await open("/projects/github/acme/does-not-exist");
  const text = doc.getElementById("projedit-body").textContent;

  if (!/Not found|No project/i.test(text)) {
    fail(`the page does not say the project is missing: ${text.slice(0, 120)}`);
  } else {
    ok("says the project is not there");
  }
  if (doc.querySelector("[data-save]")) {
    fail("a save button is offered for a project that does not exist");
  } else {
    ok("no form to submit");
  }
  if (!doc.querySelector('a[href="/projects"]')) fail("no way back");
}

console.log("");
console.log("a read-only daemon shows the settings and no form");
{
  const {doc} = await open("/projects/github/netfoundry/unified-doc", {canWrite: false});
  const text = doc.getElementById("projedit-body").textContent;

  if (!/Read-only/i.test(text)) fail("the page does not say it is read-only");
  else ok("says why it is read-only");
  if (doc.querySelector("[data-save]")) fail("a save button is offered where the write will 403");
  else ok("no save button");
  // The values are still readable: hiding them would make a remote reader unable to answer "what
  // is this project set to", which is not a credential.
  if (!doc.querySelector(".facts")) fail("the settings are hidden rather than shown read-only");
  else ok("the settings are shown");
}

console.log("");
console.log(`${failures} failure(s)`);
process.exit(failures ? 1 : 0);
