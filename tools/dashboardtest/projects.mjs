// Loads the real dashboard at /projects in a DOM and drives the page the way an
// operator would: open a project, add an environment variable, remove one, toggle a
// project off, add a project.
//
// # Why this exists
//
// Two rules the page depends on that are invisible from reading the JavaScript:
//
//   - `.wrap` sets `display: grid`, which beats `[hidden]`'s `display: none`. So
//     `el.hidden = true` alone would do nothing, and the previews and activity sections
//     would render under both admin pages with nothing in them.
//   - `run()` must render its result through this page's own renderer and print its
//     errors to `#projects-body`, not to `#setup-body`, which is hidden here. A failed
//     project save otherwise does nothing and says nothing.
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
// disabled. The second is the row a bare line of text would render as, if the
// disabled state were not carried through.
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
/* A project's settings are behind a disclosure now: the row carries identity and the live branch
   preview, and everything else opens on demand. So anything asserting on the facts, the notes or
   the environment variables has to open the row first — the same click a reader makes.

   Takes the card's data-card key. A no-op when that row has nothing to disclose, which is a
   project deferring entirely to its own .docpreview.yml. */
// Idempotent, because the state lives outside the DOM and survives every re-render: a second
// caller that clicked unconditionally would close what the first one opened.
const openDetails = async key => {
  const b = doc.querySelector(`[data-details="${key}"]`);
  if (!b) return false;
  if (b.getAttribute("aria-expanded") === "true") return true;
  await click(b);
  return true;
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

  // Collapsed, a row is identity plus the live branch preview and nothing else. That is the whole
  // point of the shape: the settings are what made four projects a wall of text.
  const firstKey = cards[0].dataset.card;
  if (cards[0].querySelector(".facts")) {
    fail("the settings are rendered before anybody asked for them");
  } else {
    ok("a collapsed row carries no settings");
  }

  await openDetails(firstKey);

  const first = $(`[data-card="${firstKey}"]`);
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

  // The second project states nothing at all, and must say so in words rather than offering a
  // disclosure that opens onto nothing.
  const deferring = $$(".pcard")[1];
  if (deferring.querySelector("[data-details]")) {
    fail("a project with no settings offers to disclose them anyway");
  }
  if (!deferring.textContent.includes("from the repository")) {
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
  // what it is. It sits in the identity block's second line with the repository path now,
  // rather than as a third chip beside the name.
  if (!$$(".pcard-id .sub").some(el => el.textContent.includes("bitbucket"))) {
    fail(`the platform is missing: ${JSON.stringify($$(".pcard-id .sub").map(e => e.textContent))}`);
  }
  ok("display name shown with owner/repo and platform beside it");
}

console.log("\nadding is reachable without scrolling past every project");
{
  // Both the button and the form it opens must stay above the list. Below it, adding a
  // project on a page of thirty would mean scrolling past every one that already exists,
  // and the form would then open where the button was — the bottom of a long page —
  // putting the fields to fill in somewhere the eye had never been.
  //
  // The button now lives in the page's action row beside Settings, outside #projects-body, so
  // "above the list" is structural rather than a thing a render has to keep getting right.
  const body = $("#projects-body");
  const btnEl = $("#p-new-top");
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

console.log("\nsecrets panel");
{
  // An accordion in the card, not a `Secrets` button beside `Edit`. As a button it made
  // a project's tokens a mode the card switched into, mutually exclusive with the form —
  // so checking a variable meant leaving whatever was being edited.
  //
  // Inside the row's disclosure now, with the rest of the settings, so the row has to be opened
  // first — the same click a reader makes.
  await openDetails($$(".pcard")[0].dataset.card);
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

console.log("\nno per-card build button");
{
  // It queued one build per open pull request — the same thing adding a project does — and
  // on a repository with several it read as having picked one at random. Adding a project
  // still scans; the button that invited it by hand is gone from every card.
  if (btn("Build open PRs") || btn("Build now")) {
    fail("a per-card build button is back on the project cards");
  } else ok("no Build control on a card");

  /* Edit is a link to the form's own page, not a button that expands a panel here.

     Nineteen fields expanding in place pushed every project below it off the screen, which is the
     one thing a list must not do. The form's behaviour is in projectedit.mjs, against the page it
     now lives on; what belongs here is that the list points at it, correctly, per project. */
  const edits = $$(".pcard a.btn[href^='/projects/']");
  if (!edits.length) {
    fail("no Edit link on any card");
  } else {
    const hrefs = edits.map(a => a.getAttribute("href"));
    const want = "/projects/github/netfoundry/unified-doc";
    if (!hrefs.includes(want)) {
      fail(`no Edit link to ${want}: ${JSON.stringify(hrefs)}`);
    } else {
      ok(`Edit links to ${want}`);
    }
  }
  // And nothing on this page renders the form itself.
  if ($(".grid-form") || $("#p-url")) {
    fail("the project form is still rendered inside the list");
  } else {
    ok("no form on the list page");
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

// Disable is one of the form's actions, so it moved to the form's page with the rest of them.
// projectedit.mjs asserts that it preserves the row.

console.log("\na failure is reported where it can be seen");
{
  nextFails = "the vault is locked; unlock it at /secrets first";
  await openSecrets($$(".pcard [data-secrets]")[0]);
  const key = "github/netfoundry/unified-doc";
  doc.getElementById(`s-env-${key}`).value = "BB_REPO_TOKEN_ONPREM";
  doc.getElementById(`s-val-${key}`).value = "a-token-value";
  await click(btn("Add variable"));

  // A toast, not a notice in the panel. A notice in the panel would leave a stacked red
  // box above the form for every rejected save, pushing the fields being corrected off
  // the screen — so this asserts both halves: the message is shown, and the document
  // did not grow a permanent notice to show it.
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

  // Only where there is something to say. A line on every card stating that everything
  // is being built would be true, unasked, and one more line to read past.
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

console.log("\nthe installation's hostname prefix is not on this page");
{
  /* It moved to the Settings page. It belongs to no project — it changes the public hostname of
     every preview at once — and sitting in the same row as New project, with its own Save button
     above a list of cards, it read as though it belonged to the first card.

     Asserted here as an absence, because the alternative is nobody noticing it came back. Its
     behaviour is tested in prefix.mjs, against the page it now lives on. */
  for (const id of ["p-prefix", "p-prefix-eg"]) {
    if (doc.getElementById(id)) fail(`#${id} is back on the projects page`);
  }
  if (doc.querySelector("[data-save-prefix]")) {
    fail("the prefix Save button is back on the projects page");
  }
  ok("no prefix field, no Save button — both are on Settings");
}

console.log("\nthe default branch's preview is on the card");
{
  const cards = $$(".pcard");
  // The project that has one: a link to it, its branch, and its state.
  const strip = cards[1].querySelector(".pbranch");
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
  const none = cards[0].querySelector(".pbranch.none");
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
