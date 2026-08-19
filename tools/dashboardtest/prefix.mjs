// The installation's hostname prefix, on the Settings page it moved to.
//
// # Why it moved
//
// It was on /projects, in the same row as the New project button, with its own Save button above a
// list of project cards — so it read as belonging to the first card. It belongs to no project: it
// changes the public hostname of every preview at once, which is what the Settings page is for.
//
// # What this asserts
//
//   - The field renders from its own endpoint, `GET /api/settings/prefix`, rather than from the
//     projects payload — that page has no reason to fetch every project row and its inlined badge
//     to draw one twelve-character input.
//   - The live example, because "a" tells nobody what they are about to get and `a-docs-main` is
//     the string that ends up in a URL.
//   - Saving PUTs to the settings route and says what saving does *not* do: nothing already
//     published is renamed.
//   - A read-only daemon shows the value and offers no Save, rather than a button that 403s.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node prefix.mjs
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

// load opens /secrets with a given prefix payload, and returns the window plus the calls made.
async function load({prefix, canWrite}) {
  const calls = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

  const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    url: "http://127.0.0.1:8471/secrets",
    virtualConsole: vc,
    beforeParse(win) {
      win.fetch = async (url, init) => {
        const method = init?.method || "GET";
        calls.push({method, url, body: init?.body ? JSON.parse(init.body) : null});

        if (url === "/api/settings/prefix") {
          const body = method === "PUT"
            ? {prefix: JSON.parse(init.body).prefix, can_write: canWrite}
            : {prefix, can_write: canWrite};
          return {ok: true, status: 200, statusText: "OK",
            json: async () => body, text: async () => JSON.stringify(body)};
        }
        // The credential and exposer panels are not under test. 404 is what a daemon without
        // them answers, and the page is required to carry on rather than break.
        return {ok: false, status: 404, statusText: "Not Found",
          text: async () => JSON.stringify({error: "404 not found"}),
          json: async () => ({error: "404 not found"})};
      };
      win.matchMedia = () => ({matches: false, addEventListener() {}});
      win.HTMLElement.prototype.scrollIntoView = function () {};
      win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
      win.requestAnimationFrame = fn => setTimeout(fn, 0);
      win.getSelection = () => ({isCollapsed: true});
      win.confirm = () => true;
    },
  });

  const win = dom.window;
  await new Promise(r => win.addEventListener("load", r));
  await new Promise(r => setTimeout(r, 350));
  return {win, doc: win.document, calls};
}

const settle = () => new Promise(r => setTimeout(r, 120));

console.log("the prefix is on the Settings page, from its own endpoint");
{
  const {doc, calls} = await load({prefix: "", canWrite: true});

  if (!calls.some(c => c.method === "GET" && c.url === "/api/settings/prefix")) {
    fail(`no GET /api/settings/prefix: ${JSON.stringify(calls.map(c => c.url))}`);
  } else {
    ok("read from GET /api/settings/prefix");
  }
  if (calls.some(c => c.url === "/api/projects")) {
    fail("the settings page fetched every project row to draw one input");
  } else {
    ok("no /api/projects fetch");
  }

  const box = doc.getElementById("p-prefix");
  if (!box) {
    fail("no prefix field on the settings page");
  } else {
    ok("the field is there");
    if (box.value !== "") fail(`it starts at ${JSON.stringify(box.value)}, want empty`);
  }
}

console.log("");
console.log("the example follows what is typed");
{
  const {doc} = await load({prefix: "", canWrite: true});
  const box = doc.getElementById("p-prefix");
  const eg = doc.getElementById("p-prefix-eg");

  if (!eg) {
    fail("no live example beside the field");
  } else {
    if (eg.textContent.trim() !== "docs-main") {
      fail(`empty shows ${JSON.stringify(eg.textContent.trim())}, want "docs-main"`);
    } else {
      ok("empty shows docs-main");
    }

    box.value = "a";
    box.dispatchEvent(new doc.defaultView.Event("input", {bubbles: true}));
    await settle();
    if (eg.textContent.trim() !== "a-docs-main") {
      fail(`typing "a" shows ${JSON.stringify(eg.textContent.trim())}, want "a-docs-main"`);
    } else {
      ok('typing "a" shows a-docs-main');
    }
  }
}

console.log("");
console.log("saving goes to the settings route and says what it does not do");
{
  const {doc, calls} = await load({prefix: "", canWrite: true});
  const box = doc.getElementById("p-prefix");
  box.value = "a";
  box.dispatchEvent(new doc.defaultView.Event("input", {bubbles: true}));

  calls.length = 0;
  doc.querySelector("[data-save-prefix]").dispatchEvent(
    new doc.defaultView.MouseEvent("click", {bubbles: true}));
  await new Promise(r => setTimeout(r, 350));

  const put = calls.find(c => c.method === "PUT" && c.url === "/api/settings/prefix");
  if (!put) {
    fail(`nothing was PUT: ${JSON.stringify(calls)}`);
  } else if (put.body?.prefix !== "a") {
    fail(`it sent ${JSON.stringify(put.body)}, want the field's value`);
  } else {
    ok("PUT /api/settings/prefix");
  }

  // Nothing already published is renamed. Somebody would otherwise find that out from a share
  // list a week later.
  const note = [...doc.querySelectorAll("#toasts .toast")]
    .find(t => t.textContent.includes("keep their names"));
  if (!note) {
    fail("saving did not say existing previews keep their names");
  } else {
    ok("says existing previews keep their names until rebuilt");
  }
}

console.log("");
console.log("read-only shows the value and offers no Save");
{
  const {doc} = await load({prefix: "a", canWrite: false});
  const box = doc.getElementById("p-prefix");

  if (!box) {
    fail("the value is hidden rather than shown read-only");
  } else if (box.value !== "a") {
    fail(`the field reads ${JSON.stringify(box.value)}, want "a"`);
  } else if (!box.disabled) {
    fail("the field is editable on a daemon that will refuse the write");
  } else {
    ok("the value is shown, the field disabled");
  }
  if (doc.querySelector("[data-save-prefix]")) {
    fail("a Save button is offered that would 403");
  } else {
    ok("no Save button");
  }
}

console.log("");
console.log(`${failures} failure(s)`);
process.exit(failures ? 1 : 0);
