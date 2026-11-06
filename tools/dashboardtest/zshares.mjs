// Drives the zrok share queue in the dashboard's footer.
//
// zrok limits how fast an account creates shares, so the daemon queues share creations and lets
// fifteen through per ten minutes. A preview waiting in that queue looks exactly like a stuck one
// unless the page says so: how much of the window is spent, how many are waiting, and when the
// next can go.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/zshares.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

const iso = ms => new Date(Date.now() + ms).toISOString();
const base = {exposer: "zrok2", instance: "test", pending: 0, running: 0, starting: false,
  projects: [], previews: [], events: []};

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));
let status = base;
const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
  runScripts: "dangerously", url: "http://127.0.0.1:8471/", virtualConsole: vc, pretendToBeVisual: true,
  beforeParse(win) {
    win.EventSource = class { constructor() { this.readyState = 1; } addEventListener() {} close() {} };
    win.fetch = async url => {
      const body = url === "/status" ? status : url === "/api/admin" ? {secrets: true, projects: true} : {};
      return {ok: true, status: 200, statusText: "OK", json: async () => body, text: async () => JSON.stringify(body)};
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
await new Promise(r => setTimeout(r, 200));
const apply = async s => { status = s; win.eval(`applyStatus(${JSON.stringify(s)})`); await new Promise(r => setTimeout(r, 80)); };
const $ = s => doc.querySelector(s);

console.log("no gate, no link");
await apply(base);
if (!$("#zshares-link").hidden) fail("the link shows with no zrok_shares in /status");
else ok("hidden");

console.log("\na full window with two queued says so in the footer");
const full = {...base, zrok_shares: {
  limit: 15, window_seconds: 600, used: 15,
  window_start: iso(-300_000), window_closes: iso(300_000), next_slot: iso(300_000),
  queued: [{name: "a-docs-pr-7", reason: "publish", since: iso(-20_000)},
           {name: "a-docs-main", reason: "publish", since: iso(-10_000)}],
  recent: [{at: iso(-60_000), process: "daemon", name: "a-x", reason: "restore", outcome: "rate-limited"},
           {at: iso(-90_000), process: "daemon", name: "a-y", reason: "publish", outcome: "created"}],
}};
await apply(full);
{
  const btn = $("#zshares-open");
  const t = btn ? btn.textContent : "";
  if ($("#zshares-link").hidden) fail("the link is hidden");
  if (!t.includes("15/15") || !t.includes("2 queued") || !t.includes("next ")) {
    fail(`footer label = ${JSON.stringify(t)}, want used/limit, queued count and next slot`);
  } else ok(`label: ${t}`);
  if (!btn.classList.contains("warn")) fail("a queue is not marked warn");
  else ok("marked warn");
}

console.log("\nthe dialog lists the queue and the attempts");
win.eval(`document.getElementById("zshares-open").click()`);
await new Promise(r => setTimeout(r, 50));
{
  const body = $("#zshares-body").textContent;
  if ($("#zshares").hidden) fail("the dialog did not open");
  if (!body.includes("a-docs-pr-7") || !body.includes("a-docs-main")) fail("queued shares are not listed");
  else ok("queued shares listed");
  if (!body.includes("rate-limited") || !$("#zshares-body .bad")) fail("a rate-limited attempt is not marked");
  else ok("rate-limited attempt marked");
  if (!body.includes("15 of 15")) fail("the window's use is not shown");
  else ok("window use shown");
}

console.log("\nthe open dialog follows the next poll");
await apply({...base, zrok_shares: {...full.zrok_shares, used: 3, queued: [], next_slot: iso(0),
  window_start: iso(-1000), window_closes: iso(599_000)}});
{
  const body = $("#zshares-body").textContent;
  if (!body.includes("3 of 15")) fail(`dialog not refreshed: ${body.slice(0, 120)}`);
  else ok("refreshed");
  if ($("#zshares-open").classList.contains("warn")) fail("still warn with nothing queued");
}

console.log("\nthe quiet period is named");
await apply({...base, zrok_shares: {limit: 15, window_seconds: 600, used: 0,
  quiet_until: iso(400_000), next_slot: iso(400_000), queued: [], recent: []}});
if (!$("#zshares-open").textContent.includes("quiet until")) fail("the footer does not say quiet");
else ok("quiet named");

console.log(failures ? `\n${failures} failure(s)` : "\nall passed");
process.exit(failures ? 1 : 0);
