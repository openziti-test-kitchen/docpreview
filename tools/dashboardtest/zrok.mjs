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
    // The three exposers that cannot be configured from a browser. The panel reports them
    // so that "which of the four is this daemon using" is answerable without `doctor`.
    others: [
      {kind: "frontdoor", label: "NetFoundry Frontdoor", in_use: false, ready: false,
       needs: "the API token in the vault as frontdoor.api_token",
       what: "Public preview URLs through a NetFoundry Frontdoor tenant.",
       doc: "www/docs/runbooks/frontdoor.md"},
      {kind: "ziti", label: "OpenZiti", in_use: false, ready: true,
       what: "Previews reachable only through a tunneler with an enrolled identity.",
       doc: "www/docs/exposers.md"},
      {kind: "local", label: "This daemon only", in_use: false, ready: true,
       what: "Previews served from the daemon's own listener, under /preview/."},
    ],
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
  return {win, doc: win.document, posted, body: win.document.getElementById("exposer-body")};
}

const text = el => (el?.textContent || "").replace(/\s+/g, " ").trim();

// wiz opens the setup wizard from a row and returns handles for driving it.
const wiz = doc => ({
  card: () => doc.querySelector(".modal.wiz"),
  title: () => text(doc.querySelector(".modal.wiz h3")),
  body: () => text(doc.querySelector(".modal.wiz")),
  act: label => [...doc.querySelectorAll(".modal.wiz [data-wiz-act]")]
    .find(b => new RegExp(label, "i").test(b.textContent)),
  back: () => doc.querySelector(".modal.wiz [data-wiz-back]"),
  field: id => doc.querySelector(`.modal.wiz #${id}`),
});

console.log("nothing enrolled: the row opens a wizard rather than a form");
{
  const {body, doc} = await load(zrokState());

  // The page itself carries no signup fields any more. Four decisions in a fixed order,
  // two of which are only possible once the one before has happened, laid out flat gave a
  // button that recorded half a decision and left the next step to be found further down.
  for (const id of ["zrok-invite", "zrok-register", "zrok-enable"]) {
    if (doc.getElementById(id)) fail(`${id} is still on the page outside the wizard`);
  }
  ok("no signup fields on the page");

  const open = doc.querySelector('[data-zrok-root="project"] [data-zrok-wizard]');
  if (!open) {
    fail("an empty directory offers no way to set zrok up");
  } else {
    // The label says what it does. "Point the daemon here" described one internal step of
    // four and read as an instruction to the reader about the daemon.
    if (!/configure the daemon for zrok/i.test(text(open))) {
      fail(`the control reads "${text(open)}"`);
    } else ok(`reads "${text(open)}"`);

    const W = wiz(doc);
    if (W.card()) fail("the wizard was open before anything was clicked");
    open.click();
    await new Promise(r => setTimeout(r, 40));
    if (!W.card()) {
      fail("clicking it opened no wizard");
    } else ok("it opens a wizard");
  }

  // Both directories are named, because "which zrok am I using" is the question this
  // panel exists to answer and a path is the only unambiguous answer to it.
  for (const [what, dir] of [["project", PROJECT_DIR], ["system", SYSTEM_DIR]]) {
    if (!text(body).includes(dir)) fail(`the ${what} directory is not shown`);
  }
  ok("both directories are named");
}

console.log("\nthe wizard walks signup end to end");
{
  const {doc, posted} = await load(zrokState());
  const W = wiz(doc);
  doc.querySelector('[data-zrok-root="project"] [data-zrok-wizard]').click();
  await new Promise(r => setTimeout(r, 40));

  // 1. Where. The directory choice is recorded before anything is written into it: the
  // enrolment endpoints act on the directory this process has loaded, so a restart would
  // otherwise look elsewhere for what the wizard just created.
  if (!/where/i.test(W.title())) fail(`step 1 is "${W.title()}"`);
  else ok(`step 1: ${W.title()}`);
  W.act("next").click();
  await new Promise(r => setTimeout(r, 60));
  const use = posted.find(p => p.url === "/api/zrok/use");
  if (!use || use.body?.scope !== "project") {
    fail(`the directory choice was not recorded: ${JSON.stringify(use)}`);
  } else ok("it records the directory first");

  // 2. Account. Two routes, and the one for somebody who already has a token is on the
  // same screen — an operator with an account should not have to work out that the signup
  // form is not for them.
  if (!/account/i.test(W.title())) fail(`step 2 is "${W.title()}"`);
  else ok(`step 2: ${W.title()}`);
  if (!W.act("i have a token")) fail("no route for an existing account");
  else ok("both routes are offered");

  // 3. Invite.
  W.act("sign me up").click();
  await new Promise(r => setTimeout(r, 40));
  W.field("wiz-email").value = "someone@example.com";
  W.act("email me a link").click();
  await new Promise(r => setTimeout(r, 60));
  const invite = posted.find(p => p.url === "/api/zrok/invite");
  if (invite?.body?.email !== "someone@example.com") {
    fail(`the invite posted ${JSON.stringify(invite?.body)}`);
  } else ok("it requests the invite");

  // 4. Finish. The email is a gap in the middle that no arrangement closes; what the wizard
  // adds is somewhere to say so.
  if (!/finish/i.test(W.title())) fail(`step 4 is "${W.title()}"`);
  else ok(`step 4: ${W.title()}`);
  if (!/email/i.test(W.body())) fail("it does not tell the reader to go and read their mail");
  else ok("it says to open the email");

  W.field("wiz-link").value = "https://zrok.io/register/abc123";
  W.field("wiz-password").value = "a-zrok-password";
  W.act("create the account").click();
  await new Promise(r => setTimeout(r, 60));
  const reg = posted.find(p => p.url === "/api/zrok/register");
  if (reg?.body?.link !== "https://zrok.io/register/abc123" || !reg?.body?.password) {
    fail(`the registration posted ${JSON.stringify(reg?.body)}`);
  } else ok("it registers with the link and the password");

  // 5. Done, and it says what to do next rather than just closing.
  if (!/enrolled/i.test(W.title())) fail(`the last step is "${W.title()}"`);
  else ok(`step 5: ${W.title()}`);
  if (!/exposer\.kind/.test(W.body())) {
    fail("the last step does not mention setting exposer.kind");
  } else ok("it says what to do next");
}

console.log("\nthe wizard's token route skips the email entirely");
{
  const {doc, posted} = await load(zrokState({has_account_token: true}));
  const W = wiz(doc);
  doc.querySelector('[data-zrok-root="project"] [data-zrok-wizard]').click();
  await new Promise(r => setTimeout(r, 40));
  W.act("next").click();
  await new Promise(r => setTimeout(r, 60));
  W.act("i have a token").click();
  await new Promise(r => setTimeout(r, 40));

  // A stored token makes the field optional, and the placeholder has to say so or it reads
  // as a required box with nothing to put in it.
  const box = W.field("wiz-token");
  if (!box) {
    fail("no token field on the token route");
  } else if (!/leave blank/i.test(box.getAttribute("placeholder") || "")) {
    fail(`the field does not say it is optional: ${box.getAttribute("placeholder")}`);
  } else ok("it offers to use the stored token");

  W.act("enrol this host").click();
  await new Promise(r => setTimeout(r, 60));
  if (!posted.find(p => p.url === "/api/zrok/enable")) {
    fail("the token route posted no enrolment");
  } else ok("it enrols");
  if (posted.find(p => p.url === "/api/zrok/invite")) {
    fail("the token route asked for an email invite");
  } else ok("no email round trip");
}

console.log("\nBack goes back, and the wizard closes without doing anything");
{
  const {doc, posted} = await load(zrokState());
  const W = wiz(doc);
  doc.querySelector('[data-zrok-root="project"] [data-zrok-wizard]').click();
  await new Promise(r => setTimeout(r, 40));

  // The first screen has nowhere to go back to, so it must not offer it.
  if (W.back()) fail("the first step offers Back");
  else ok("no Back on the first step");

  W.act("next").click();
  await new Promise(r => setTimeout(r, 60));
  if (!W.back()) {
    fail("no way back from the second step");
  } else {
    W.back().click();
    await new Promise(r => setTimeout(r, 40));
    if (!/where/i.test(W.title())) fail(`Back landed on "${W.title()}"`);
    else ok("Back returns to the previous step");
  }

  const before = posted.length;
  doc.querySelector(".modal.wiz [data-wiz-close]").click();
  await new Promise(r => setTimeout(r, 40));
  if (W.card()) fail("closing left the wizard open");
  else ok("it closes");
  // Closing re-reads the panel, which is one GET. Nothing else may have been sent.
  if (posted.length !== before) {
    fail(`closing sent ${posted.length - before} mutation(s)`);
  } else ok("closing changes nothing");
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

  // The absent form is explained by state rather than by a sentence about it. An earlier
  // version wrote out the reasoning — "a second account would have its own set of names…" —
  // which is true, is in the design doc, and is not something a reader of this page needs.
  //
  // Nor is there a row for the account token any more: it is a vault entry, listed in the
  // credential panel above with every other one, and a second place reporting the same fact
  // answered a question the panel twelve lines up already answers.
  if (!/enrolled/.test(text(body))) {
    fail(`${where}: nothing on the panel says an account is enrolled`);
  } else ok(`${where}: it reports the enrolment`);
  if (/Account token/.test(text(body))) {
    fail(`${where}: the account token has a row of its own again`);
  } else ok(`${where}: no duplicate token row`);

  // Un-enrolling is on the row it acts on, and only for this installation's own directory.
  //
  // It used to be a button in a row of its own below both directories, which made it a
  // control with no visible subject — "un-enrol this host" with two hosts listed above it.
  //
  // And never for the machine's: `~/.zrok2` belongs to the zrok CLI and to whatever else that
  // account is used for — a share somebody left running, another tool, a colleague's scripts.
  // Deleting it from a documentation-preview dashboard takes those with it, and nobody would
  // think to look here for the cause.
  const scope = over.in_force;
  const un = doc.querySelectorAll("[data-zrok-unenrol]");
  if (scope === "system") {
    if (un.length) {
      fail(`${where}: offered to delete the machine's zrok environment`);
    } else ok(`${where}: no un-enrol for the machine's environment`);
  } else if (un.length !== 1) {
    fail(`${where}: ${un.length} un-enrol controls, want exactly one`);
  } else if (un[0].dataset.zrokUnenrol !== scope) {
    fail(`${where}: un-enrol is on the ${un[0].dataset.zrokUnenrol} row, want ${scope}`);
  } else if (!un[0].closest(`[data-zrok-root="${scope}"]`)) {
    fail(`${where}: un-enrol is not inside the row it acts on`);
  } else ok(`${where}: un-enrolling sits on the ${scope} row`);
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
  // Both enrolled, because "use this one" only exists over a directory that has something
  // in it — an empty one gets the wizard instead, which is the point of the split.
  const {doc, posted} = await load(zrokState({
    enrolled: true, stored: "system", in_force: "system",
    system: {path: SYSTEM_DIR, exists: true, enabled: true},
    project: {path: PROJECT_DIR, exists: true, enabled: true},
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
  if (doc.getElementById("exposer-head")?.hidden) {
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

console.log("\nwith an account already enrolled, the wizard skips the signup");
{
  // The account is in one directory and the operator opens the wizard on the other. Offering
  // "sign me up" here would be offering a route that ends in the server's 409 — there is
  // already an account, and a second one would have its own set of names while every preview
  // URL already posted to a pull request lives on the first.
  const {doc, posted} = await load(zrokState({
    enrolled: true, stored: "system", in_force: "system", has_account_token: true,
    project: {path: PROJECT_DIR, exists: false, enabled: false},
    system: {path: SYSTEM_DIR, exists: true, enabled: true},
  }));
  const W = wiz(doc);
  doc.querySelector('[data-zrok-root="project"] [data-zrok-wizard]').click();
  await new Promise(r => setTimeout(r, 40));
  W.act("next").click();
  await new Promise(r => setTimeout(r, 60));

  if (/account\?/i.test(W.title())) {
    fail("it asked whether an account exists when one already does");
  } else if (!/token/i.test(W.title())) {
    fail(`landed on "${W.title()}", want the token step`);
  } else ok(`skips to "${W.title()}"`);

  // And it names the cost, because a quota is a thing the operator cannot see.
  if (!/quota/.test(W.body())) {
    fail("nothing says a second enrolment costs an environment");
  } else ok("it names the cost");

  // Back has to reach the step before, which is the directory choice — not the account
  // question, which was never asked.
  W.back()?.click();
  await new Promise(r => setTimeout(r, 40));
  if (!/where/i.test(W.title())) {
    fail(`Back landed on "${W.title()}" from a skipped route`);
  } else ok("Back skips it too");

  if (posted.find(p => p.url === "/api/zrok/invite")) {
    fail("an invite was requested");
  }
}

console.log("\nthe machine's directory is described as a user account, not a host");
{
  const {body} = await load(zrokState({run_as: "svc-docpreview"}));
  const t = text(body);
  // `~/.zrok2` is per-user. A daemon under a service account reports nothing enrolled while
  // `zrok2 overview` in the operator's own shell shows a working environment, and a row
  // labelled "This machine" makes that difference impossible to see.
  if (!t.includes("svc-docpreview")) {
    fail("the row does not name the account the daemon runs as");
  } else ok("it names the account");
}

console.log("\nan empty directory is never offered as something to use");
{
  // "Use this one" over a directory with nothing in it was the complaint that started this:
  // selecting it cannot mean "publish through this", because there is nothing there. The
  // empty one gets the wizard, the enrolled one gets the switch, and neither gets both.
  const {doc} = await load(zrokState({
    enrolled: true, stored: "system", in_force: "system",
    system: {path: SYSTEM_DIR, exists: true, enabled: true},
    project: {path: PROJECT_DIR, exists: false, enabled: false},
  }));
  const empty = doc.querySelector('[data-zrok-root="project"]');
  const full = doc.querySelector('[data-zrok-root="system"]');

  if (empty.querySelector("[data-zrok-use]")) {
    fail("the empty directory is offered as something to use");
  } else ok("no switch control over an empty directory");
  if (!empty.querySelector("[data-zrok-wizard]")) {
    fail("the empty directory offers no wizard");
  } else ok("the empty one offers the wizard");
  if (full.querySelector("[data-zrok-wizard]")) {
    fail("an enrolled directory offers the setup wizard");
  } else ok("no wizard over an enrolled directory");
}

console.log("\nan enrolment against the wrong zrok version is called out, not shown as working");
{
  // The state this catches: enrolled, selected, and dead on the first publish. Everything
  // else about it says ready — there is an account, a directory and an endpoint — so without
  // a distinct signal it reads as a working setup until a build fails with a 404.
  const {doc, body} = await load(zrokState({
    enrolled: true, stored: "system", in_force: "system",
    system: {path: SYSTEM_DIR, exists: true, enabled: true,
      api_endpoint: "https://api.zrok.io", enabled_at: null,
      unsupported: "this is enrolled against the zrok v1 service (https://api.zrok.io). " +
        "docpreview needs v2, which has the namespaces and reserved names a stable preview " +
        "URL depends on — v1 has neither"},
  }));

  const t = text(body);
  if (!/not zrok v2/.test(t)) {
    fail("a v1 enrolment is not flagged");
  } else ok("flagged as not zrok v2");
  if (!/v1 has neither/.test(t)) {
    fail("it does not say why v1 cannot work");
  } else ok("it says why v1 cannot work");

  // And the section reads as broken rather than as the working exposer, because the stripe
  // is the only thing anybody looks at before expanding it.
  const zrok = doc.querySelector('[data-exposer="zrok2"]');
  if (!zrok?.classList.contains("broken")) {
    fail(`the zrok section is not marked broken: ${zrok?.className}`);
  } else ok("the section is marked broken");
  if (/>enrolled</.test(zrok.querySelector("summary").innerHTML)) {
    fail("the summary still claims it is enrolled and fine");
  } else ok("the summary does not claim it is fine");
}

console.log("\nevery exposer has a section, and one of them is marked in use");
{
  const {doc, body} = await load(zrokState({
    exposer_kind: "ziti",
    others: zrokState().others.map(o => ({...o, in_use: o.kind === "ziti"})),
  }));

  const sections = [...doc.querySelectorAll("[data-exposer]")].map(d => d.dataset.exposer);
  for (const want of ["zrok2", "frontdoor", "ziti", "local"]) {
    if (!sections.includes(want)) fail(`no section for ${want}`);
  }
  if (sections.length === 4) ok(`four sections: ${sections.join(", ")}`);

  // Exactly one, because "in use" is exposer.kind and there is one of those. Two would
  // mean the page had invented a state the daemon cannot be in.
  const inUse = [...doc.querySelectorAll("[data-exposer].inuse")].map(d => d.dataset.exposer);
  if (inUse.length !== 1 || inUse[0] !== "ziti") {
    fail(`marked in use: ${JSON.stringify(inUse)}, want exactly ["ziti"]`);
  } else ok("only the configured exposer is marked in use");

  // The one in use is the section anybody arriving here wants; the rest are reference.
  const open = [...doc.querySelectorAll("[data-exposer][open]")].map(d => d.dataset.exposer);
  if (!open.includes("ziti")) {
    fail(`the exposer in use is collapsed; open: ${JSON.stringify(open)}`);
  } else ok("the exposer in use is expanded");

  // A missing credential is named rather than left as "not configured".
  if (!text(body).includes("frontdoor.api_token")) {
    fail("Frontdoor is unconfigured and the panel does not say what it needs");
  } else ok("it names what an unconfigured exposer needs");
}

console.log("\nevery exposer can be selected from the page, and only when it would start");
{
  // Switching exposers used to be "edit config.yml and restart" — on a page built to avoid
  // exactly that. The kind is a stored setting now, so every section carries the same control.
  const {doc, posted} = await load(zrokState({
    enrolled: true, stored: "project", in_force: "project", exposer_kind: "local",
    project: {path: PROJECT_DIR, exists: true, enabled: true},
    others: [
      {kind: "frontdoor", label: "NetFoundry Frontdoor", in_use: false, ready: false,
       needs: "the API token", what: "…", setup: "frontdoor"},
      {kind: "ziti", label: "OpenZiti", in_use: false, ready: true, what: "…", setup: "ziti"},
      {kind: "local", label: "This daemon only", in_use: true, ready: true, what: "…"},
    ],
  }));

  // The enabled one offers no button — it is already the answer — but it does say so, because a
  // row with nothing where every other row has a control reads as a control that failed to
  // render.
  const local = doc.querySelector('[data-exposer="local"]');
  if (local.querySelector("[data-exposer-use]")) {
    fail("the enabled exposer offers to be enabled again");
  } else ok("no button on the enabled exposer");
  if (!/enabled/.test(text(local))) {
    fail("the enabled exposer does not say it is enabled");
  } else ok("it says it is enabled");

  // And there is no Disable anywhere. Exactly one exposer publishes previews, so disabling one
  // would have to mean "publish nothing", which is not a state the daemon has.
  const disables = [...doc.querySelectorAll("#exposer-body button")]
    .filter(b => /disable/i.test(b.textContent));
  if (disables.length) {
    fail(`${disables.length} Disable control(s) for a one-of-four choice`);
  } else ok("no Disable, because the choice is one of four");
  if (!/turns the current one off/i.test(text(doc.getElementById("exposer-body")))) {
    fail("nothing says that enabling one turns the others off");
  } else ok("it says enabling one turns the others off");

  // A configured one can be chosen — behind a confirmation, because at the next restart every
  // preview gets a new URL and every open pull request comment is rewritten. One click and a
  // tooltip put `http://127.0.0.1:8471/...` into seven pull requests.
  const ziti = doc.querySelector('[data-exposer="ziti"] [data-exposer-use]');
  if (!ziti) {
    fail("a configured exposer cannot be selected");
  } else {
    ziti.click();
    await new Promise(r => setTimeout(r, 40));
    const dialog = doc.querySelector(".modal.ask");
    if (!dialog) {
      fail("enabling an exposer posted with no confirmation");
    } else {
      ok("it asks first");
      const t = text(dialog);
      // The two consequences that are not guessable, and the one that is the point.
      if (!/pull request/i.test(t)) fail("the dialog does not mention the comments");
      else if (!/URL/i.test(t)) fail("the dialog does not mention the URLs changing");
      else ok("it names what changes");

      // Cancelling sends nothing.
      dialog.querySelector('[data-ask="no"]').click();
      await new Promise(r => setTimeout(r, 40));
      if (posted.find(p => p.url === "/api/zrok/exposer")) {
        fail("cancelling still switched the exposer");
      } else ok("Cancel changes nothing");

      ziti.click();
      await new Promise(r => setTimeout(r, 40));
      doc.querySelector('.modal.ask [data-ask="yes"]').click();
      await new Promise(r => setTimeout(r, 60));
      const call = posted.find(p => p.url === "/api/zrok/exposer");
      if (call?.body?.kind !== "ziti") {
        fail(`confirming posted ${JSON.stringify(call?.body)}`);
      } else ok("confirming posts its kind");
    }
  }

  // An unconfigured one must not be selectable: recording it produces a daemon that will not
  // start, and the page that would fix it is served by that daemon.
  const fd = doc.querySelector('[data-exposer="frontdoor"]');
  const fdUse = fd.querySelector("[data-exposer-use]");
  const fdDisabled = [...fd.querySelectorAll("button")].some(b => b.disabled);
  if (fdUse) {
    fail("an exposer with no credential can be selected");
  } else if (!fdDisabled) {
    fail("an unconfigured exposer offers no control at all, not even a disabled one");
  } else ok("an unconfigured exposer is offered but disabled");

  // zrok's section gets the same control, since it is one of the four.
  if (!doc.querySelector('[data-exposer="zrok2"] [data-exposer-use]')) {
    fail("zrok cannot be selected from its own section");
  } else ok("zrok has the same control");
}

console.log("\na chosen exposer that is not running yet says so, in three places");
{
  // The reported bug: pressing Enable and watching nothing move. The chips come from the
  // *running* exposer, which does not change until a restart, so a panel that reported only
  // that state looked inert — the click had worked and there was no way to tell.
  const {doc, body} = await load(zrokState({
    enrolled: true, stored: "project", in_force: "project",
    exposer_kind: "zrok2", exposer_stored: "ziti",
    project: {path: PROJECT_DIR, exists: true, enabled: true},
    others: [
      {kind: "frontdoor", label: "NetFoundry Frontdoor", in_use: false, ready: false,
       needs: "the API token", what: "…", setup: "frontdoor"},
      {kind: "ziti", label: "OpenZiti", in_use: false, ready: true, what: "…", setup: "ziti"},
      {kind: "local", label: "This daemon only", in_use: false, ready: true, what: "…"},
    ],
  }));

  // 1. A banner, because the restart is the moment every preview URL changes.
  if (!/Restart to apply/.test(text(body))) {
    fail("nothing says a restart is owed");
  } else ok("a banner says a restart is owed");

  // 2. The chosen section's summary, which is where somebody looks when it is collapsed.
  const zitiChips = text(doc.querySelector('[data-exposer="ziti"] summary'));
  if (!/next restart/.test(zitiChips)) {
    fail(`the chosen exposer's summary reads "${zitiChips}"`);
  } else ok("the chosen section says it starts at the next restart");

  // 3. The running one, which is still running and already replaced.
  const zrokChips = text(doc.querySelector('[data-exposer="zrok2"] summary'));
  if (!/until the restart/.test(zrokChips)) {
    fail(`the running exposer's summary reads "${zrokChips}"`);
  } else ok("the running section says it is running until then");

  // And the chosen one offers no second Enable: pressing it again would do nothing and look
  // like the first press had failed.
  if (doc.querySelector('[data-exposer="ziti"] [data-exposer-use]')) {
    fail("the already-chosen exposer can be enabled again");
  } else ok("no repeat control on the chosen exposer");
}

console.log("\nFrontdoor's five fields are on the page, credential included");
{
  const {doc, posted} = await load(zrokState({
    frontdoor: {api_base: "https://gw.example/frontdoor/abc", frontend: "bMTHPrtQ",
      env_z_id: "", agent_reachable_host: "", has_token: true},
    others: [
      {kind: "frontdoor", label: "NetFoundry Frontdoor", in_use: true, ready: false,
       needs: "the agent's ziti identity id", what: "…", setup: "frontdoor"},
      {kind: "ziti", label: "OpenZiti", in_use: false, ready: true, what: "…", setup: "ziti"},
      {kind: "local", label: "This daemon only", in_use: false, ready: true, what: "…"},
    ],
  }));

  for (const id of ["fd-token", "fd-api-base", "fd-frontend", "fd-env-z-id", "fd-agent-host"]) {
    if (!doc.getElementById(id)) fail(`no ${id} field`);
  }
  ok("all five fields");

  // What is already set comes back filled in. Five empty boxes on a working installation is a
  // form that cannot tell you what it is about to change.
  if (doc.getElementById("fd-api-base").value !== "https://gw.example/frontdoor/abc") {
    fail("the gateway URL is not pre-filled");
  } else ok("existing values are pre-filled");

  // The credential never is, and blank means keep it — which is what makes the form
  // re-submittable to correct a frontend id without re-pasting a token.
  if (doc.getElementById("fd-token").value) {
    fail("the token field is pre-filled with something");
  } else ok("the token field is always empty");

  doc.getElementById("fd-env-z-id").value = "agent-identity-id";
  doc.getElementById("fd-save").click();
  await new Promise(r => setTimeout(r, 60));
  const save = posted.find(p => p.url === "/api/zrok/frontdoor");
  if (save?.body?.env_z_id !== "agent-identity-id") {
    fail(`saving posted ${JSON.stringify(save?.body)}`);
  } else ok("it saves what was typed");
  if (save?.body?.token) {
    fail("an untouched token field was sent as a value");
  } else ok("an untouched token is not sent");

  // Testing is refused while the token is missing, because the answer would be about the
  // missing token rather than about the tenant.
  const test = doc.getElementById("fd-test");
  if (!test) fail("no way to test the token");
  else ok("testing is offered");
}

console.log("\nziti enrols from a pasted JWT");
{
  // The last thing on this panel that needed a second binary. The SDK does the same exchange
  // `ziti edge enroll` does, so the whole step is a textarea.
  const {doc, posted} = await load(zrokState({
    ziti: {service: "docpreview-previews", domain: "preview.ziti"},
    others: [
      {kind: "frontdoor", label: "NetFoundry Frontdoor", in_use: false, ready: false,
       needs: "the API token", what: "…", setup: "frontdoor"},
      {kind: "ziti", label: "OpenZiti", in_use: true, ready: false,
       needs: "exposer.ziti.identity_file", what: "…", setup: "ziti"},
      {kind: "local", label: "This daemon only", in_use: false, ready: true, what: "…"},
    ],
  }));

  const jwt = doc.getElementById("ziti-jwt");
  if (!jwt) {
    fail("no field for the enrolment token");
  } else ok("there is a field for the JWT");
  if (doc.getElementById("ziti-service")?.value !== "docpreview-previews") {
    fail("the service is not pre-filled");
  } else ok("service and domain are pre-filled");

  // The one-time nature is stated. Pasting the same token twice cannot work, and the reason —
  // the controller marks it spent and the key exists only in the file written here — is not
  // guessable.
  const t = text(doc.querySelector('[data-exposer="ziti"]'));
  if (!/once/.test(t)) {
    fail("nothing says the enrolment token works only once");
  } else ok("it says the token works once");

  jwt.value = "eyJhbGciOiJFUzI1NiJ9.eyJlbSI6Im90dCJ9.sig";
  doc.getElementById("ziti-enroll").click();
  await new Promise(r => setTimeout(r, 60));
  const call = posted.find(p => p.url === "/api/zrok/ziti/enroll");
  if (!call?.body?.jwt) {
    fail(`enrolling posted ${JSON.stringify(call?.body)}`);
  } else if (call.body.service !== "docpreview-previews") {
    fail("the service was not sent with the enrolment");
  } else ok("it posts the token, the service and the domain together");
}

console.log("\nthe rows are padded, not flush against the border");
{
  // Every row lives inside a .secgroup-box, which is the only rule that gives .sec any
  // horizontal padding — the first version put them straight into .secbox and the text
  // touched the border. Asserted structurally because jsdom resolves no layout.
  const {doc} = await load(zrokState());
  const loose = [...doc.querySelectorAll("#exposer-body .sec")]
    .filter(el => !el.closest(".secgroup-box"));
  if (loose.length) {
    fail(`${loose.length} row(s) outside a .secgroup-box, so they render unpadded`);
  } else ok("every row is inside a padded group box");
}

console.log(failures === 0 ? "\nall exposer panel checks OK" : `\n${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
