// Loads the real dashboard and lets its session expire underneath it.
//
// # Why this exists
//
// The daemon signs its session cookie with a secret held in memory, so every restart
// invalidates every session — which is most restarts, during development. A tab left
// open across one then behaved like this: the stream failed, the header said
// "reconnecting…", the panels kept rendering their last good state, and nothing on the
// page said the word "login". The reader's conclusion is that the dashboard is broken.
//
// It is not visible by reading the code either, because EventSource exposes no status
// code: a 401 and a stopped daemon arrive as the same `error` event. So the page has to
// ask, and this harness is the only thing that can show that it does.
//
// # What it checks
//
//   - A stream error makes the page probe an authenticated endpoint. /status, not
//     /readyz: /readyz answers without a login and so can never return the 401 being
//     looked for, which would make the probe useless and passing.
//   - A 401 from that probe navigates away. jsdom refuses to navigate and reports the
//     attempt, which is what the assertion reads — see the note on the target URL below.
//   - A stream error while the session is still good navigates nowhere. Without this,
//     the harness would pass on a page that redirected to the login on every hiccup.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node expired.mjs
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

// run loads the page with a stream that fails immediately and a fetch that answers
// every request with `status`. Returns what was fetched and what jsdom said about any
// attempted navigation.
async function run(status) {
  const fetched = [];
  const navigations = [];
  const vc = new VirtualConsole();
  // A blocked navigation is how the redirect shows up here. jsdom refuses to navigate
  // and reports it as a jsdomError — so this counts attempts.
  //
  // It cannot check *where* the page went, and there is no way to make it: the message
  // names no URL, Location.prototype has no own `assign` to replace, `location`'s own
  // properties are non-configurable, and the page's `next` is built inside a lexical
  // scope. So the URL and its `next` parameter are unasserted here, deliberately. What
  // this file does cover is the pair of failures that actually happened — never probing
  // at all, and probing but ignoring the 401.
  vc.on("jsdomError", e => {
    if (/Not implemented: navigation/.test(e.message || "")) navigations.push("navigated");
  });

  const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    url: "http://127.0.0.1:8471/",
    virtualConsole: vc,
    beforeParse(win) {
      // A stream that fails the way an expired session fails: an error event and no
      // status code, because that is all EventSource ever gives.
      win.EventSource = class {
        constructor(url) {
          this.url = url;
          this.readyState = 1;
          this.listeners = {};
          setTimeout(() => { if (this.onerror) this.onerror({}); }, 10);
        }
        addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
        close() { this.readyState = 2; }
      };
      win.fetch = async url => {
        fetched.push(url);
        if (status === 401) {
          return {ok: false, status: 401, statusText: "Unauthorized",
            json: async () => ({error: "not logged in", login: "/login"}),
            text: async () => `{"error":"not logged in"}`};
        }
        return {ok: true, status: 200, statusText: "OK",
          json: async () => ({}), text: async () => "{}"};
      };
      win.matchMedia = () => ({matches: false, addEventListener() {}});
      win.HTMLElement.prototype.scrollIntoView = function () {};
      win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
      win.requestAnimationFrame = fn => setTimeout(fn, 0);
      win.getSelection = () => ({isCollapsed: true});
    },
  });

  await new Promise(r => dom.window.addEventListener("load", r));
  await new Promise(r => setTimeout(r, 250));
  return {fetched, navigations, win: dom.window};
}

console.log("1. the stream fails and the session is gone");
{
  const {fetched, navigations, win} = await run(401);

  if (!fetched.includes("/status")) {
    fail(`the page did not probe /status after the stream failed; it fetched ${
      JSON.stringify(fetched)}`);
  } else {
    ok("a stream error probes /status");
  }
  if (fetched.includes("/readyz")) {
    fail("the page probed /readyz, which answers without a login and cannot return 401");
  }

  if (navigations.length === 0) {
    fail("a 401 left the page where it was; nothing sent the reader to the login");
  } else {
    ok("a 401 navigates away");
  }

  // The header still has to say something, because the redirect is a navigation jsdom
  // refuses — in a browser the page is replaced, but a page that only redirected and
  // said nothing would look frozen for the moment before it does.
  const conn = win.document.getElementById("conn");
  if (conn && conn.textContent.trim() === "") {
    fail("the connection indicator went blank");
  }
}

console.log("2. the stream fails and the session is fine");
{
  const {fetched, navigations} = await run(200);
  if (!fetched.includes("/status")) {
    fail("the page did not probe at all");
  } else {
    ok("it still probes");
  }
  if (navigations.length > 0) {
    fail("a healthy session was navigated away from the dashboard");
  } else {
    ok("a reachable daemon with a good session stays put");
  }
}

console.log(failures === 0 ? "\nPASS" : `\n${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
