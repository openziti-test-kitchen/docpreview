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
    // The shape the daemon actually sends (knownImages), including the two entries the
    // rows annotate: a -slim one with no git, and an alpine one whose musl breaks
    // dependencies shipping prebuilt binaries.
    images: ["node:24-bookworm-slim", "node:24-bookworm", "node:24-alpine"],
    // The preset table, as the server sends it. Three entries are enough: the default,
    // one Node preset, and one that needs a tool a node image does not have.
    frameworks: [
      {id: "", label: "None — the repository decides"},
      {id: "docusaurus", label: "Docusaurus (v2+)",
       build_command: "npm run build", output: "build"},
      {id: "mkdocs", label: "MkDocs",
       build_command: "mkdocs build", output: "site", needs_tool: "mkdocs"},
    ],
    // What a new project's form starts on. Docusaurus, because that is what this daemon
    // previews in practice — a form defaulting to "the repository decides" makes the
    // commonest case two clicks and the rarest zero.
    framework: "docusaurus",
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
      // Its own access token stored, no API token — the recommended shape, and the one
      // that makes the "set"/"missing" chips distinguishable in the form.
      scm: ["scm.access_token"],
      // Private, because the credential state is only reported for a repository that needs
      // one: a public repo clones with no token, so a red "missing" beside one would be the
      // page inventing a problem.
      private: true,
      // One pull request unlinked, which is what the card has to display: an ignore that
      // nothing shows is indistinguishable from a build system that stopped noticing a
      // pull request.
      ignored: [{number: 20, branch: "feature/pricing", created_at: "2026-07-30T18:26:23Z"}],
      // A branch preview that exists and works. `master` rather than `main`, because the
      // name comes from the platform and a page that hardcoded one would pass with `main`.
      branch: {
        name: "master", preview_id: "b400e0aa1234", state: "ready",
        url: "https://customer-connect-docs-master.shares.zrok.io/",
        commit: "cf9f37d25cf7515f8c7e531afbe97cc6ee4238f3",
        updated_at: "2026-07-30T18:26:23Z",
      },
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
// Expanding a <details> the way a click on its summary does. jsdom does not implement the
// summary click that flips `open`, so the property is set and the event the page listens
// for is dispatched by hand.
const openSecrets = async d => {
  d.open = true;
  d.dispatchEvent(new win.Event("toggle"));
  await settle();
};
// Typing, not assignment: the page listens for input, and a bare `.value =` exercises
// none of it — the kind of stub that makes a harness agree with itself.
const type = async (el, v) => {
  el.value = v;
  el.dispatchEvent(new win.Event("input", {bubbles: true}));
  await settle();
};
const btn = text => $$("#projects-body .btn, #projects-body button")
  .find(b => b.textContent.trim().startsWith(text));

console.log("operations chrome");
{
  // The bug that made half the page noise. `hidden` is set by the page; whether it
  // takes effect is a question about the stylesheet, so it is asked of the computed
  // style rather than of the property.
  for (const sel of [".wrap", "#counters", "#projpick", "#search"]) {
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

console.log("\nadding is reachable without scrolling past every project");
{
  // Both the button and the form it opens used to be below the list. Fine for three
  // projects and wrong for thirty: adding one meant scrolling past every project that
  // already existed, and the form then appeared where the button had been — the bottom of
  // a long page — so the fields to fill in were somewhere the eye had never been.
  const body = $("#projects-body");
  const btnEl = $("#p-new");
  const firstCard = $(".pcard");
  if (!btnEl) {
    fail("no New project button");
  } else if (!firstCard) {
    fail("no project cards to compare against");
  } else {
    // compareDocumentPosition: 4 means the argument follows the reference node.
    const buttonComesFirst = !!(btnEl.compareDocumentPosition(firstCard) &
      body.ownerDocument.defaultView.Node.DOCUMENT_POSITION_FOLLOWING);
    if (!buttonComesFirst) {
      fail("New project sits after the project list");
    } else {
      ok("New project is above the list");
    }
  }
}

console.log("\nthe add form is a dialog, not the bottom of the page");
{
  await click($("#p-new"));
  const modal = $("#p-new-modal");
  if (!modal) {
    fail("the new-project form is not in a dialog");
  } else if (!modal.querySelector("#p-url")) {
    fail("the dialog does not contain the form");
  } else if (modal.querySelector(".modal-card") === null) {
    fail("the dialog has no card");
  } else {
    ok("opens in a modal dialog");
  }
  // Wide, because a build command is a shell line and a container image is a registry
  // path: the report dialog's 34rem turns both into boxes you scroll sideways to read.
  if (modal && !modal.querySelector(".modal-card.wide")) {
    fail("the project dialog uses the narrow card");
  }
  await click(btn("Cancel"));
  if ($("#p-new-modal")) fail("Cancel left the dialog open");
  else ok("Cancel closes it");
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
  //
  // Everything except the variables accordions, whose Name and Value fields use the same
  // markup. Unscoped, this counted two per project card, so the number grew with the
  // fixture rather than with the form being tested — and scoping to `.panel` misses the
  // new-project form, which is not inside one.
  const rows = $$(".grid-form .field").filter(f => !f.closest("[data-secrets]"));
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

  // The image field is a search box over the known images, and still free text — a
  // private registry mirror is the normal answer in an enterprise and must not be
  // refused by a closed list.
  const image = $("#p-image");
  if (image.tagName !== "INPUT") fail("the image field is a closed list, not pick-or-type");
  const panel = $("#p-image-panel");
  if (!panel) {
    fail("the image field has no suggestion panel");
  } else {
    if (!panel.hidden) fail("the image panel is open before the field was touched");
    image.dispatchEvent(new win.Event("focus", {bubbles: true}));
    await settle();
    if (panel.hidden) fail("focusing the image field did not open the list");
    const all = $$("#p-image-panel [data-image]").filter(r => !r.hidden);
    if (all.length < 2) fail(`${all.length} images offered`);

    // The field filters its own list, which is the point of it being one field.
    await type(image, "alpine");
    const shown = $$("#p-image-panel [data-image]").filter(r => !r.hidden);
    if (!shown.length || shown.some(r => !r.dataset.image.includes("alpine"))) {
      fail(`filtering by "alpine" gave ${JSON.stringify(shown.map(r => r.dataset.image))}`);
    }
    // Each row says the one thing worth knowing about that image.
    if (!shown[0].textContent.includes("musl")) fail("an alpine row does not mention musl");

    // A value that matches nothing is legal, and the panel gets out of the way.
    await type(image, "registry.internal/ours:1");
    if (!$("#p-image-panel").hidden) fail("the panel stayed open over a custom value");

    // Clicking a row sets the field.
    image.dispatchEvent(new win.Event("focus", {bubbles: true}));
    await type(image, "bookworm");
    const row = $$("#p-image-panel [data-image]").find(r => !r.hidden);
    await click(row);
    if (image.value !== row.dataset.image) {
      fail(`clicking a row set ${JSON.stringify(image.value)}`);
    } else ok(`pick-or-type: filtered, clicked, field reads ${image.value}`);
  }

  // Notes is a textarea with a cap, not a one-line input.
  const notes = $("#p-notes");
  if (notes.tagName !== "TEXTAREA") fail("notes is still a single-line input");
  else if (notes.getAttribute("maxlength") !== "5000") {
    fail(`notes maxlength = ${notes.getAttribute("maxlength")}, want 5000`);
  } else ok("notes is a capped textarea");

  await click(btn("Cancel"));
}

console.log("\na framework preset fills the fields it knows");
{
  await click($("#p-new"));
  const sel = $("[data-framework]");
  if (!sel) {
    fail("no framework preset dropdown");
  } else {
    // A new project starts on the server's default preset, not on "none": this daemon
    // previews Docusaurus sites, so defaulting to "the repository decides" makes the
    // commonest case two clicks and the rarest zero. An *existing* project still shows
    // what it stored, blank included — asserted below.
    if (sel.value !== "docusaurus") {
      fail(`a new project starts on ${JSON.stringify(sel.value)}, want the default preset`);
    } else {
      ok("a new project starts on Docusaurus");
    }

    // Placeholders, never values. Typing is what overriding means, so a preset that
    // prefilled the box would be indistinguishable from a value somebody set — and would
    // then be stored as an explicit one that stops tracking the preset.
    const cmd = $('[data-preset="build_command"]');
    const out = $('[data-preset="output"]');
    sel.value = "docusaurus";
    sel.dispatchEvent(new win.Event("change", {bubbles: true}));
    await settle();

    if (cmd.placeholder !== "npm run build" || out.placeholder !== "build") {
      fail(`placeholders are ${JSON.stringify([cmd.placeholder, out.placeholder])}`);
    } else if (cmd.value !== "" || out.value !== "") {
      fail("the preset filled the boxes instead of their placeholders");
    } else {
      ok(`placeholders follow the preset: ${JSON.stringify(cmd.placeholder)}, ${
        JSON.stringify(out.placeholder)}`);
    }

    // A preset that needs a tool the node images do not have says so here, rather than
    // twenty seconds into a build that reports "mkdocs: not found".
    sel.value = "mkdocs";
    sel.dispatchEvent(new win.Event("change", {bubbles: true}));
    await settle();
    const warn = $("#framework-tool");
    if (!warn || warn.hidden) {
      fail("MkDocs does not warn that the image needs mkdocs");
    } else if (!/mkdocs/.test(warn.textContent)) {
      fail(`the warning says ${JSON.stringify(warn.textContent)}`);
    } else {
      ok("warns when the preset needs a tool the image lacks");
    }
    if (cmd.placeholder !== "mkdocs build") {
      fail(`switching presets left the placeholder at ${JSON.stringify(cmd.placeholder)}`);
    }

    // And back to none, which is how a project defers to the repository entirely.
    sel.value = "";
    sel.dispatchEvent(new win.Event("change", {bubbles: true}));
    await settle();
    if (!$("#framework-tool").hidden) fail("the tool warning outlived the preset");
    else ok("no preset, no warning");
  }
  await click(btn("Cancel"));
}

console.log("\nthe new-project form asks about the credential without collecting it");
{
  // A token pasted into a form for a project that does not exist yet has nowhere to be
  // stored until the row is written, which made the save two requests where the second
  // could fail on its own — and did, reporting "failed to fetch" about a project that had
  // been created.
  await click($("#p-new"));
  await type($("#p-url"), "https://bitbucket.org/netfoundry/customer-connect-docs");
  const box = $("#scm-fields");
  if (!box || box.hidden) {
    fail("no credential question on a new Bitbucket project");
  } else {
    if (!box.querySelector("[data-privtoggle]")) {
      fail("the new form does not ask whether the repository is private");
    } else {
      ok("asks whether it is private");
    }
    if (box.querySelector("[data-scmrow]") || box.querySelector("input[type=password]")) {
      fail("the new form collects a credential it has nowhere to store yet");
    } else {
      ok("collects no token before the project exists");
    }
    if (!/Create the project first/.test(box.textContent)) {
      fail("the form does not say when to paste the token");
    } else {
      ok("says to paste it after creating");
    }
  }
  await click($("#p-new-modal .modal-x"));
}

console.log("\na private project with no credential says so on its card");
{
  // On the card, not only in a toast: the toast is gone in five seconds and this state can
  // last days — a private repository with no token builds nothing, and the only other
  // symptom is a failed clone in a log nobody opened.
  const wanting = state();
  wanting.projects[1].scm = [];
  wanting.projects[1].private = true;
  win.eval(`projOpen = {key: null, tab: null}`);
  win.eval(`renderProjectsPage(${JSON.stringify(wanting)})`);
  await settle();

  const notice = $(".pcard-wants");
  if (!notice) {
    fail("a private project with no credential says nothing on its card");
  } else if (!/access token/i.test(notice.textContent)) {
    fail(`the notice says ${JSON.stringify(notice.textContent.trim())}`);
  } else {
    ok("the card names what it needs");
  }

  // And the three working states say nothing: token stored, inherits a workspace-wide
  // one, or public. A warning on a working state is one nobody reads twice.
  const ok3 = state();
  ok3.projects[1].private = true;               // its own token is in the fixture
  win.eval(`renderProjectsPage(${JSON.stringify(ok3)})`);
  await settle();
  if ($(".pcard-wants")) fail("a project with its own token is still warned about");

  const inheriting = state();
  inheriting.projects[1].scm = [];
  inheriting.projects[1].private = true;
  inheriting.defaults.scm_global = ["bitbucket.access_token"];
  win.eval(`renderProjectsPage(${JSON.stringify(inheriting)})`);
  await settle();
  if ($(".pcard-wants")) fail("a project inheriting a workspace token is warned about");

  const publicRepo = state();
  publicRepo.projects[1].scm = [];
  publicRepo.projects[1].private = false;
  win.eval(`renderProjectsPage(${JSON.stringify(publicRepo)})`);
  await settle();
  if ($(".pcard-wants")) fail("a public repository is warned about");
  else ok("silent on every working state");

  win.eval(`renderProjectsPage(${JSON.stringify(state())})`);
  await settle();
}

console.log("\nan existing project keeps the preset it stored");
{
  // The default applies to a *new* form only. Applying it to a stored blank would change
  // what every project written before presets existed builds.
  const bb = $$(".pcard [data-tab=build]").find(
    b => b.dataset.key === "bitbucket/netfoundry/customer-connect-docs");
  await click(bb);
  const sel = $("[data-framework]");
  if (!sel) {
    fail("no preset control on an existing project");
  } else if (sel.value !== "") {
    fail(`a project with no stored preset shows ${JSON.stringify(sel.value)}`);
  } else {
    ok("blank stays blank — the repository decides");
  }
  await click(btn("Cancel — discard edits"));
}

console.log("\nthe dialog has a close control that does not scroll away");
{
  await click($("#p-new"));
  const x = $("#p-new-modal .modal-x");
  if (!x) {
    fail("no close control in the dialog");
  } else {
    // Sticky, so nineteen fields do not have to be scrolled past to find the way out.
    const pos = win.getComputedStyle(x.parentElement).position;
    if (pos !== "sticky") {
      fail(`the close control's row is position:${pos}, so it scrolls away with the form`);
    } else {
      ok("pinned to the corner");
    }
    await click(x);
    if ($("#p-new-modal")) fail("the close control did not close the dialog");
    else ok("closes the dialog");
  }
}

console.log("\none credential field, because that is all an access token needs");
{
  // This offered a choice between an access token and an account email plus API token,
  // which put an email field in front of every operator. An access token needs none: the
  // clone username is the literal x-token-auth and the API call is a bearer header. The
  // account-token mode is a server-wide setting, not a per-project question.
  const bb = $$(".pcard [data-tab=build]").find(
    b => b.dataset.key === "bitbucket/netfoundry/customer-connect-docs");
  await click(bb);

  const box = $("#scm-fields");
  const rows = [...box.querySelectorAll("[data-scmrow]")].map(r => r.dataset.scmrow);
  if (rows.length !== 1 || rows[0] !== "scm.access_token") {
    fail(`credential rows are ${JSON.stringify(rows)}, want only the access token`);
  } else {
    ok("one row: the access token");
  }
  if (box.querySelector("[data-scmmode]")) {
    fail("the credential type picker is back, which is a question with one answer");
  }
  // No email *field*. The word still appears, in the sentence explaining that an access
  // token needs none — asserting on the word flagged the explanation.
  if (box.querySelector('[id$="scmemail"]') || /Account email/.test(box.textContent)) {
    fail("the form still collects an email, which an access token does not use");
  } else {
    ok("no email field");
  }
  // And it says how the token is used, since that is what makes the absence of an email
  // obvious rather than suspicious.
  if (!/x-token-auth/.test(box.textContent)) {
    fail("the form does not say how the token is used");
  } else {
    ok("says x-token-auth for clone, bearer for the API");
  }
  await click(btn("Cancel — discard edits"));
}

console.log("\nsecrets panel");
{
  // An accordion in the card, not a `Secrets` button beside `Edit`. As a button it made
  // a project's tokens a mode the card switched into, mutually exclusive with the form —
  // so checking a variable meant leaving whatever was being edited.
  const sec = $$(".pcard [data-secrets]")[0];
  if (!sec) {
    fail("no environment-variables section on a project card");
  } else {
    if (sec.open) fail("the variables section starts expanded");
    const summary = sec.querySelector("summary").textContent;
    // The collapsed summary has to distinguish "none of its own" from "none at all":
    // only one of those means a build is about to fail for a missing token.
    if (!/2 of its own/.test(summary) || !/1 inherited/.test(summary)) {
      fail(`the summary does not count them: ${JSON.stringify(summary.trim())}`);
    }
    await openSecrets(sec);

    // One row per variable, in the credential page's shape. It was a strip of chips with
    // an ✕, which meant the only thing you could do to an existing variable was delete
    // it — replacing a rotated token was delete-then-retype-the-name.
    const rows = $$(".pcard [data-secret]");
    const own = rows.filter(r => !r.dataset.inherited).map(r => r.dataset.secret);
    if (own.length !== 2 || !own.includes("BB_REPO_TOKEN_ONPREM")) {
      fail(`the panel lists ${JSON.stringify(own)}`);
    }
    // Inherited names are listed rather than omitted: "no variables" and "none of its
    // own" look identical otherwise, and only one means a build will fail.
    const inherited = rows.filter(r => r.dataset.inherited).map(r => r.dataset.secret);
    if (!inherited.includes("SHARED_TOKEN")) {
      fail(`the server-wide variables are not listed as inherited: ${JSON.stringify(inherited)}`);
    }
    ok(`2 own variables, ${inherited.length} inherited`);

    // Every row can replace its value in place, and only a project's own can be deleted:
    // there is nothing project-scoped to delete for an inherited name.
    const ownRow = rows.find(r => r.dataset.secret === "BB_REPO_TOKEN_ONPREM");
    if (!ownRow.querySelector("[data-set-secret]") || !ownRow.querySelector("[data-del-secret]")) {
      fail("a project's own variable has no Save and Delete");
    } else {
      ok("replace and delete on each row");
    }
    const inhRow = rows.find(r => r.dataset.inherited);
    if (inhRow.querySelector("[data-del-secret]")) {
      fail("an inherited variable offers Delete, which would delete nothing");
    }
    if (!inhRow.querySelector("[data-set-secret]")) {
      fail("an inherited variable cannot be overridden, which is the point of listing it");
    }

    // No value is ever rendered: nothing can read one back, so every field starts empty
    // and masked. A populated box would be a lie about what is stored.
    const filled = rows.flatMap(r => [...r.querySelectorAll("input")])
      .filter(i => i.type !== "password" || i.value !== "");
    if (filled.length) {
      fail(`${filled.length} variable field(s) are unmasked or prefilled`);
    } else {
      ok("every field is masked and empty");
    }

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

console.log("\nthe Bitbucket credential appears only where it applies");
{
  // A GitHub App is installed on repositories, so the installation is the grant and there
  // is nothing per project to paste. A Bitbucket access token is scoped to one repository
  // at creation — unless a workspace admin permits wider ones, which many do not — so the
  // credential has to live beside the project row.
  await click($("#p-new"));
  const box = $("#scm-fields");
  if (!box) {
    fail("the new-project form has no credential block at all");
  } else if (!box.hidden) {
    fail("the credential block is shown before the platform is known");
  } else {
    ok("hidden until the platform says Bitbucket");
  }

  // Pasting a Bitbucket URL fills the form from script, which fires no change event — so
  // the URL handler has to reveal the block itself.
  await type($("#p-url"), "https://bitbucket.org/netfoundry/customer-connect-docs");
  if ($("#scm-fields").hidden) {
    fail("a pasted Bitbucket URL left the credential block hidden");
  } else {
    ok("a pasted Bitbucket URL reveals it");
  }

  // And back again, so somebody who corrects the URL is not left with a token box for a
  // platform that has no use for one.
  await type($("#p-url"), "https://github.com/acme/docs");
  if (!$("#scm-fields").hidden) {
    fail("switching back to GitHub left the credential block on screen");
  } else {
    ok("hidden again for GitHub");
  }
  await click(btn("Cancel"));
}

console.log("\nediting a Bitbucket project shows what is stored");
{
  const bb = $$(".pcard [data-tab=build]").find(
    b => b.dataset.key === "bitbucket/netfoundry/customer-connect-docs");
  if (!bb) {
    fail("no Edit control on the Bitbucket project");
  } else {
    await click(bb);
    const box = $("#scm-fields");
    if (!box || box.hidden) {
      fail("the credential block is missing on a Bitbucket project's form");
    } else {
      // Names only, never values: "set" is the most that can be shown, because nothing
      // reads a stored credential back.
      const head = box.querySelector(".field-head").textContent;
      if (!/set/.test(head)) fail(`the access token is not marked set: ${JSON.stringify(head)}`);
      else ok("the stored access token is marked set");

      // Text and password only. A checkbox's value is "on" whether it is ticked or not,
      // so counting it made the private-repository question look like a leaked secret.
      const boxes = [...box.querySelectorAll("input[type=password]")];
      const leaked = boxes.filter(i => i.value !== "");
      if (leaked.length) {
        fail(`${leaked.length} credential box(es) are prefilled: ${
          JSON.stringify(leaked.map(i => i.id))}`);
      } else {
        ok(`${boxes.length} credential boxes, all empty`);
      }

      const test = box.querySelector("[data-test-scm]");
      if (!test) fail("no Test credential control on a saved project");
      else ok("Test credential offered");

      // The private-repository question, and what a blank field will do. With a
      // workspace-wide token stored, blank means "inherit"; with none it means "this
      // cannot clone" — opposite meanings for the same empty box, so the page has to say
      // which. This fixture stores no global credential.
      const priv = box.querySelector('input[type=checkbox]');
      if (!priv) {
        fail("nothing asks whether the repository is private");
      } else {
        ok("asks whether the repository is private");
      }
      // This project has its own token stored, so the line says so. The other two
      // branches — inherit a workspace-wide one, or nothing anywhere — are asserted below
      // by rendering with a different state.
      if (!/uses its own credential/.test(box.textContent)) {
        fail(`the form does not say which credential applies: ${
          JSON.stringify(box.querySelector(".why").textContent.trim())}`);
      } else {
        ok("says this repository uses its own");
      }

      // The credential rows use the secrets-page shape, with their own Save — a token is
      // pasted and then tested, so committing it must not require the form's Save.
      const row = box.querySelector('[data-scmrow="scm.access_token"]');
      if (!row || !row.querySelector("[data-set-scm]")) {
        fail("the access token has no Save of its own");
      } else if (!row.querySelector("[data-expand]")) {
        fail("the access token box has no expand control");
      } else {
        ok("credential rows have their own Save and expand");
      }

      // A public repository reports nothing: it clones with no credential at all.
      const priv2 = box.querySelector("[data-privtoggle]");
      priv2.checked = false;
      priv2.dispatchEvent(new win.Event("change", {bubbles: true}));
      await settle();
      if ($("#scm-flag").textContent.trim() !== "") {
        fail(`unchecking private left ${JSON.stringify($("#scm-flag").textContent.trim())}`);
      } else {
        ok("no credential state reported for a public repository");
      }
      priv2.checked = true;
      priv2.dispatchEvent(new win.Event("change", {bubbles: true}));
      await settle();
    }
  }
}

console.log("\na blank field says whether it inherits or fails");
{
  // The same empty box means opposite things depending on what is stored workspace-wide,
  // and the page is the only thing that can tell an operator which. Both branches, by
  // rendering the page with each state.
  const withGlobal = state();
  withGlobal.defaults.scm_global = ["bitbucket.access_token"];
  withGlobal.projects[1].scm = [];
  withGlobal.projects[1].private = true;
  win.eval(`projOpen = {key: "bitbucket/netfoundry/customer-connect-docs", tab: "build"}`);
  win.eval(`renderProjectsPage(${JSON.stringify(withGlobal)})`);
  await settle();

  let text = $("#scm-fields").textContent;
  if (!/inherit the workspace-wide credential/.test(text)) {
    fail("with a global token stored, a blank field does not say it inherits");
  } else {
    ok("blank inherits the workspace-wide credential");
  }
  // And it is marked inherited rather than missing: calling it missing tells an operator
  // to fix something that works.
  if (!/inherited/.test($("#scm-fields .field-head").textContent)) {
    fail("an inheriting project's credential is not marked inherited");
  } else {
    ok("marked inherited, not missing");
  }

  const withNothing = state();
  withNothing.projects[1].scm = [];
  withNothing.projects[1].private = true;
  win.eval(`renderProjectsPage(${JSON.stringify(withNothing)})`);
  await settle();

  text = $("#scm-fields").textContent;
  if (!/No workspace-wide credential is stored/.test(text)) {
    fail("with nothing stored anywhere, the form does not say a token is needed");
  } else {
    ok("says a private repo needs its own token here");
  }
  if (!/missing/.test($("#scm-fields .field-head").textContent)) {
    fail("with nothing to inherit, the credential is not marked missing");
  } else {
    ok("marked missing when there is nothing to fall back to");
  }

  // Back to the original state for the sections below.
  win.eval(`renderProjectsPage(${JSON.stringify(state())})`);
  await settle();
}

console.log("\ntesting a credential asks the platform");
{
  const test = $("[data-test-scm]");
  calls.length = 0;
  await click(test);

  const post = calls.find(c => c.method === "POST");
  const want = "/api/projects/bitbucket/netfoundry/customer-connect-docs/scm-test";
  if (!post) fail("Test credential sent no request");
  else if (post.url !== want) fail(`POST ${post.url}, want ${want}`);
  else ok(`POST ${post.url}`);

  // The form must stay open: if the answer is bad, this is where the token gets fixed.
  if (!$("#scm-fields")) {
    fail("the form closed on a credential test, taking the field with it");
  } else {
    ok("the form stays open");
  }

  // A toast lives for five seconds, which outlasts the rest of this file — so the next
  // section's "was anything toasted" check would read this one's. Cleared rather than
  // waited out.
  $$("#toasts .toast").forEach(t => t.remove());
}

console.log("\nsaving sends only the credential boxes that were typed into");
{
  // An empty box means "leave what is stored alone". The alternative is that editing any
  // other field on the form silently clears the token, which is a build that stops
  // working for a reason nothing reports.
  const key = "bitbucket/netfoundry/customer-connect-docs";
  doc.getElementById(`p-scmtoken-${key}`).value = "a-new-repository-token";
  calls.length = 0;
  await click(btn("Save changes"));

  const scm = calls.filter(c => c.url.includes("/scm/"));
  if (scm.length !== 1) {
    fail(`${scm.length} credential requests, want 1: ${JSON.stringify(scm.map(c => c.url))}`);
  } else if (!scm[0].url.endsWith("/scm/scm.access_token")) {
    fail(`PUT ${scm[0].url}`);
  } else if (scm[0].body?.value !== "a-new-repository-token") {
    fail("the token did not travel with the request");
  } else {
    ok(`PUT ${scm[0].url}`);
  }
  // The row itself still goes to the project endpoint, separately: one is sqlite, the
  // other is the vault, and the credential must not travel in a payload that is logged.
  if (!calls.some(c => c.method === "PUT" && c.url === `/api/projects/${key}`)) {
    fail("the project row was not saved");
  }
}

console.log("\nsaving an edit closes the form and says so");
{
  await click($$(".pcard [data-tab=build]")[0]);
  if (!$(".pcard .panel")) fail("Edit did not open the panel");
  calls.length = 0;
  await click(btn("Save changes"));

  if ($(".pcard .panel")) {
    fail("the form is still open after saving, which looks identical to nothing happening");
  }
  // A toast, over the page. An inline notice landed where the form used to be — which is
  // where the eye has already stopped looking — so a successful save read as nothing
  // happening.
  const note = $("#toasts .toast");
  if (!note || !note.textContent.includes("Saved")) {
    fail(`no confirmation after saving: ${JSON.stringify(note?.textContent || null)}`);
  } else ok(`closed, and toasted ${JSON.stringify(note.textContent.trim())}`);

  // An edit must not scan: the pull requests are already known to the daemon, and
  // re-queueing every one of them on every settings tweak would rebuild the world.
  if (calls.some(c => c.method === "POST" && c.url.endsWith("/scan"))) {
    fail("editing a project queued builds for every open pull request");
  }
}

console.log("\nno per-card build button");
{
  // It queued one build per open pull request — the same thing adding a project does — and
  // on a repository with several it read as having picked one at random. Adding a project
  // still scans; the button that invited it by hand is gone from every card.
  if (btn("Build open PRs") || btn("Build now")) {
    fail("a per-card build button is back on the project cards");
  } else ok("no Build control on a card");

  // The scan on *add* is a different thing and must survive, which the add flow asserts
  // further down. Nothing here posts a scan.
  calls.length = 0;
  await click($$(".pcard [data-tab=build]")[0]);
  if (calls.some(c => c.url.endsWith("/scan"))) fail("opening Edit scanned the repository");
  await click(btn("Cancel — discard edits") || btn("Cancel"));
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
  // Reopen the section: the sections above left the page elsewhere, and a harness that
  // depends on the previous section's leftover state breaks the moment one is inserted.
  if (!$(".pcard .env")) await openSecrets($$(".pcard [data-secrets]")[0]);
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
  // The section stays open across the refresh: the operator is usually adding several,
  // and one that closed after each would be a click per token. This is what the
  // out-of-DOM open-state set is for — the page rebuilds its markup on every save.
  const after = $$(".pcard [data-secrets]").find(d => d.dataset.secrets === key);
  if (!after || !after.open) {
    fail("the variables section collapsed after adding one");
  } else {
    ok("still expanded, ready for the next");
  }
}

console.log("\nremoving a variable");
{
  calls.length = 0;
  await click($(".pcard [data-secret] [data-del-secret]"));
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
  await openSecrets($$(".pcard [data-secrets]")[0]);
  const key = "github/netfoundry/unified-doc";
  doc.getElementById(`s-env-${key}`).value = "BB_REPO_TOKEN_ONPREM";
  doc.getElementById(`s-val-${key}`).value = "a-token-value";
  await click(btn("Add variable"));

  // A toast, not a notice in the panel. Ten rejected saves used to leave ten stacked
  // red boxes above the form, pushing the fields being corrected off the screen — so
  // this asserts both halves: the message is shown, and the document did not grow a
  // permanent notice to show it.
  const t = $("#toasts .toast.bad");
  if (!t) {
    fail("a failed call reported nothing on the page it happened on");
  } else if (!t.textContent.includes("vault is locked")) {
    fail(`the toast says ${JSON.stringify(t.textContent)}`);
  } else {
    ok("the error is toasted");
  }
  if ($("#projects-body .notice.bad")) {
    fail("the error was also left in the page, which is what stacked up");
  }
  if ($("#setup-body .notice.bad")) {
    fail("the error was also written into the hidden secrets panel");
  }

  // Two failures in a row leave two toasts and no residue in the form.
  nextFails = "the vault is locked; unlock it at /secrets first";
  doc.getElementById(`s-env-${key}`).value = "BB_REPO_TOKEN_ONPREM";
  doc.getElementById(`s-val-${key}`).value = "a-token-value";
  await click(btn("Add variable"));
  if ($$("#projects-body .notice.bad").length) {
    fail("a second failure accumulated in the form");
  } else {
    ok(`${$$("#toasts .toast.bad").length} toasts, nothing added to the form`);
  }
}

console.log("\nunlinked pull requests are listed, and can be linked back");
{
  const cards = $$(".pcard");
  const bb = cards[1];

  // Only where there is something to say. The first version put a line on every card
  // saying everything was being built — true, unasked, and one more line to read past.
  if (cards[0].querySelector(".pcard-links")) {
    fail("a project with nothing unlinked still renders the strip");
  } else ok("silent on a project that is building everything");

  const links = bb.querySelector(".pcard-links");
  if (!links) {
    fail("the card with an unlinked pull request says nothing about it");
  } else if (!/Skipping\s+1\s+pull request/.test(links.textContent.replace(/\s+/g, " "))) {
    fail(`the strip reads ${JSON.stringify(links.textContent.replace(/\s+/g, " ").trim())}`);
  } else ok("the count is on the card");

  // The numbers themselves are in the dialog, not on the card: which pull request, what
  // branch, when it was unlinked and a way back is four facts per row.
  calls.length = 0;
  const relink = bb.querySelector("[data-relink]");
  if (!relink) fail("no way to reach the unlinked pull requests");
  else {
    await click(relink);
    const picker = $(".modal .picklist");
    if (!picker) {
      fail("the button did not open a picker");
    } else {
      if (calls.some(c => c.method === "POST")) {
        fail("opening the picker posted something");
      } else ok("the picker opens without posting");

      const rows = $$(".modal .pickrow");
      if (rows.length !== 1) fail(`${rows.length} rows in the picker, want 1`);
      else if (!rows[0].textContent.includes("#20")) {
        fail(`the row reads ${JSON.stringify(rows[0].textContent.replace(/\s+/g, " ").trim())}`);
      } else if (!rows[0].textContent.includes("feature/pricing")) {
        fail("the row does not say which branch it was");
      } else ok("one row per unlinked pull request, with its branch");

      // Escape closes it and posts nothing.
      doc.dispatchEvent(new win.KeyboardEvent("keydown", {key: "Escape", bubbles: true}));
      await settle();
      if ($(".modal .picklist")) fail("Escape did not close the picker");
      else if (calls.some(c => c.method === "POST")) fail("Escape linked it anyway");
      else ok("Escape closes it and links nothing");

      // Choosing a row is the whole gesture: the row is the button.
      calls.length = 0;
      await click($$(".pcard")[1].querySelector("[data-relink]"));
      const row = $(".modal .pickrow");
      if (!row) fail("the picker did not reopen");
      else {
        await click(row);
        const post = calls.find(c => c.method === "POST" && c.url.endsWith("/link"));
        if (!post) fail(`choosing a row posted nothing: ${JSON.stringify(calls)}`);
        else if (!post.url.includes("/bitbucket/netfoundry/customer-connect-docs/")) {
          fail(`it posted to ${post.url}`);
        } else if (post.body?.number !== 20) {
          fail(`it sent ${JSON.stringify(post.body)}, want number 20`);
        } else ok("clicking a row posts its number to its own project");
        if ($(".modal .picklist")) fail("the picker stayed open after choosing");
      }
    }
  }
}

console.log("\nthe default branch's preview is on the card");
{
  const cards = $$(".pcard");
  // The project that has one: a link to it, its branch, and its state.
  const strip = cards[1].querySelector(".pcard-branch");
  if (!strip) {
    fail("a project with a branch preview does not show it");
  } else {
    const text = strip.textContent.replace(/\s+/g, " ").trim();
    if (!text.includes("master")) {
      fail(`the strip does not name the branch: ${JSON.stringify(text)}`);
    } else ok(`reads ${JSON.stringify(text.slice(0, 40))}`);

    // The name comes from the platform, so a page that assumed "main" would be wrong on
    // every repository that never renamed.
    if (text.includes("main")) fail("the page invented the branch name main");

    const open = strip.querySelector('a[href^="https://"]');
    if (!open) fail("no link to the branch preview");
    else if (open.href !== "https://customer-connect-docs-master.shares.zrok.io/") {
      fail(`the link goes to ${open.href}`);
    } else ok("links to the published URL");
  }

  // The project that has none says so and offers to start one, rather than leaving a blank
  // that reads as a broken feature.
  const none = cards[0].querySelector(".pcard-branch.none");
  if (!none) {
    fail("a project with no branch preview says nothing about it");
  } else if (!none.querySelector("[data-branch]")) {
    fail("nothing offers to build the default branch");
  } else ok("offers to build it where there is none");

  // And the button posts to the branch route, with no branch named — the server reads the
  // repository's default, which is the whole point of not asking here.
  calls.length = 0;
  const start = cards[0].querySelector("[data-branch]");
  await click(start);
  const post = calls.find(c => c.method === "POST" && c.url.endsWith("/branch"));
  if (!post) fail(`Build the default branch posted nothing: ${JSON.stringify(calls)}`);
  else if (post.url !== "/api/projects/github/netfoundry/unified-doc/branch") {
    fail(`it posted to ${post.url}`);
  } else if (post.body && post.body.branch) {
    fail(`it named a branch (${post.body.branch}); the server decides`);
  } else ok("POST /api/projects/github/netfoundry/unified-doc/branch");
}

console.log("\na note from the server is toasted, not swallowed");
{
  // A project saves even when its default-branch preview could not be started — the row is
  // correct and only that one action failed. The page has to say so: a save that silently
  // did nine tenths of the job is the failure this exists to prevent.
  const el = doc.getElementById("projects-body");
  win.eval(`renderProjectsPage(${JSON.stringify({
    can_write: true, secrets_available: true, vault_locked: false,
    global_secrets: [], defaults: {driver: "docker", images: []}, projects: [],
    note: "no github client is configured on this daemon",
  })})`);
  await settle();
  const t = [...doc.querySelectorAll("#toasts .toast")]
    .find(x => x.textContent.includes("no github client"));
  if (!t) fail("the server's note was dropped");
  else ok("toasted the note");
  if (el && el.querySelector(".notice.bad")) {
    fail("the note was also left in the page, which is what stacked up");
  }
}

console.log(failures ? `\n${failures} failure(s)` : `\nall projects-page checks OK`);
process.exit(failures ? 1 : 0);
