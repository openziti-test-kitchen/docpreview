// Covers two things on the operations dashboard that a screenshot cannot settle: what a
// finished build's timestamp does over time, and what Unlink sends.
//
// # Why this exists
//
// **The timestamp.** A finished preview rendered its relative age, which under a minute
// counts up once a second. Beside a status line already saying "took 1m 9s", that read as
// a build still being timed — reported as "it shows it was built but the timer keeps
// ticking up". The fix is a fixed finishing time for anything finished and the stopwatch
// only for something running, and the difference between those two is a property of a
// render repeated a second apart. Nothing static can check it.
//
// **Unlink.** It deletes a preview, its shares and its pull request comment, and records
// that the pull request must not be built again. A button that renders correctly and posts
// to the wrong preview would remove somebody else's, so what it sends is asserted rather
// than looked at.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/unlink.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

// One preview that finished a few seconds ago, and one still building. Both are needed:
// the point is that only one of the two moves.
//
// On two different projects, deliberately. Every preview of one repository shares a
// project row, and that row renders the selected preview — so two previews of one project
// give one visible time, and the building one wins.
const finishedAt = new Date(Date.now() - 8000).toISOString();
const startedAt = new Date(Date.now() - 42000).toISOString();

const status = {
  exposer: "zrok2", instance: "test", pending: 0, running: 1,
  projects: [
    {key: "bitbucket:netfoundry/customer-connect-docs", label: "Customer Connect", avatar: ""},
    {key: "github:netfoundry/unified-doc", label: "Unified Doc", avatar: ""},
  ],
  previews: [
    {preview_id: "81379294374a", repo: "bitbucket:netfoundry/customer-connect-docs",
     number: 19, branch: "feature/assistant", name: "ccd-fa", url: "https://ccd-fa.example/",
     state: "ready", updated_at: finishedAt, commit: "cf9f37d25cf7515f8c7e531afbe97cc6ee4238f3",
     seconds: 69},
    {preview_id: "b400e0aa1234", repo: "github:netfoundry/unified-doc",
     number: 20, branch: "feature/pricing", name: "ccd-fp", url: "",
     state: "building", updated_at: startedAt, commit: "a4fd6c9db194"},
  ],
  events: [],
};

const calls = [];

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
    win.fetch = async (url, init) => {
      const method = init?.method || "GET";
      calls.push({method, url});
      const body = url === "/status" ? status
        : url === "/api/admin" ? {secrets: true, projects: true}
        : url.startsWith("/logs/") ? {live: false, preview_id: "81379294374a", logs: [
            {preview_id: "81379294374a", build_id: "20260730-184200-cf9f37d",
             state: "ready", mod_time: finishedAt, started_at: startedAt, seconds: 69,
             url: "https://ccd-fa-cf9f37d.example/"}]}
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
    win.alert = msg => { calls.push({method: "ALERT", url: msg}); };
  },
});

const win = dom.window;
const doc = win.document;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 300));

const $ = s => doc.querySelector(s);
const $$ = s => [...doc.querySelectorAll(s)];
const settle = () => new Promise(r => setTimeout(r, 120));
const click = async el => {
  el.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
};

// The page's state arrives over the EventSource in production, which the stub above does
// not deliver. Inject it and render, as switcher.mjs does.
const ui = win.eval("ui");
ui.previews = status.previews;
ui.events = status.events;
ui.status = status;
win.eval("render()");
await settle();

console.log("a finished build's time does not tick");
{
  const rowTime = branch => {
    const head = $$(".item .head").find(h => h.textContent.includes(branch));
    return head ? head.querySelector(".when")?.textContent.trim() : undefined;
  };

  const readyBefore = rowTime("feature/assistant");
  const liveBefore = rowTime("feature/pricing");
  if (readyBefore === undefined || liveBefore === undefined) {
    fail(`could not find the row times: ready=${readyBefore} live=${liveBefore}`);
  }

  // Long enough that a per-second counter has to have moved. The page re-renders on its
  // own tick, so nothing here forces it.
  await new Promise(r => setTimeout(r, 2200));

  const readyAfter = rowTime("feature/assistant");
  const liveAfter = rowTime("feature/pricing");

  if (readyBefore !== readyAfter) {
    fail(`a finished preview's time changed from ${JSON.stringify(readyBefore)} to ` +
      `${JSON.stringify(readyAfter)} — that is the ticking clock on a build that is over`);
  } else ok(`a finished preview stays at ${JSON.stringify(readyAfter)}`);

  // The other half. A page where nothing moves cannot be told from a page that has
  // stopped updating, and the running build is what says it is alive.
  if (liveBefore === liveAfter) {
    fail(`a running build's stopwatch is stuck at ${JSON.stringify(liveAfter)}`);
  } else ok(`a running build still counts: ${liveBefore} -> ${liveAfter}`);

  // Shape, not just stability: "8s ago" would also be stable if the render stopped, and
  // the whole complaint was about a value that reads like a duration.
  if (/\bago\b|^\d+[smhd]$/.test(String(readyAfter))) {
    fail(`a finished preview reads as an age: ${JSON.stringify(readyAfter)}`);
  } else ok("it reads as a time, not an age");
}

console.log("\nUnlink removes the preview it is attached to");
{
  // Expand the finished preview's row: the control lives with the log pane, beside
  // Rebuild, because both act on the preview whose log is on screen.
  const head = $$(".item .head").find(h => h.textContent.includes("feature/assistant"));
  if (!head) fail("no row for the finished preview");
  else {
    await click(head);
    await settle();

    const unlink = $('.item.open [data-role="unlink"]');
    if (!unlink) {
      fail("no Unlink control on an expanded row");
    } else if (unlink.hidden) {
      fail("Unlink is hidden on a page that /api/admin says can write");
    } else {
      calls.length = 0;
      await click(unlink);

      // A dialog of the page's own, not the browser's. `confirm()` cannot show what is
      // about to be deleted, which is the only thing anybody wants to know here — and it
      // is stubbed to true in this harness, so a regression to it would silently pass
      // every assertion below.
      // `.modal.ask`, not `.modal`: the boot report is also a .modal and is present from
      // the start, so the bare selector finds that one and every assertion below reads an
      // empty element and passes for the wrong reason.
      const dialog = $(".modal.ask");
      if (!dialog) {
        fail("Unlink did not open a dialog of its own");
      } else {
        if (calls.some(c => c.method === "POST")) {
          fail("Unlink posted before the dialog was answered");
        } else ok("nothing is posted until the dialog is answered");

        // It answers the question rather than only asking one. Both halves: what goes,
        // and that it can be undone.
        const said = dialog.textContent;
        for (const phrase of ["Removed now", "comes back", "Reversible", "#19"]) {
          if (!said.includes(phrase)) {
            fail(`the dialog does not mention ${JSON.stringify(phrase)}`);
          }
        }
        if (!failures) ok("says what goes, what returns, and that it is reversible");

        // Cancel has focus, not the destructive button. A stray Return on a dialog that
        // opens focused on Unlink is an irreversible action nobody chose.
        const focused = doc.activeElement;
        if (!focused || focused.dataset.ask !== "no") {
          fail(`focus is on ${focused && focused.textContent}, not Cancel`);
        } else ok("Cancel has focus, so Return does not delete anything");

        // Escape cancels, and posts nothing.
        dialog.ownerDocument.dispatchEvent(
          new win.KeyboardEvent("keydown", {key: "Escape", bubbles: true}));
        await settle();
        if ($(".modal.ask")) fail("Escape did not close the dialog");
        else if (calls.some(c => c.method === "POST")) fail("Escape posted the request anyway");
        else ok("Escape cancels and sends nothing");

        // And the whole way through, from the top.
        calls.length = 0;
        await click($('.item.open [data-role="unlink"]'));
        const yes = $('.modal.ask .ask-acts [data-ask="yes"]');
        if (!yes) fail("the dialog has no confirming button");
        else {
          await click(yes);
          const post = calls.find(c => c.method === "POST");
          if (!post) {
            fail(`confirming posted nothing: ${JSON.stringify(calls)}`);
          } else if (post.url !== "/api/builds/81379294374a/unlink") {
            fail(`Unlink posted to ${post.url}`);
          } else ok("POST /api/builds/81379294374a/unlink");
          if ($(".modal.ask")) fail("the dialog stayed open after confirming");
        }

        // No browser alert anywhere in the flow. Failures are toasts; success is the row
        // disappearing on the next status tick.
        if (calls.some(c => c.method === "ALERT")) {
          fail("Unlink raised an alert");
        } else ok("no alert(), before or after");
      }
    }
  }
}

console.log("\nUnlink is absent where the page cannot write");
{
  // A read-only dashboard — the public one, served through the dashboard-only share —
  // must not offer it. /api/admin 404s there, so the page's own answer is "no", and a
  // button that 403s teaches the reader to distrust the page.
  const dom2 = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    url: "http://127.0.0.1:8471/",
    virtualConsole: new VirtualConsole(),
    pretendToBeVisual: true,
    beforeParse(win) {
      win.EventSource = class {
        constructor() { this.readyState = 1; }
        addEventListener() {}
        close() { this.readyState = 2; }
      };
      win.fetch = async url => {
        if (url === "/api/admin") {
          return {ok: false, status: 404, statusText: "Not Found",
            text: async () => "404", json: async () => ({})};
        }
        const body = url === "/status" ? status : {};
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
  const w2 = dom2.window;
  await new Promise(r => w2.addEventListener("load", r));
  await new Promise(r => setTimeout(r, 300));
  const ui2 = w2.eval("ui");
  ui2.previews = status.previews;
  ui2.events = status.events;
  ui2.status = status;
  w2.eval("render()");
  await new Promise(r => setTimeout(r, 120));

  const head = [...w2.document.querySelectorAll(".item .head")]
    .find(h => h.textContent.includes("feature/assistant"));
  if (head) {
    head.dispatchEvent(new w2.MouseEvent("click", {bubbles: true}));
    await new Promise(r => setTimeout(r, 200));
  }
  const unlink = w2.document.querySelector('.item.open [data-role="unlink"]');
  if (unlink && !unlink.hidden) {
    fail("a read-only dashboard offers Unlink");
  } else ok("hidden where /api/admin says no");
  dom2.window.close();
}

console.log(failures ? `\n${failures} failure(s)` : `\nall unlink checks OK`);
dom.window.close();
process.exit(failures ? 1 : 0);
