// The Bitbucket credential box on a project's edit page: Save, Delete, and Test credential.
//
// # Why this exists
//
// Three things went wrong in that box in one sitting, and none of them is visible without
// clicking it:
//
//   - Test credential answered in a toast at the top of the window while the box sits at the
//     bottom of a long form, so a passing test read as a dead button.
//   - Saving a token turned the section's header chip green and left the row itself saying
//     "not set" over a placeholder saying "paste to set". The token was stored; the row lied.
//   - Delete went through the project list's renderer, which on the edit page drew the list
//     into its hidden container and left the form exactly as it was.
//
// And the same failure one step over: a refused Build it was a neutral toast in the far corner,
// gone in five seconds, so "context canceled" read as nothing having happened.
//
// # What it asserts
//
// The test panel appears inside the box the moment the button is pressed, with three rows each
// spinning; it settles each row to pass, fail or not attempted; it shows the masked header the
// server sent and nothing else; and the row's chip, placeholder and Delete button follow a save
// and a delete without the form being rebuilt. A refused Build it leaves a red note under its
// row; a failure toast stays until closed while a success toast leaves on its own.
//
// # Running it
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/credpanel.mjs
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

const KEY = "bitbucket/netfoundry/k8s-on-prem-installations";
const PATH = "/projects/" + KEY;

// One private Bitbucket project. `scm` is mutable: the fake server keeps it, so a save and a
// delete change what the next response says, as the real one does.
function server({scm = [], testAnswer, branchAnswer, scanAnswer}) {
  const st = {scm: [...scm]};
  const payload = () => ({
    can_write: true, read_only_why: "", secrets_available: true, vault_locked: false,
    global_secrets: [],
    defaults: {
      driver: "docker", image: "node:24-bookworm-slim", docker_available: true,
      allow_local_driver: false, images: ["node:24-bookworm-slim"],
      frameworks: [{id: "", label: "None — the repository decides"}],
      framework: "", timeout: "15m0s", scm_global: [],
    },
    projects: [{
      platform: "bitbucket", owner: "netfoundry", repo: "k8s-on-prem-installations",
      enabled: true, driver: "", build_dir: "", build_command: "", build_output: "",
      base_url: "", detect_script: "", image: "", notes: "", secrets: [],
      private: true, ...(st.scm.length ? {scm: [...st.scm]} : {}),
    }],
  });
  let release;
  const gate = {wait: null};
  return {
    st,
    hold() { gate.wait = new Promise(r => { release = r; }); },
    release() { release && release(); },
    async handle(method, url, body) {
      if (url === "/api/admin") return [200, {secrets: true, projects: true}];
      if (url.endsWith("/branch") && branchAnswer) return branchAnswer();
      if (url.endsWith("/scan")) {
        st.scans = (st.scans || 0) + 1;
        return scanAnswer ? scanAnswer() : [200, {queued: 2}];
      }
      if (url.endsWith("/scm-test")) {
        if (gate.wait) await gate.wait;
        return testAnswer();
      }
      const m = url.match(/\/scm\/([^/]+)$/);
      if (m && method === "PUT") {
        const name = decodeURIComponent(m[1]);
        st.scm = st.scm.filter(n => n !== name);
        if (body && body.value) st.scm.push(name);
        return [200, payload()];
      }
      return [200, payload()];
    },
  };
}

async function open(srv) {
  const calls = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", e => {
    if (/Not implemented: navigation/.test(e.message)) return;
    fail(`page threw: ${e.message}`);
  });
  const dom = new JSDOM(readFileSync(dashboard, "utf8"), {
    runScripts: "dangerously",
    url: `http://127.0.0.1:8471${PATH}`,
    virtualConsole: vc,
    pretendToBeVisual: true,
    beforeParse(win) {
      win.fetch = async (url, init) => {
        const method = init?.method || "GET";
        const body = init?.body ? JSON.parse(init.body) : null;
        calls.push({method, url, body});
        const [status, out] = await srv.handle(method, url, body);
        return {ok: status < 300, status, statusText: String(status),
          json: async () => out, text: async () => JSON.stringify(out)};
      };
      win.matchMedia = () => ({matches: false, addEventListener() {}});
      win.HTMLElement.prototype.scrollIntoView = function () {};
      win.CSS = {escape: s => String(s).replace(/[^\w-]/g, ch => "\\" + ch)};
      win.requestAnimationFrame = fn => setTimeout(fn, 0);
      win.getSelection = () => ({isCollapsed: true, removeAllRanges() {}, addRange() {}});
      win.confirm = () => true;
      win.alert = () => {};
    },
  });
  const win = dom.window;
  await new Promise(r => win.addEventListener("load", r));
  await new Promise(r => setTimeout(r, 300));
  return {win, doc: win.document, calls};
}

const click = (doc, el) => el.dispatchEvent(new doc.defaultView.MouseEvent("click", {bubbles: true}));
const wait = ms => new Promise(r => setTimeout(r, ms));

const passing = () => [200, {
  ok: true,
  credential: {source: "this project's own access token",
               header: "Authorization: Bearer ••••••9f2c"},
  checks: [
    {name: "Repository read", outcome: "pass", message: "answered: private",
     request: "GET https://api.bitbucket.org/2.0/repositories/netfoundry/k8s-on-prem-installations"},
    {name: "Pull request read", outcome: "pass", message: "Answered with 2 open pull request(s)",
     request: "GET https://api.bitbucket.org/2.0/repositories/netfoundry/k8s-on-prem-installations/pullrequests?state=OPEN&pagelen=1"},
    {name: "Pull request write", outcome: "not_attempted", request: "",
     message: "Not attempted — cannot be checked without writing to a pull request."},
  ],
}];

console.log("Test credential answers inside the credential box, with a spinner per step");
{
  const srv = server({scm: ["scm.access_token"], testAnswer: passing});
  const {doc} = await open(srv);
  const btn = doc.querySelector("[data-test-scm]");
  const host = doc.querySelector(".credbox [data-ctest-host]");
  if (!btn || !host) {
    fail("no Test button, or no panel host inside .credbox");
  } else {
    srv.hold();
    click(doc, btn);
    await wait(50);
    const rows = host.querySelectorAll(".ctest-step");
    if (rows.length !== 3) fail(`${rows.length} rows while running, want 3`);
    else ok("three rows the moment the button is pressed");
    if (host.querySelectorAll(".ctest-step .spinner").length !== 3) {
      fail("not every row has its own spinner while the check runs");
    } else ok("each row spins");
    const names = [...rows].map(r => r.querySelector(".ctest-name")?.textContent || "");
    for (const want of ["Repository read", "Pull request read", "Pull request write"]) {
      if (!names.some(n => n.includes(want))) fail(`no "${want}" row`);
    }
    if (!host.textContent.includes("/2.0/repositories/netfoundry/k8s-on-prem-installations")) {
      fail("the running panel does not name the request");
    } else ok("the requests are named before any answer");
    if (!btn.disabled) fail("the button is not disabled while the check runs");

    srv.release();
    await wait(700);
    const settled = host.querySelectorAll(".ctest-step");
    const classes = [...settled].map(r => r.className);
    if (!classes[0]?.includes("pass") || !classes[1]?.includes("pass") ||
        !classes[2]?.includes("not_attempted")) {
      fail(`rows settled to ${JSON.stringify(classes)}`);
    } else ok("pass, pass, not attempted");
    if (host.querySelector(".spinner")) fail("a spinner is still running after the answer");
    if (!host.querySelector(".ctest.good")) fail("the panel is not marked as passing");
    if (!host.textContent.includes("Authorization: Bearer ••••••9f2c") ||
        !host.textContent.includes("this project's own access token")) {
      fail("the masked header or its source is missing");
    } else ok("shows the source and the masked header");
    if (/pass/i.test(settled[2]?.querySelector(".ctest-out")?.textContent || "")) {
      fail("the write step reads as a pass");
    }
    if (btn.disabled) fail("the button stayed disabled");
    if (doc.querySelector("#toasts .toast")) fail("the answer was also toasted");
    else ok("no toast");
  }
}

console.log("");
console.log("a refused step shows Bitbucket's own message on that row");
{
  const srv = server({scm: ["scm.access_token"], testAnswer: () => [200, {
    ok: false,
    credential: {source: "this project's own access token", header: "Authorization: Bearer ••••••9f2c"},
    checks: [
      {name: "Repository read", outcome: "pass", message: "fine", request: "GET a"},
      {name: "Pull request read", outcome: "fail", request: "GET b",
       message: "bitbucket returned 403: Your credentials lack one or more required privilege scopes."},
      {name: "Pull request write", outcome: "not_attempted", request: "", message: "Not attempted"},
    ],
  }]});
  const {doc} = await open(srv);
  click(doc, doc.querySelector("[data-test-scm]"));
  await wait(700);
  const host = doc.querySelector("[data-ctest-host]");
  const bad = host.querySelector(".ctest-step.fail");
  if (!bad || !bad.textContent.includes("lack one or more required privilege scopes")) {
    fail("the failed row does not carry the platform's message");
  } else ok("the failed row carries the platform's message");
  if (!host.querySelector(".ctest.bad")) fail("the panel is not marked as failed");
}

console.log("");
console.log("a check that cannot start fails its first row rather than spinning forever");
{
  const srv = server({scm: [], testAnswer: () => [400,
    {error: "no Bitbucket credential for netfoundry/k8s-on-prem-installations"}]});
  const {doc} = await open(srv);
  click(doc, doc.querySelector("[data-test-scm]"));
  await wait(700);
  const host = doc.querySelector("[data-ctest-host]");
  const rows = [...host.querySelectorAll(".ctest-step")].map(r => r.className);
  if (!rows[0]?.includes("fail") || !rows[1]?.includes("not_attempted") ||
      !rows[2]?.includes("not_attempted")) {
    fail(`rows settled to ${JSON.stringify(rows)}`);
  } else ok("fail, not attempted, not attempted");
  if (!host.textContent.includes("no Bitbucket credential")) fail("the reason is not shown");
  if (host.querySelector(".spinner")) fail("still spinning");
}

console.log("");
console.log("saving a token updates the row, not just the header");
{
  const srv = server({scm: [], testAnswer: passing});
  const {doc} = await open(srv);
  const row = doc.querySelector('[data-scmrow="scm.access_token"]');
  const input = row?.querySelector("input");
  if (!row || !input) {
    fail("no access-token row");
  } else {
    if (!row.textContent.includes("not set")) fail("an unset row does not say so");
    // Leave a test result up, so the save can be seen clearing it.
    click(doc, doc.querySelector("[data-test-scm]"));
    await wait(700);
    input.value = "ATCTT3x-a-pasted-token-value-1234";
    click(doc, row.querySelector("[data-set-scm]"));
    await wait(200);
    const flag = row.querySelector("[data-scmrow-flag]")?.textContent.trim();
    if (flag !== "set") fail(`row chip after save = ${JSON.stringify(flag)}, want "set"`);
    else ok("the row says set");
    if (input.placeholder !== "paste to replace") {
      fail(`placeholder after save = ${JSON.stringify(input.placeholder)}`);
    } else ok("the placeholder says replace");
    if (input.value !== "") fail("the pasted token was left in the box");
    if (!row.querySelector("[data-clear-scm]")) fail("no Delete after a first save");
    else ok("Delete appears");
    if (doc.querySelector("[data-ctest-host]")?.textContent.trim()) {
      fail("the old test result survived replacing the token");
    } else ok("the stale test result is cleared");
    const head = doc.getElementById("scm-flag")?.textContent.trim();
    if (head !== "set") fail(`header chip = ${JSON.stringify(head)}`);

    console.log("");
    console.log("deleting it updates the row back, in place");
    click(doc, row.querySelector("[data-clear-scm]"));
    await wait(200);
    const after = doc.querySelector('[data-scmrow="scm.access_token"]');
    if (after !== row) fail("the form was rebuilt rather than updated in place");
    const flag2 = row.querySelector("[data-scmrow-flag]")?.textContent.trim();
    if (flag2 !== "not set") fail(`row chip after delete = ${JSON.stringify(flag2)}`);
    else ok("the row says not set");
    if (input.placeholder !== "paste to set") fail("the placeholder did not follow the delete");
    if (row.querySelector("[data-clear-scm]")) fail("Delete is still offered");
    else ok("Delete is gone");
    if (srv.st.scm.length) fail("the fake server still holds the token; nothing was sent");
    const head2 = doc.getElementById("scm-flag")?.textContent.trim();
    if (head2 !== "missing") fail(`header chip after delete = ${JSON.stringify(head2)}`);
    else ok("the header says missing");
  }
}

console.log("");
console.log("a refused Build it stays beside the button, red, until dismissed");
{
  const srv = server({scm: ["scm.access_token"], testAnswer: passing, branchAnswer: () => [200, {
    queued: false,
    error: "reading branch main on bitbucket:netfoundry/k8s-on-prem-installations: context canceled",
  }]});
  const {doc} = await open(srv);
  const btn = doc.querySelector(".pedit-head [data-branch]");
  if (!btn) {
    fail("no Build it button in the edit page's header");
  } else {
    click(doc, btn);
    await wait(150);
    const note = doc.querySelector(".pedit-head + .actnote.bad");
    if (!note || !note.textContent.includes("Nothing queued: reading branch main")) {
      fail(`no red note under the header: ${note ? note.textContent : "(none)"}`);
    } else ok("the refusal is under the row the button is in");
    if (btn.disabled) fail("the button stayed disabled");
    // Pressed again, the note is replaced rather than stacked.
    click(doc, btn);
    await wait(150);
    const n = doc.querySelectorAll(".actnote").length;
    if (n !== 1) fail(`${n} notes after two presses, want 1`);
    else ok("a second press replaces the note");
    doc.querySelector(".actnote button")?.dispatchEvent(
      new doc.defaultView.MouseEvent("click", {bubbles: true}));
    if (doc.querySelector(".actnote")) fail("the note did not dismiss");
    else ok("dismissable");
  }
}

console.log("");
console.log("Scan pull requests answers under the header, with the count");
{
  const srv = server({scm: ["scm.access_token"], testAnswer: passing,
    scanAnswer: () => [200, {queued: 1, error: "pull request 14: the queue is closed"}]});
  const {doc} = await open(srv);
  const btn = doc.querySelector(".pedit-head [data-scan]");
  if (!btn) {
    fail("no Scan pull requests button on the edit page");
  } else {
    click(doc, btn);
    await wait(150);
    const note = doc.querySelector(".pedit-head + .actnote.bad");
    if (!note || !note.textContent.includes("Queued 1 build, but not every open pull request") ||
        !note.textContent.includes("pull request 14")) {
      fail(`partial scan reported as ${note ? JSON.stringify(note.textContent) : "nothing"}`);
    } else ok("a partial scan says how many and which failed");
  }
}

console.log("");
console.log("the first usable token scans on its own; a rotation does not");
{
  const srv = server({scm: [], testAnswer: passing});
  const {doc} = await open(srv);
  const row = doc.querySelector('[data-scmrow="scm.access_token"]');
  row.querySelector("input").value = "ATCTT3x-the-first-token-0001";
  click(doc, row.querySelector("[data-set-scm]"));
  await wait(250);
  if (srv.st.scans !== 1) fail(`${srv.st.scans || 0} scans after the first token, want 1`);
  else ok("scanned once");
  const host = doc.querySelector(".credbox [data-scan-host]");
  if (!host || !host.textContent.includes("Queued 2 builds, one per open pull request")) {
    fail(`scan result in the credential box: ${host ? JSON.stringify(host.textContent) : "(no host)"}`);
  } else ok("the result is in the credential box");
  if (host && host.querySelector(".spinner")) fail("the scan note is still spinning");

  row.querySelector("input").value = "ATCTT3x-the-rotated-token-0002";
  click(doc, row.querySelector("[data-set-scm]"));
  await wait(250);
  if (srv.st.scans !== 1) fail("rotating the token scanned again, rebuilding every open pull request");
  else ok("a rotation does not scan");
}

console.log("");
console.log("a failure toast stays; a success toast goes on its own");
{
  const srv = server({scm: [], testAnswer: passing});
  const {doc, win} = await open(srv);
  win.eval(`toast("a failure that names a scope", "bad"); toast("a success", "good")`);
  const host = doc.getElementById("toasts");
  const cs = win.getComputedStyle(host);
  if (cs.bottom === "auto" || cs.bottom === "" || (cs.top && cs.top !== "auto")) {
    fail(`toasts are anchored top=${cs.top} bottom=${cs.bottom}, want the bottom edge`);
  } else ok("anchored at the bottom");
  if (!doc.querySelector("#toasts .toast.bad .toast-x")) fail("a failure toast has no close button");
  if (doc.querySelector("#toasts .toast.good .toast-x")) fail("a success toast has a close button");
  await wait(6600);
  if (!doc.querySelector("#toasts .toast.bad")) fail("the failure toast timed out");
  else ok("the failure is still there after the success has gone");
  if (doc.querySelector("#toasts .toast.good")) fail("the success toast never left");
  doc.querySelector("#toasts .toast.bad .toast-x").dispatchEvent(
    new doc.defaultView.MouseEvent("click", {bubbles: true}));
  await wait(300);
  if (doc.querySelector("#toasts .toast.bad")) fail("the close button did not close it");
  else ok("and closes when asked");
}

console.log("");
if (failures) {
  console.log(`${failures} failure(s)`);
  process.exit(1);
}
console.log("all passed");
