// Covers the confirm dialog's handling of text that arrives from a repository.
//
// # Why this exists
//
// The dialog `ask()` opens was built by assigning a template to innerHTML, and three of its
// fields went in as HTML. Unlink and Cancel put the pull request's branch name in one of them
// unescaped. A branch name is whatever somebody with push access typed, and git allows `<`,
// `>`, `=` and `/` in one, so a branch named `<img src=x onerror=…>` ran script in the
// dashboard of whoever clicked Cancel on the junk build: with an admin's session, that is the
// whole admin API.
//
// What is asserted is the DOM, not the source: no element the branch or project name asked
// for exists in the dialog, the name is shown as the text it is, and the dialog's own markup
// (the `#19` span, the bold "Reversible") still renders as markup.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/askescape.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

const hostileBranch = "<img/src=x/onerror=window.__pwned=1>";
const hostileLabel = "<svg/onload=window.__pwned=2>Docs";

const finishedAt = new Date(Date.now() - 8000).toISOString();
const startedAt = new Date(Date.now() - 42000).toISOString();

const status = {
  exposer: "zrok2", instance: "test", pending: 0, running: 1,
  projects: [
    {key: "github:acme/docs", label: hostileLabel, avatar: ""},
    {key: "github:acme/site", label: "Site", avatar: ""},
  ],
  previews: [
    {preview_id: "81379294374a", repo: "github:acme/docs",
     number: 19, branch: hostileBranch + "unlinkme", name: "docs-x", url: "https://docs-x.example/",
     state: "ready", updated_at: finishedAt, commit: "cf9f37d25cf7515f8c7e531afbe97cc6ee4238f3",
     seconds: 69},
    {preview_id: "b400e0aa1234", repo: "github:acme/site",
     number: 20, branch: hostileBranch + "cancelme", name: "site-x", url: "",
     state: "building", updated_at: startedAt, commit: "a4fd6c9db194"},
  ],
  events: [],
};

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  // So an <img> the page was tricked into creating would really load and fire onerror.
  resources: "usable",
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
        : url === "/api/admin" ? {secrets: true, projects: true}
        : url.startsWith("/logs/") ? {live: false, preview_id: "81379294374a", logs: [
            {preview_id: "81379294374a", build_id: "20260730-184200-cf9f37d",
             state: "ready", mod_time: finishedAt, started_at: startedAt, seconds: 69,
             url: "https://docs-x-cf9f37d.example/"}]}
        : {};
      return {ok: true, status: 200, statusText: "OK",
        json: async () => body, text: async () => JSON.stringify(body)};
    };
    win.matchMedia = () => ({matches: false, addEventListener() {}});
    win.HTMLElement.prototype.scrollIntoView = function () {};
    win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
    win.requestAnimationFrame = fn => setTimeout(fn, 0);
    win.getSelection = () => ({isCollapsed: true});
    win.confirm = () => true;
    win.alert = () => {};
  },
});

const win = dom.window;
const doc = win.document;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 300));

const $ = s => doc.querySelector(s);
const $$ = s => [...doc.querySelectorAll(s)];
const settle = () => new Promise(r => setTimeout(r, 150));
const click = async el => {
  el.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
};

const ui = win.eval("ui");
ui.previews = status.previews;
ui.events = status.events;
ui.status = status;
win.eval("render()");
await settle();

// checkDialog asserts on the open dialog and closes it.
const checkDialog = async (label, wantMarkup) => {
  const dialog = $(".modal.ask");
  if (!dialog) {
    fail(`${label}: no dialog opened`);
    return;
  }
  const what = dialog.querySelector(".ask-what");
  const lede = dialog.querySelector(".ask-lede");

  if (dialog.querySelector("img, svg, script, iframe")) {
    fail(`${label}: the dialog contains an element that came from a branch or project name`);
  } else ok(`${label}: no element was created from the branch or project name`);

  if (!what.textContent.includes(hostileBranch)) {
    fail(`${label}: the branch is not shown as text: ${JSON.stringify(what.textContent)}`);
  } else ok(`${label}: the branch name is shown as the text it is`);

  // The lede names the repository and is plain text, escaped by ask() rather than by the
  // caller. An entity showing up here would mean it was escaped twice.
  if (!/^\w+ — /.test(lede.textContent.trim())) {
    fail(`${label}: the lede does not start with the repository: ${JSON.stringify(lede.textContent)}`);
  } else ok(`${label}: the lede names the repository as text`);

  // The dialog's own markup still renders. Escaping everything would also pass the
  // assertions above, and would show the reader a literal <span>.
  const number = what.querySelector("span.mono");
  if (!number || !/^#\d+$/.test(number.textContent)) {
    fail(`${label}: the pull request number lost its markup: ${JSON.stringify(what.innerHTML)}`);
  } else ok(`${label}: the dialog's own markup still renders`);
  if (dialog.textContent.includes("<span") || dialog.textContent.includes("<b>") ||
      dialog.textContent.includes("&lt;")) {
    fail(`${label}: markup or an entity is shown as literal text: ${dialog.textContent.trim().slice(0, 200)}`);
  }
  if (wantMarkup && !dialog.querySelector(wantMarkup)) {
    fail(`${label}: expected ${wantMarkup} in the dialog`);
  }

  await click(dialog.querySelector('.btn[data-ask="no"]'));
  if ($(".modal.ask")) fail(`${label}: Cancel did not close the dialog`);
};

console.log("Unlink shows a hostile branch name as text");
{
  const head = $$(".item .head").find(h => h.textContent.includes("unlinkme"));
  if (!head) fail("no row for the finished preview");
  else {
    await click(head);
    await settle();
    const unlink = $('.item.open [data-role="unlink"]');
    if (!unlink || unlink.hidden) fail("no Unlink control on the expanded row");
    else {
      await click(unlink);
      await checkDialog("unlink", ".ask-note b");
    }
  }
}

console.log("\nCancel shows a hostile branch name as text");
{
  const head = $$(".item .head").find(h => h.textContent.includes("cancelme"));
  if (!head) fail("no row for the running build");
  else {
    await click(head);
    await settle();
    const cancel = $('.item.open [data-role="cancel"]');
    if (!cancel || cancel.hidden) fail("no Cancel control on the running build's row");
    else {
      await click(cancel);
      await checkDialog("cancel", "");
    }
  }
}

console.log("\nask() escapes by default, and markup`` escapes what is interpolated");
{
  win.eval(`ask({kind: "<i>k</i>", what: "<img src=x onerror=window.__pwned=3>",
    lede: "<u>l</u>", note: "<s>n</s>", goes: ["<em>g</em>"], keeps: ["<em>k</em>"],
    confirm: "<b>c</b>"})`);
  await settle();
  const dialog = $(".modal.ask");
  if (!dialog) fail("no dialog");
  else {
    if (dialog.querySelector("img, i, u, s, em, .ask-acts b")) {
      fail(`a plain string was rendered as HTML: ${dialog.querySelector(".modal-body").innerHTML.slice(0, 300)}`);
    } else ok("every plain string field is text");
    await click(dialog.querySelector('.btn[data-ask="no"]'));
  }

  const made = win.eval("markup`<code>${'<img src=x onerror=window.__pwned=4>'}</code>`").html;
  if (made !== "<code>&lt;img src=x onerror=window.__pwned=4&gt;</code>") {
    fail(`markup did not escape its interpolation: ${made}`);
  } else ok("markup`` keeps its literal tags and escapes the value");
}

// Give a loaded image time to fail and fire its handler before reading the flag.
await new Promise(r => setTimeout(r, 400));
if (win.__pwned !== undefined) {
  fail(`script from a branch or project name ran: window.__pwned = ${win.__pwned}`);
} else ok("no script from a branch or project name ran");

console.log(failures ? `\n${failures} FAILED` : "\nall passed");
process.exit(failures ? 1 : 0);
