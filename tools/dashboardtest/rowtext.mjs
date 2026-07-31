// Prints the rendered text of the first few activity rows, to see what a reader sees
// rather than what the markup intends.
//
// Needs DOCPREVIEW_PASSWORD, since /status is behind the login and this file is only
// useful against real state.
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

import {statusFetcher} from "./live.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const daemon = process.env.DOCPREVIEW_URL || "http://127.0.0.1:8471";
const status = await statusFetcher(daemon)();

const vc = new VirtualConsole();
vc.on("jsdomError", e => console.log("PAGE ERROR:", e.message));

// Resolved from this file rather than from the working directory, which made it runnable
// only from the repository root and only by somebody who knew that.
const dom = new JSDOM(readFileSync(
  join(here, "..", "..", "internal", "daemon", "dashboard.html"), "utf8"), {
  runScripts: "dangerously", url: `${daemon}/`, virtualConsole: vc,
  beforeParse(win) {
    win.EventSource = class { constructor() { this.readyState = 1; } close() {} addEventListener() {} };
    win.fetch = async () => ({ok: true, status: 200, statusText: "OK",
      json: async () => ({logs: []}), text: async () => "{}"});
    win.matchMedia = () => ({matches: false, addEventListener() {}});
    win.HTMLElement.prototype.scrollIntoView = function () {};
    win.CSS = {escape: s => String(s).replace(/[^\w-]/g, c => "\\" + c)};
    win.requestAnimationFrame = fn => setTimeout(fn, 0);
  },
});

const win = dom.window;
await new Promise(r => win.addEventListener("load", r));
const ui = win.eval("ui");
ui.previews = status.previews;
ui.events = status.events;
ui.status = status;
win.eval("render()");

for (const row of [...win.document.querySelectorAll("#events .ev")].slice(0, 8)) {
  const t = row.querySelector(".t").textContent.trim();
  const k = row.querySelector(".k").textContent.trim();
  const who = row.querySelectorAll(".l2")[0].textContent.trim();
  const detail = row.querySelectorAll(".l2")[1].textContent.trim();
  console.log(`${t}  ${k.padEnd(9)} ${who.padEnd(14)} ${detail}`);
}
