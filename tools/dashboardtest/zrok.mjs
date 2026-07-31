// Drives the zrok panel on /secrets through the states that decide what it must not do.
//
// # Why this exists
//
// Two of the panel's rules are invisible by reading it. It must never offer a signup when an
// account is already enrolled — a second account means a second set of reserved names, and every
// preview URL already pasted into a pull request lives on the first. And when both zrok
// environments are enrolled it must refuse to imply a default, because they are different accounts
// and startup deletes every share it recognises as its own.
//
// Neither is expressible as a unit test on the Go side: both are decisions the page makes about
// what to render.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node zrok.mjs
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

const PROJECT_DIR = "D:\\docpreview\\.docpreview\\zrok2";
const SYSTEM_DIR = "C:\\Users\\someone\\.zrok2";

// zrokState builds the payload /api/zrok returns, with the parts each case varies.
function zrokState(over = {}) {
  return {
    available: true,
    can_write: true,
    exposer_kind: "zrok2",
    default_api_endpoint: "https://api-v2.zrok.io",
    stored: "",
    in_force: "project",
    enrolled: false,
    has_account_token: false,
    vault_locked: false,
    must_choose: false,
    project: {path: PROJECT_DIR, exists: false, enabled: false},
    system: {path: SYSTEM_DIR, exists: false, enabled: false},
    ...over,
  };
}

// load renders /secrets with the given zrok payload and returns the panel.
async function load(state) {
  const posted = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", () => {});

  const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    // /secrets, because that is the only page the panel is on.
    url: "http://127.0.0.1:8471/secrets",
    virtualConsole: vc,
    beforeParse(win) {
      win.EventSource = class {
        constructor() { this.readyState = 1; }
        addEventListener() {}
        close() {}
      };
      win.fetch = async (url, init) => {
        if (init?.method && init.method !== "GET") {
          posted.push({url, body: init.body ? JSON.parse(init.body) : null});
        }
        const body =
          url === "/api/zrok" || url.startsWith("/api/zrok/") ? state :
          // The credential panel, which shares this page. A locked vault is the least
          // interesting state for it here and it keeps the fixture small.
          url === "/api/secrets" ? {available: true, can_write: true, locked: true,
            vault_path: "vault.age", has_vault: true, entries: []} :
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
  await new Promise(r => setTimeout(r, 120));
  return {win, doc: win.document, posted, body: win.document.getElementById("zrok-body")};
}

const text = el => (el?.textContent || "").replace(/\s+/g, " ").trim();

console.log("nothing enrolled: the signup is offered");
{
  const {body, doc} = await load(zrokState());
  if (!doc.getElementById("zrok-invite")) {
    fail("no signup form on a fresh installation");
  } else ok("the invite step is there");
  if (!doc.getElementById("zrok-register")) {
    fail("no way to finish the signup");
  } else ok("the finish step is there");
  if (!doc.getElementById("zrok-enable")) {
    fail("no path for somebody who already has an account token");
  } else ok("an existing account can be used instead");

  // Both directories are named, because "which zrok am I using" is the question this
  // panel exists to answer and a path is the only unambiguous answer to it.
  for (const [what, dir] of [["project", PROJECT_DIR], ["system", SYSTEM_DIR]]) {
    if (!text(body).includes(dir)) fail(`the ${what} directory is not shown`);
  }
  ok("both directories are named");
}

console.log("\nalready enrolled: no second signup, whichever root it is in");
for (const [where, over] of [
  ["this installation", {enrolled: true, stored: "project", in_force: "project",
    project: {path: PROJECT_DIR, exists: true, enabled: true,
      api_endpoint: "https://api-v2.zrok.io", namespace: "public"}}],
  ["this machine", {enrolled: true, stored: "system", in_force: "system",
    system: {path: SYSTEM_DIR, exists: true, enabled: true,
      api_endpoint: "https://api-v2.zrok.io", namespace: "public"}}],
]) {
  const {body, doc} = await load(zrokState(over));
  const offered = ["zrok-invite", "zrok-register", "zrok-enable"]
    .filter(id => doc.getElementById(id));
  if (offered.length) {
    fail(`enrolled in ${where} and the panel still offers ${offered.join(", ")}`);
  } else ok(`${where}: no signup offered`);

  if (!text(body).includes("nothing to sign up for")) {
    fail(`${where}: the panel does not say why the signup is gone`);
  } else ok(`${where}: it says why`);

  if (!doc.getElementById("zrok-disable")) {
    fail(`${where}: no way out of a wrong enrolment`);
  } else ok(`${where}: Disable is offered`);
}

console.log("\nboth enrolled and nothing chosen: it must not imply a default");
{
  const {body} = await load(zrokState({
    enrolled: true, stored: "", must_choose: true,
    project: {path: PROJECT_DIR, exists: true, enabled: true},
    system: {path: SYSTEM_DIR, exists: true, enabled: true},
  }));
  const t = text(body);
  if (!/Two zrok environments are enrolled/.test(t)) {
    fail("no warning that two accounts are in play");
  } else ok("it warns");
  // The consequence, not just the fact. "Pick one" without "or it deletes the other
  // account's shares" is a choice nobody has a reason to make carefully.
  if (!/deletes/.test(t)) {
    fail("the warning does not say what the wrong choice costs");
  } else ok("it says what the wrong choice costs");
}

console.log("\nchoosing an environment posts the choice and warns about the restart");
{
  const {doc, posted} = await load(zrokState({
    stored: "system", in_force: "system",
    system: {path: SYSTEM_DIR, exists: true, enabled: true},
  }));
  // The row already chosen offers nothing; the other one does.
  const rows = [...doc.querySelectorAll("[data-zrok-root]")];
  const chosen = rows.find(r => r.dataset.zrokRoot === "system");
  const other = rows.find(r => r.dataset.zrokRoot === "project");
  if (chosen?.querySelector("[data-zrok-use]")) {
    fail("the environment already in use offers to be selected again");
  } else ok("no pointless control on the row already in use");

  const pick = other?.querySelector("[data-zrok-use]");
  if (!pick) {
    fail("the other environment cannot be selected");
  } else {
    pick.click();
    await new Promise(r => setTimeout(r, 60));
    const call = posted.find(p => p.url === "/api/zrok/use");
    if (!call) {
      fail("selecting an environment posted nothing");
    } else if (call.body?.scope !== "project") {
      fail(`posted scope ${JSON.stringify(call.body)}`);
    } else ok("posts the scope it was asked for");
  }
}

console.log("\na stored choice the process has not adopted says a restart is needed");
{
  const {body} = await load(zrokState({
    stored: "project", in_force: "system",
    project: {path: PROJECT_DIR, exists: true, enabled: true},
    system: {path: SYSTEM_DIR, exists: true, enabled: true},
    enrolled: true,
  }));
  // zrok's root directory is a process-wide global, so a changed choice takes a restart.
  // A panel that showed only the stored value would report a state the daemon is not in.
  const t = text(body);
  if (!/restart/i.test(t)) {
    fail("nothing says the change needs a restart");
  } else ok("it says a restart is needed");
  if (!/in use/.test(t)) {
    fail("the panel does not say which one is actually in use");
  } else ok("it distinguishes chosen from in use");
}

console.log("\nread-only: it reports, and offers nothing");
{
  const {body, doc} = await load(zrokState({
    can_write: false,
    read_only_why: "this session is signed in as a viewer",
    enrolled: true, stored: "project", in_force: "project",
    project: {path: PROJECT_DIR, exists: true, enabled: true},
  }));
  const controls = [...body.querySelectorAll("button")];
  if (controls.length) {
    fail(`${controls.length} control(s) offered to a reader who cannot use them`);
  } else ok("no controls");
  if (!text(body).includes(PROJECT_DIR)) {
    fail("a reader cannot see which environment is in use");
  } else ok("still says which environment is in use");
  if (!text(body).includes("viewer")) {
    fail("does not say why it is read-only");
  } else ok("says why");
  if (doc.getElementById("zrok-head")?.hidden) {
    fail("the section was hidden rather than shown read-only");
  }
}

console.log("\nenrolled under another exposer: the panel says it is doing nothing");
{
  const {body} = await load(zrokState({
    exposer_kind: "local", enrolled: true, stored: "project", in_force: "project",
    project: {path: PROJECT_DIR, exists: true, enabled: true},
  }));
  if (!/publishes with/.test(text(body))) {
    fail("an enrolled zrok on a local-exposer daemon looks like a working setup");
  } else ok("it says the exposer is not zrok");
}

console.log(failures === 0 ? "\nall zrok panel checks OK" : `\n${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
