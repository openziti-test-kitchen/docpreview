// Reports every DOM write the one-second render makes with a row open, and what it wrote to.
//
// A diagnostic rather than an assertion: the question it answers is "what is still touching the
// page when nothing changed", which is the question behind a text selection that will not survive
// a second and a dropdown that closes under the pointer.
//
// Run it after changing anything in the render path. Anything listed here happens once per second
// for as long as a row is open.
//
//   npm install --prefix tools/dashboardtest
//   cd tools/dashboardtest && node churn.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = process.env.DOCPREVIEW_DASHBOARD ||
  join(here, "..", "..", "internal", "daemon", "dashboard.html");

const PREVIEW = "abc123def456";
const PROJECT = "acme/docs";

// No logs, which is the state in the report: a preview whose log retention has passed.
let logs = [];

function statusPayload() {
  return {
    exposer: "test", instance: "harness-1", version: "v9.9.9 · abcdef123456",
    can_rebuild: true, pending: 0, running: 0,
    previews: [{
      preview_id: PREVIEW, repo: `github:${PROJECT}`, number: 20, branch: "feature/x",
      name: "acme-docs-feature-x", url: "https://example.invalid/", state: "failed",
      updated_at: "2026-08-04T10:57:02.000Z", commit: "old1234",
      pr_url: "https://example.invalid/pr/20",
    }],
    events: [{
      repo: `github:${PROJECT}`, preview_id: PREVIEW, number: 20, branch: "feature/x",
      at: "2026-08-04T10:57:02.000Z", kind: "failed", commit: "old1234",
      message: "build failed", openable: true,
    }],
  };
}

const vc = new VirtualConsole();
vc.on("jsdomError", e => console.log(`page threw: ${e.message}`));

const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously",
  url: "http://127.0.0.1:8471/",
  virtualConsole: vc,
  beforeParse(win) {
    win.EventSource = class {
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.listeners = {};
        if (!/^\/logs\/.+\/stream$/.test(url)) return;
        setTimeout(() => {
          if (this.readyState !== 1) return;
          this.emit("done", JSON.stringify({reason: "no build log", live: false}));
        }, 10);
      }
      emit(type, data) { for (const fn of this.listeners[type] || []) fn({data}); }
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
      close() { this.readyState = 2; }
    };
    win.fetch = async url => {
      const body =
        url === "/status" ? statusPayload() :
        url === `/logs/${PREVIEW}` ? {preview_id: PREVIEW, live: false, logs} :
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
await new Promise(r => setTimeout(r, 200));

const settle = (ms = 150) => new Promise(r => setTimeout(r, ms));
const applyStatus = async () => {
  win.eval(`applyStatus(${JSON.stringify(statusPayload())})`);
  await settle();
};

// Describe an element the way somebody reading the page would recognise it.
const describe = el => {
  const role = el.getAttribute && el.getAttribute("data-role");
  const cls = el.className && typeof el.className === "string" ? el.className : "";
  return [el.tagName.toLowerCase(), role ? `[${role}]` : "", cls ? `.${cls.split(" ")[0]}` : ""]
    .join("");
};

let counting = false;
const writes = new Map();
const note = (el, prop, changed) => {
  if (!counting) return;
  const key = `${describe(el)} ${prop} ${changed ? "CHANGED" : "same-value"}`;
  writes.set(key, (writes.get(key) || 0) + 1);
};

for (const [proto, prop] of [
  [win.Element.prototype, "innerHTML"],
  [win.Node.prototype, "textContent"],
]) {
  const desc = Object.getOwnPropertyDescriptor(proto, prop);
  if (!desc || !desc.set) continue;
  Object.defineProperty(proto, prop, {
    configurable: true,
    get() { return desc.get.call(this); },
    set(v) {
      const before = desc.get.call(this);
      note(this, prop, before !== String(v));
      desc.set.call(this, v);
    },
  });
}

await applyStatus();
win.document.querySelector(".item .head").click();
await settle(400);

console.log("three one-second renders with a row open on a preview that has no log\n");
counting = true;
for (let i = 0; i < 3; i++) {
  win.eval("renderList()");
  await settle(120);
}
counting = false;

if (writes.size === 0) {
  console.log("   nothing written — the render touched no text or markup");
} else {
  for (const [key, n] of [...writes.entries()].sort((a, b) => b[1] - a[1])) {
    console.log(`   ${String(n).padStart(2)}x  ${key}`);
  }
}
console.log("\n'same-value' writes are pure churn: they collapse a selection and reset a control");
console.log("for no change. 'CHANGED' writes may be legitimate — a clock, a state, a new line.");
