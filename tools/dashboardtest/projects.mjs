// Loads the real dashboard at /projects in a DOM and drives the page the way an
// operator would: open a project, add an environment variable, remove one, toggle a
// project off, add a project.
//
// # Why this exists
//
// The projects page was reported as unusable, and the causes were not in the page's
// logic — they were in what it rendered and where its errors went. Two of them are
// invisible from reading the JavaScript:
//
//   - `.wrap` sets `display: grid`, which beats `[hidden]`'s `display: none`. So
//     `el.hidden = true` did nothing and the previews and activity sections rendered
//     under both admin pages with nothing in them.
//   - `run()` rendered every result through the *secrets* renderer and prepended its
//     errors to `#setup-body`, which is hidden on this page. A failed project save
//     therefore did nothing and said nothing.
//
// Both are asserted below, because both will come back the moment someone adds a
// third page.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/projects.mjs
//
// No daemon needed: this page has nothing live on it, so the state is a fixture and
// every call the page makes is recorded rather than served.
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

// Two projects: one that states most of its build and holds two of its own
// environment variables, and one that defers entirely to its repository and is
// disabled. The second is the row that used to render as a bare line of text.
const state = () => ({
  can_write: true,
  secrets_available: true,
  vault_locked: false,
  global_secrets: ["SHARED_TOKEN"],
  // docker present, local not enabled: the shipped arrangement, and the one where the
  // form has to disable a choice rather than offer it.
  defaults: {
    driver: "docker", image: "node:24-bookworm-slim",
    docker_available: true, allow_local_driver: false,
    images: ["node:24-bookworm-slim", "node:22-bookworm-slim"],
  },
  projects: [
    {
      platform: "github", owner: "netfoundry", repo: "unified-doc", enabled: true,
      driver: "docker", build_dir: "www", build_command: "npm run build",
      build_output: "build", base_url: "/", detect_script: "", image: "",
      notes: "the big one", display_name: "Unified Doc", avatar: "",
      secrets: ["BB_REPO_TOKEN_ONPREM", "GH_ZITI_CI_REPO_ACCESS_PAT"],
    },
    {
      platform: "bitbucket", owner: "netfoundry", repo: "customer-connect-docs",
      enabled: false, driver: "", build_dir: "", build_command: "", build_output: "",
      base_url: "", detect_script: "", image: "", notes: "", secrets: [],
    },
  ],
});

// calls records everything the page asks the daemon to do, which is what the
// assertions are really about: a button that renders correctly and sends the wrong
// request is the failure mode a screenshot cannot see.
const calls = [];
let nextFails = null;

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/projects",
  virtualConsole: vc,
  pretendToBeVisual: true,
  beforeParse(win) {
    win.fetch = async (url, init) => {
      const method = init?.method || "GET";
      calls.push({method, url, body: init?.body ? JSON.parse(init.body) : null});
      if (nextFails) {
        const msg = nextFails;
        nextFails = null;
        return {ok: false, status: 409, statusText: "Conflict",
          text: async () => JSON.stringify({error: msg}), json: async () => ({error: msg})};
      }
      const body = url === "/api/admin" ? {secrets: true, projects: true} : state();
      return {ok: true, status: 200, statusText: "OK",
        json: async () => body, text: async () => JSON.stringify(body)};
    };
    win.matchMedia = () => ({matches: false, addEventListener() {}});
    win.HTMLElement.prototype.scrollIntoView = function () {};
    win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
    win.requestAnimationFrame = fn => setTimeout(fn, 0);
    win.getSelection = () => ({isCollapsed: true});
    // jsdom implements neither, and the page uses both. Answering yes to confirm is
    // the interesting path: it is the one that sends the request.
    win.confirm = () => true;
    win.alert = msg => { calls.push({method: "ALERT", url: msg}); };
  },
});

const win = dom.window;
const doc = win.document;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 200));

const $ = sel => doc.querySelector(sel);
const $$ = sel => [...doc.querySelectorAll(sel)];
const settle = () => new Promise(r => setTimeout(r, 120));
const click = async el => {
  el.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
};
const btn = text => $$("#projects-body .btn, #projects-body button")
  .find(b => b.textContent.trim().startsWith(text));

console.log("operations chrome");
{
  // The bug that made half the page noise. `hidden` is set by the page; whether it
  // takes effect is a question about the stylesheet, so it is asked of the computed
  // style rather than of the property.
  for (const sel of [".wrap", "#counters", "#project", "#search"]) {
    const el = $(sel);
    if (!el.hidden) {
      fail(`${sel} is not marked hidden on /projects`);
      continue;
    }
    if (win.getComputedStyle(el).display !== "none") {
      fail(`${sel} is marked hidden but still renders — an author display rule ` +
        `beats [hidden] without the !important guard`);
    }
  }
  if (!failures) ok("previews and activity are gone, not merely marked hidden");
  if ($("#projects").hidden) fail("the projects panel is hidden on its own page");
}

console.log("\nno class collides with the operations dashboard");
{
  // One document serves three pages, so a class named for one of them styles all
  // three. `.proj` was the project name inside a previews row before it was a card on
  // this page, and claiming it drew a bordered, padded box around the project name on
  // the dashboard — a page nobody looks at while editing the projects CSS. The second
  // time this stylesheet was bitten that way; `.row .go` carries the first note.
  //
  // Checked by rendering the *previews* markup this page's rules could reach, and
  // asking whether any of them decorated it.
  const probe = win.document.createElement("div");
  probe.className = "list";
  probe.innerHTML = `<div class="item"><div class="row"><button class="head">
    <span class="who"><span class="proj">docpreview</span>
    <span class="branch">round-4</span></span></button></div></div>`;
  win.document.body.append(probe);

  const name = probe.querySelector(".proj");
  const cs = win.getComputedStyle(name);
  for (const [prop, bad] of [["borderTopWidth", "0px"], ["paddingLeft", "0px"]]) {
    const got = cs[prop];
    if (got && got !== bad && got !== "") {
      fail(`a previews-row .proj has ${prop} = ${got}; a projects-page rule is ` +
        `styling the operations dashboard`);
    }
  }
  probe.remove();
  ok("previews-row classes are untouched by this page's CSS");
}

console.log("\nproject cards");
{
  const cards = $$(".pcard");
  if (cards.length !== 2) fail(`${cards.length} cards, want 2`);

  const first = cards[0];
  const pairs = [...first.querySelectorAll(".facts dt")].map(dt =>
    [dt.textContent, dt.nextElementSibling?.textContent]);
  const asMap = Object.fromEntries(pairs);
  if (asMap.command !== "npm run build") {
    fail(`the command renders as ${JSON.stringify(asMap.command)}`);
  }
  if (asMap.output !== "build") fail(`the output renders as ${JSON.stringify(asMap.output)}`);
  // Each fact is its own element, which is what stops a value being split from its
  // label when the column narrows — the defect in the original report.
  if (pairs.some(([, v]) => !v)) fail("a fact has a label with no value beside it");
  // A field the project does not state must be absent rather than rendered as the
  // word "default": the card is for what this project decides.
  if ("ignore script" in asMap) fail("an unset field is rendered anyway");
  if (!asMap.env) fail("the card does not say the project has environment variables");
  ok(`facts render as ${pairs.length} label/value pairs`);

  // The second project states nothing at all, and must say so in words rather than
  // rendering an empty card.
  if (!cards[1].textContent.includes("entirely from the repository")) {
    fail("a project that defers everything renders no explanation");
  }
  if (!cards[1].classList.contains("pcard-off")) fail("a disabled project is not marked");
  ok("a deferring, disabled project explains itself");
}

console.log("\nidentity: badge, name, platform label");
{
  const cards = $$(".pcard");
  // A monogram derived from the name, so ten projects are distinguishable with nothing
  // configured. "Unified Doc" -> UD.
  const badge = cards[0].querySelector(".ava");
  if (!badge) fail("no badge on a project card");
  else if (badge.textContent.trim() !== "UD") {
    fail(`badge reads ${JSON.stringify(badge.textContent)}, want the initials UD`);
  } else ok("a name with two words gives initials");

  // One word gives its first two characters: customer-connect-docs -> CU is wrong,
  // CD is right, because the hyphens are word breaks.
  const second = cards[1].querySelector(".ava");
  if (second && second.textContent.trim() !== "CC") {
    fail(`hyphenated name gave ${JSON.stringify(second.textContent)}, want CC`);
  } else ok("a hyphenated name is treated as words");

  // The badge must not be an <img>: a remote avatar would announce every project on
  // the page to whoever hosts the image.
  if (cards[0].querySelector("img")) fail("the badge fetches a remote image");

  // A display name is shown, with the real identity still visible beside it, because
  // owner/repo is what a webhook is matched against.
  const head = cards[0].querySelector(".pcard-head").textContent;
  if (!head.includes("Unified Doc")) fail("the display name is not shown");
  if (!head.includes("netfoundry/unified-doc")) {
    fail("the real owner/repo is hidden behind the display name");
  }
  // `local` is the git simulator. The stored value stays `local`; what is read says
  // what it is.
  if (!$$(".pcard-head .flag").some(f => f.textContent.trim() === "bitbucket")) {
    fail("the platform chip is missing");
  }
  ok("display name shown with owner/repo beside it");
}

console.log("\nthe add form is closed until asked for");
{
  if ($("#p-owner")) fail("the add form is open before anything asked for it");
  const newBtn = $("#p-new");
  if (!newBtn) {
    fail("there is no New project button");
  } else {
    await click(newBtn);
    if (!$("#p-owner")) fail("New project did not open the form");
    else ok("New project opens the form");
    // And closing it puts the page back, rather than leaving eleven inputs open.
    const cancel = btn("Cancel");
    if (cancel) await click(cancel);
    if ($("#p-owner")) fail("Cancel left the form open");
    else ok("Cancel closes it");
  }
}

console.log("\nthe new-project form");
{
  await click($("#p-new"));

  // Columnar: every field is its own row of label + control, not seven across.
  const rows = $$(".grid-form .field");
  if (rows.length < 10) fail(`${rows.length} field rows, want one per field`);
  const stacked = rows.every(r => r.querySelector("label") && r.children.length >= 2);
  if (!stacked) fail("a field row has no label beside its control");
  else ok(`${rows.length} fields, one per row`);

  // Paste a URL, get the three identity fields. Each form is what somebody actually
  // has on a clipboard.
  const cases = [
    ["https://github.com/acme/docs", "github", "acme", "docs"],
    ["https://github.com/acme/docs/pull/4/files", "github", "acme", "docs"],
    ["git@bitbucket.org:netfoundry/customer-connect-docs.git", "bitbucket", "netfoundry",
     "customer-connect-docs"],
    ["https://bitbucket.org/netfoundry/platform-doc/src/main/docs", "bitbucket",
     "netfoundry", "platform-doc"],
  ];
  for (const [url, platform, owner, repo] of cases) {
    const el = $("#p-url");
    el.value = url;
    el.dispatchEvent(new win.Event("input", {bubbles: true}));
    await settle();
    const got = [$("#p-platform").value, $("#p-owner").value, $("#p-repo").value];
    if (got.join("/") !== [platform, owner, repo].join("/")) {
      fail(`${url} -> ${got.join("/")}, want ${platform}/${owner}/${repo}`);
    }
  }
  ok(`${cases.length} URL forms parsed into platform/owner/repo`);

  // An unrecognised host must leave the fields alone rather than clearing them: a
  // half-typed value would otherwise wipe what was already correct.
  $("#p-owner").value = "acme";
  const el = $("#p-url");
  el.value = "https://git.example.com/acme/docs";
  el.dispatchEvent(new win.Event("input", {bubbles: true}));
  await settle();
  if ($("#p-owner").value !== "acme") fail("an unknown host cleared the owner field");

  // The driver select must not offer what the daemon will refuse. docker is available
  // and local is not enabled in this fixture.
  const opts = [...$("#p-driver").options];
  const local = opts.find(o => o.value === "local");
  const docker = opts.find(o => o.value === "docker");
  if (!local?.disabled) fail("local is offered although this daemon has not enabled it");
  if (!local.textContent.includes("not enabled")) {
    fail(`the local option does not say why: ${JSON.stringify(local.textContent)}`);
  }
  if (docker?.disabled) fail("docker is disabled although the probe found it");
  ok("the driver select disables what would be refused");

  // The image field suggests without constraining.
  const image = $("#p-image");
  if (image.getAttribute("list") !== "known-images") fail("the image field offers no suggestions");
  if ($$("#known-images option").length < 2) fail("the suggestion list is empty");
  if (image.tagName !== "INPUT") fail("the image field is a closed list, not pick-or-type");
  ok("image is pick-or-type");

  // Notes is a textarea with a cap, not a one-line input.
  const notes = $("#p-notes");
  if (notes.tagName !== "TEXTAREA") fail("notes is still a single-line input");
  else if (notes.getAttribute("maxlength") !== "5000") {
    fail(`notes maxlength = ${notes.getAttribute("maxlength")}, want 5000`);
  } else ok("notes is a capped textarea");

  await click(btn("Cancel"));
}

console.log("\nsecrets panel");
{
  const secretsBtn = $$(".pcard [data-tab=secrets]")[0];
  if (!secretsBtn) {
    fail("no Secrets control on a project card");
  } else {
    if (!secretsBtn.textContent.includes("(2)")) {
      fail(`the Secrets control does not count them: ${secretsBtn.textContent.trim()}`);
    }
    await click(secretsBtn);

    const own = $$(".pcard .env:not(.inherited)").map(e => e.textContent.replace("✕", "").trim());
    if (own.length !== 2 || !own.includes("BB_REPO_TOKEN_ONPREM")) {
      fail(`the panel lists ${JSON.stringify(own)}`);
    }
    // Inherited names are shown greyed rather than omitted: "no variables" and "none
    // of its own" look identical otherwise, and only one means a build will fail.
    const inherited = $$(".pcard .env.inherited").map(e => e.textContent.trim());
    if (!inherited.includes("SHARED_TOKEN")) {
      fail(`the server-wide variables are not shown as inherited: ${JSON.stringify(inherited)}`);
    }
    ok(`2 own variables, ${inherited.length} inherited`);

    // No value may appear anywhere in the panel, because nothing can read one back.
    if ($$(".pcard .env input").length) fail("a value input sits inside the variable list");

    // The value being typed is masked, and the name field carries no placeholder — a
    // greyed-out example in the name box reads as a value already entered.
    const val = doc.getElementById("s-val-github/netfoundry/unified-doc");
    if (val?.type !== "password") fail(`the value field is type=${val?.type}, want password`);
    const env = doc.getElementById("s-env-github/netfoundry/unified-doc");
    if (env?.getAttribute("placeholder")) {
      fail(`the name field has placeholder ${JSON.stringify(env.getAttribute("placeholder"))}`);
    }
    ok("the value is masked and the name box is empty");
  }
}

console.log("\nunsaved edits are not thrown away silently");
{
  // Open a project's settings and type into it.
  await click($$(".pcard [data-tab=build]")[0]);
  const dir = doc.getElementById("p-dir-github/netfoundry/unified-doc");
  dir.value = "somewhere-else";
  dir.dispatchEvent(new win.Event("input", {bubbles: true}));
  await settle();

  // Refuse the confirm: the panel stays open with the typing in it.
  win.confirm = () => false;
  await click(btn("Cancel"));
  if (!$(".pcard .panel")) fail("declining the discard prompt closed the form anyway");
  else if (doc.getElementById("p-dir-github/netfoundry/unified-doc").value !== "somewhere-else") {
    fail("declining the prompt lost the typing");
  } else ok("declining keeps the form and its edits");

  // Accept it: the panel closes.
  win.confirm = () => true;
  await click(btn("Cancel"));
  if ($(".pcard .panel")) fail("accepting the discard prompt left the form open");
  else ok("accepting discards and closes");
}

console.log("\nadding a project queues its open pull requests");
{
  calls.length = 0;
  await click($("#p-new"));
  doc.getElementById("p-url").value = "https://github.com/acme/newdocs";
  doc.getElementById("p-url").dispatchEvent(new win.Event("input", {bubbles: true}));
  await settle();
  await click(btn("Create project"));

  const put = calls.find(c => c.method === "PUT");
  const scan = calls.find(c => c.method === "POST" && c.url.endsWith("/scan"));
  if (!put) fail("Create sent no PUT");
  if (!scan) {
    fail("Create did not scan for open pull requests, so nothing gets built");
  } else if (scan.url !== "/api/projects/github/acme/newdocs/scan") {
    fail(`scanned ${scan.url}`);
  } else ok(`POST ${scan.url}`);

  // And the order matters: the project has to exist before anything is queued against it.
  if (put && scan && calls.indexOf(put) > calls.indexOf(scan)) {
    fail("the scan was sent before the project was saved");
  }
}

console.log("\nadding a variable");
{
  // Reopen the panel: the sections above left the page elsewhere, and a harness that
  // depends on the previous section's leftover state breaks the moment one is inserted.
  if (!$(".pcard .env")) await click($$(".pcard [data-tab=secrets]")[0]);
  const key = "github/netfoundry/unified-doc";
  doc.getElementById(`s-env-${key}`).value = "BB_REPO_TOKEN_FRONTDOOR";
  doc.getElementById(`s-val-${key}`).value = "a-token-value";
  calls.length = 0;
  await click(btn("Add variable"));

  const put = calls.find(c => c.method === "PUT");
  if (!put) {
    fail("Add variable sent no request");
  } else {
    const want = "/api/projects/github/netfoundry/unified-doc/secrets/BB_REPO_TOKEN_FRONTDOOR";
    if (put.url !== want) fail(`PUT ${put.url}, want ${want}`);
    else if (put.body?.value !== "a-token-value") fail("the value did not go with it");
    else ok(`PUT ${put.url}`);
  }
  // The panel stays open across the refresh: the operator is usually adding several,
  // and a panel that closed after each would be a click per token.
  if (!$(".pcard .panel")) fail("the panel closed after adding a variable");
}

console.log("\nremoving a variable");
{
  calls.length = 0;
  await click($(".pcard .env button[data-del-secret]"));
  const del = calls.find(c => c.method === "DELETE");
  if (!del) fail("Remove sent no request");
  else if (!del.url.endsWith("/secrets/BB_REPO_TOKEN_ONPREM")) fail(`DELETE ${del.url}`);
  else ok(`DELETE ${del.url}`);
}

console.log("\ndisabling a project");
{
  await click($$(".pcard [data-tab=build]")[0]);
  calls.length = 0;
  const toggle = btn("Disable");
  if (!toggle) {
    fail("no Disable control in the build panel");
  } else {
    await click(toggle);
    const put = calls.find(c => c.method === "PUT");
    if (!put) fail("Disable sent no request");
    else if (put.body.enabled !== false) fail(`enabled = ${put.body.enabled}, want false`);
    // PUT is a whole-row upsert, so every other field has to go with it or disabling a
    // project would quietly erase its build command.
    else if (put.body.build_command !== "npm run build") {
      fail("disabling a project dropped its build command");
    } else ok("Disable preserves the rest of the row");
  }
}

console.log("\na failure is reported where it can be seen");
{
  nextFails = "the vault is locked; unlock it at /secrets first";
  await click($$(".pcard [data-tab=secrets]")[0]);
  const key = "github/netfoundry/unified-doc";
  doc.getElementById(`s-env-${key}`).value = "BB_REPO_TOKEN_ONPREM";
  doc.getElementById(`s-val-${key}`).value = "a-token-value";
  await click(btn("Add variable"));

  const notice = $("#projects-body .notice.bad");
  if (!notice) {
    fail("a failed call reported nothing on the page it happened on");
  } else if (!notice.textContent.includes("vault is locked")) {
    fail(`the notice says ${JSON.stringify(notice.textContent)}`);
  } else {
    ok("the error lands in #projects-body");
  }
  if ($("#setup-body .notice.bad")) {
    fail("the error was also written into the hidden secrets panel");
  }
}

console.log(failures ? `\n${failures} failure(s)` : `\nall projects-page checks OK`);
process.exit(failures ? 1 : 0);
