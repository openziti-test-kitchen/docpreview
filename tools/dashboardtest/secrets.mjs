// Drives the credential page: the three groups, what lands in which, and the
// collapse that has to survive a save.
//
// # Why this exists
//
// The grouping is a security boundary rather than a layout choice. Everything under
// "Platform and exposer credentials" is used by docpreview to verify a webhook, clone,
// or comment, and is never handed to a build. Everything under "Build variables" is
// injected into every build as an environment variable, where the pull request's own
// script decides what to do with it. One flat list put a GitHub App private key three
// rows above a Docusaurus API key with nothing to say which was which — which is how
// six tokens came to be stored in the belief that storing them was enough.
//
// So the assertion that matters is not "three headings render". It is that a dotted
// daemon key never appears under Build variables, and an UPPER_CASE key never appears
// under credentials.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/secrets.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

// One entry of each kind, including a missing required credential and a key nothing
// reads. Mirrors what SecretsAdmin.snapshot builds.
const state = () => ({
  available: true, can_write: true, locked: false,
  vault_path: "C:\\vault.age", has_vault: true,
  entries: [
    {key: "github.private_key", set: true, label: "GitHub App private key",
     hint: "the .pem GitHub generated", required: true, group: "daemon"},
    {key: "github.webhook_secret", set: true, label: "GitHub webhook secret",
     hint: "the value in the App's Webhook secret field", required: true, group: "daemon"},
    {key: "bitbucket.webhook_secret", set: false, label: "Bitbucket webhook secret",
     hint: "generate it here", required: true, group: "daemon"},
    {key: "BB_REPO_TOKEN_ONPREM", set: true, label: "BB_REPO_TOKEN_ONPREM",
     env_var: "BB_REPO_TOKEN_ONPREM", group: "build"},
    {key: "GH_ZITI_CI_REPO_ACCESS_PAT", set: true, label: "GH_ZITI_CI_REPO_ACCESS_PAT",
     env_var: "GH_ZITI_CI_REPO_ACCESS_PAT", group: "build"},
    {key: "demo.algolia_key", set: true, label: "demo.algolia_key",
     hint: "nothing on this daemon reads this key.", group: "unused"},
  ],
});

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/secrets",
  virtualConsole: vc,
  pretendToBeVisual: true,
  beforeParse(win) {
    win.EventSource = class {
      constructor() { this.readyState = 1; }
      addEventListener() {}
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      const body = url === "/api/admin" ? {secrets: true, projects: true} : state();
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
await new Promise(r => setTimeout(r, 250));

const $ = s => doc.querySelector(s);
const $$ = s => [...doc.querySelectorAll(s)];
const settle = () => new Promise(r => setTimeout(r, 80));
const box = group => $(`.secgroup-box[data-group="${group}"]`);
// The keys rendered inside one group, read from the row markup rather than from the
// fixture — the point is what the page put where.
//
// From data-key rather than the visible .key line, which a row omits when it would just
// repeat the label. Asserting against what is displayed would make this test fail for a
// display change while missing the thing it is for.
const keysIn = group =>
  [...box(group).querySelectorAll(".sec[data-key]")].map(n => n.dataset.key);

console.log("the page is the credential panel, not the previews list");
{
  if ($("#setup").hidden) fail("the secrets panel is hidden on /secrets");
  else ok("secrets panel shown");
  if (!$(".wrap").hidden) {
    fail("the previews grid renders under the credential page");
  } else {
    ok("previews and activity are gone");
  }
}

console.log("\nthree groups, each with its own heading");
{
  const titles = $$(".secgroup-box > summary .secgroup").map(n => n.textContent.trim());
  const want = ["Platform and exposer credentials", "Build variables", "Nothing reads these"];
  if (titles.length !== 3) {
    fail(`${titles.length} groups: ${JSON.stringify(titles)}`);
  } else if (titles.join("|") !== want.join("|")) {
    fail(`groups are ${JSON.stringify(titles)}`);
  } else {
    ok("credentials, build variables, unused — in that order");
  }
}

console.log("\nnothing crosses the boundary");
{
  // The load-bearing assertion. A daemon credential rendered among build variables
  // reads as something a build can see, and the reverse reads as something it cannot.
  const daemon = keysIn("daemon");
  const build = keysIn("build");

  const leaked = daemon.filter(k => !k.includes("."));
  if (leaked.length) {
    fail(`shell-shaped keys under credentials: ${JSON.stringify(leaked)}`);
  } else {
    ok(`${daemon.length} credentials, all dotted`);
  }

  const wrong = build.filter(k => k.includes("."));
  if (wrong.length) {
    fail(`dotted daemon keys under build variables: ${JSON.stringify(wrong)}`);
  } else {
    ok(`${build.length} build variables, all UPPER_CASE`);
  }

  if (!daemon.includes("github.private_key")) {
    fail("the GitHub private key is not under credentials");
  }
  if (build.includes("github.private_key")) {
    fail("the GitHub private key renders under Build variables");
  }
}

console.log("\na row does not print its key twice");
{
  // A build variable's label is its key, so rendering both printed
  // GH_ZITI_CI_REPO_ACCESS_PAT on two consecutive lines of every row.
  const row = box("build").querySelector('.sec[data-key="BB_REPO_TOKEN_ONPREM"]');
  if (!row) {
    fail("the build variable row is missing");
  } else if (row.querySelector(".key")) {
    fail("a row whose label is its key still renders the key line");
  } else {
    ok("no second line where it would repeat the label");
  }

  // And a credential keeps it, because the label is prose and the key is what
  // `docpreview vault set` takes.
  const cred = box("daemon").querySelector('.sec[data-key="github.private_key"]');
  const shown = cred && cred.querySelector(".key");
  if (!shown || shown.textContent.trim() !== "github.private_key") {
    fail("a credential no longer shows the vault key you would type");
  } else {
    ok("credentials still show their vault key");
  }
}

console.log("\neverything starts collapsed");
{
  const open = $$(".secgroup-box").filter(d => d.open).map(d => d.dataset.group);
  if (open.length) {
    fail(`${JSON.stringify(open)} rendered expanded`);
  } else {
    ok("three closed sections");
  }
}

console.log("\nwhat is missing is visible without opening anything");
{
  // This is what makes collapsed-by-default safe. A missing credential is the one fact
  // this page exists to show, so it has to survive being behind a closed section.
  const chip = box("daemon").querySelector("summary .flag.off");
  if (!chip || !/1 missing/.test(chip.textContent)) {
    fail(`the collapsed summary does not say what is missing: ${JSON.stringify(
      box("daemon").querySelector("summary").textContent.trim())}`);
  } else {
    ok(`summary reads "${chip.textContent.trim()}" while closed`);
  }
  const n = box("build").querySelector("summary .secgroup-n");
  if (!n || n.textContent.trim() !== "2") {
    fail("a complete group does not show its count");
  } else {
    ok("a complete group shows a plain count");
  }
}

// open expands a section the way a click does, including the toggle event the
// accordion listens for.
const open = async group => {
  const d = box(group);
  d.open = true;
  d.dispatchEvent(new win.Event("toggle"));
  await settle();
};

console.log("\nthe colour stripe belongs to the group, not to its header");
{
  // Read from the stylesheet source, not through getComputedStyle: jsdom does not
  // resolve a `border-left` shorthand whose colour is a var(), so the computed value
  // comes back empty whether the rule is there or not.
  //
  // A source-level check, and it is worth being clear about what it can prove. It
  // catches the regression that prompted it — the stripe moving back onto the summary,
  // where it stops at the header's bottom edge and an expanded section's rows hang off a
  // mark that has already ended. It cannot prove the stripe *renders*; only a browser
  // can do that.
  const css = readFileSync(dashboard, "utf8");
  const ruleFor = sel => {
    const at = css.indexOf("\n" + sel + " {");
    if (at < 0) return null;
    return css.slice(at, css.indexOf("}", at));
  };

  const onBox = ruleFor(".secgroup-box");
  const onSummary = ruleFor(".secgroup-box > summary");
  if (!onBox || !/border-left:\s*3px/.test(onBox)) {
    fail("the group container has no 3px border-left, so the stripe cannot span its rows");
  } else {
    ok("3px on the container, so it runs the full height");
  }
  if (onSummary && /border-left:/.test(onSummary)) {
    fail("the summary draws its own stripe, which is where it used to stop short");
  } else {
    ok("the header does not draw one of its own");
  }

  // A colour each. Three groups sharing one stripe colour is three sections that look
  // like one, which is what the grouping exists to prevent.
  const edges = ["daemon", "build", "unused"].map(g => {
    const m = css.match(new RegExp(`\\.secgroup-box\\[data-group="${g}"\\][^}]*--edge:\\s*([^;}]+)`));
    return m && m[1].trim();
  });
  if (edges.some(e => !e)) {
    fail(`a group has no --edge: ${JSON.stringify(edges)}`);
  } else if (new Set(edges).size !== 3) {
    fail(`two groups share a stripe colour: ${JSON.stringify(edges)}`);
  } else {
    ok(`three distinct colours: ${edges.join(", ")}`);
  }

  // The tint that makes an expanded section read as one block, with its fallback: an
  // engine that cannot parse color-mix drops that declaration alone, so the plain
  // background before it is what keeps the section shaded at all.
  const openRule = ruleFor(".secgroup-box[open]");
  if (!openRule || !/color-mix/.test(openRule)) {
    fail("an expanded section is not tinted");
  } else if (!/background:\s*var\(--raised\)/.test(openRule)) {
    fail("the tint has no plain-colour fallback, so it vanishes where color-mix is unsupported");
  } else {
    ok("expanded sections are tinted, with a fallback");
  }
}

console.log("\nan accordion: opening one closes the others");
{
  await open("daemon");
  if (!box("daemon").open) fail("opening the credentials section did not open it");

  await open("build");
  const stillOpen = $$(".secgroup-box").filter(d => d.open).map(d => d.dataset.group);
  if (stillOpen.length !== 1 || stillOpen[0] !== "build") {
    fail(`after opening Build variables, open sections are ${JSON.stringify(stillOpen)} — ` +
         "two open at once puts a credential and a build variable back on one screen");
  } else {
    ok("only Build variables is open");
  }
}

console.log("\nthe open section survives a re-render");
{
  // The bug this is here for: the panel replaces its own innerHTML on every save, so a
  // <details open> attribute is reset by any action taken inside it — expand a group,
  // save a token inside it, and it collapses under you.
  win.eval(`renderSetup(${JSON.stringify(state())})`);
  await settle();

  const stillOpen = $$(".secgroup-box").filter(d => d.open).map(d => d.dataset.group);
  if (stillOpen.length !== 1 || stillOpen[0] !== "build") {
    fail(`after renderSetup, open sections are ${JSON.stringify(stillOpen)}`);
  } else {
    ok("Build variables is still the open one");
  }
}

console.log("\nclosing the open one leaves everything closed");
{
  const d = box("build");
  d.open = false;
  d.dispatchEvent(new win.Event("toggle"));
  await settle();

  win.eval(`renderSetup(${JSON.stringify(state())})`);
  await settle();

  const stillOpen = $$(".secgroup-box").filter(d => d.open).map(d => d.dataset.group);
  if (stillOpen.length) {
    fail(`${JSON.stringify(stillOpen)} reopened after being closed`);
  } else {
    ok("all closed");
  }
}

console.log("\nadding a value belongs to Build variables");
{
  // The name typed into that form decides which group the result lands in, so the form
  // lives in the section that states the rule — not under its own heading below the card,
  // where it belonged to nothing.
  const form = box("build") && box("build").querySelector("#new-key");
  if (!form) {
    fail("the add-a-value form is not inside the Build variables group");
  } else {
    ok("the form is the last row of Build variables");
  }
  if ($("#setup-body > #new-key") || $("#setup-body > .sec #new-key")) {
    fail("the form also renders outside the card");
  }
}

console.log("\na value box can be expanded for a PEM, and collapsed again");
{
  // The element is replaced rather than a second one rendered hidden beside it, so what
  // this has to prove is that the id survives — every save handler reads the field by id,
  // and an expanded box that saved nothing would be silent.
  const btn = box("build").querySelector('[data-expand="new-val"]');
  if (!btn) {
    fail("no expand control on the new-entry value box");
  } else {
    const before = doc.getElementById("new-val");
    before.value = "-----BEGIN RSA PRIVATE KEY-----";
    if (before.tagName !== "INPUT" || before.type !== "password") {
      fail("the value box does not start as a masked single-line input");
    }

    btn.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
    await settle();

    const after = doc.getElementById("new-val");
    if (!after) {
      fail("expanding lost the field's id, so nothing can read the value back out");
    } else if (after.tagName !== "TEXTAREA") {
      fail(`expanding produced a ${after.tagName}, want TEXTAREA`);
    } else if (after.value !== "-----BEGIN RSA PRIVATE KEY-----") {
      fail("expanding discarded what had been typed");
    } else {
      ok("expands to a textarea, same id, value carried over");
    }

    btn.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
    await settle();

    const back = doc.getElementById("new-val");
    if (back.tagName !== "INPUT" || back.type !== "password") {
      fail("collapsing did not restore a masked input");
    } else if (back.value !== "-----BEGIN RSA PRIVATE KEY-----") {
      fail("collapsing discarded the value");
    } else {
      ok("collapses back to masked, value intact");
    }
  }
}

console.log("\nan empty group renders nothing, unless it holds the form");
{
  const only = state();
  only.entries = only.entries.filter(e => e.group === "daemon");
  win.eval(`renderSetup(${JSON.stringify(only)})`);
  await settle();

  if (box("unused")) {
    fail("an empty group rendered a heading with nothing under it");
  } else {
    ok("'Nothing reads these' is gone when there is nothing in it");
  }
  // Build variables survives being empty, because that is a fresh install and the form
  // is the only way to add the first one. A group that vanished would leave nowhere to
  // start.
  if (!box("build")) {
    fail("Build variables vanished when empty, taking the add form with it");
  } else if (!box("build").querySelector("#new-key")) {
    fail("Build variables rendered empty with no form in it");
  } else {
    ok("Build variables stays, holding the form");
  }
}

console.log(failures ? `\n${failures} failure(s)` : `\nall secrets-page checks OK`);
process.exit(failures ? 1 : 0);
