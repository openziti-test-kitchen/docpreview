// Drives the startup banner on the operations dashboard.
//
// # Why this exists
//
// `.wrap` is a two-column grid, and the banner must be a sibling of it, never a child.
// Prepending into `.wrap` would make the banner its first cell, push the previews list
// into the 21rem rail column, and give the activity rail the wide one — every row in the
// list overlapping itself in a third of the width it was built for. That reads as broken
// row CSS and is not: nothing in the markup or the row styles is wrong, so no amount of
// reading either of them finds it. A harness that asks "is the banner a child of the
// grid" does.
//
// It also covers what the banner says. Recovery takes minutes of zrok round trips, and a
// wait with no progress is indistinguishable from a hang, so the banner has to report
// visible progress rather than sit silent.
//
//   npm install --prefix tools/dashboardtest
//   node tools/dashboardtest/startup.mjs
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";
import {JSDOM, VirtualConsole} from "jsdom";

const here = dirname(fileURLToPath(import.meta.url));
const dashboard = join(here, "..", "..", "internal", "daemon", "dashboard.html");

let failures = 0;
const fail = msg => { failures++; console.log(`   FAIL: ${msg}`); };
const ok = msg => console.log(`   ok: ${msg}`);

// A daemon mid-recovery: one preview in the database, three shares still to republish,
// and a queued build that cannot start because no worker exists yet.
const starting = {
  exposer: "zrok2", instance: "test", pending: 1, running: 0,
  starting: true,
  startup: {stage: "restoring", note: "Republishing preview URLs from artifacts already on disk.",
            done: 1, total: 3},
  projects: [{key: "github:netfoundry/unified-doc", label: "Unified Doc", avatar: ""}],
  previews: [
    {preview_id: "aaa", repo: "github:netfoundry/unified-doc", number: 1, branch: "a",
     name: "", url: "", state: "queued",
     updated_at: new Date(Date.now() - 42_000).toISOString(), commit: "1111111"},
  ],
  events: [],
};

const vc = new VirtualConsole();
vc.on("jsdomError", e => fail(`page threw: ${e.message}`));

let status = starting;
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
    win.fetch = async url => {
      const body = url === "/status" ? status
        : url === "/api/admin" ? {secrets: true, projects: true} : {};
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
const doc = win.document;
await new Promise(r => win.addEventListener("load", r));
await new Promise(r => setTimeout(r, 200));

const $ = s => doc.querySelector(s);
const $$ = s => [...doc.querySelectorAll(s)];
const settle = () => new Promise(r => setTimeout(r, 80));

// applyStatus is what the stream calls; drive it directly rather than waiting on a poll.
const apply = async s => {
  status = s;
  win.eval(`applyStatus(${JSON.stringify(s)})`);
  await settle();
};

await apply(starting);

console.log("the banner is not a cell of the previews grid");
{
  const el = $("#starting");
  if (!el) {
    fail("no banner while the daemon reports starting");
  } else if (el.parentNode === $(".wrap")) {
    fail("the banner is a child of .wrap, so it takes the first grid cell and " +
         "pushes the previews list into the 21rem rail column");
  } else if (el.nextElementSibling !== $(".wrap")) {
    fail("the banner is not immediately above .wrap");
  } else {
    ok("a sibling directly above .wrap");
  }
}

console.log("\nthe page's own content stays on screen");
{
  // Hiding the page's content during recovery would leave a dashboard whose whole
  // visible contents are one notice, which reads as broken. The rows are true — they
  // come from the database.
  if ($(".wrap").hidden) {
    fail("the previews list and activity rail are hidden, leaving a page whose only " +
         "content is a banner");
  } else {
    ok(".wrap is still shown");
  }
}

console.log("\nnothing in the banner looks clickable");
{
  // The stages were bordered pills, which is what every button on this page looks
  // like, so three of them in a row were read as three broken buttons.
  const interactive = $$("#starting button, #starting a, #starting [role=button], #starting input");
  if (interactive.length) {
    fail(`${interactive.length} interactive element(s) in a banner that has no actions`);
  } else {
    ok("no buttons, links or controls");
  }
  // The spinner is the only moving thing in the banner and the clearest signal that the
  // stage is in progress. It was a ::before with a color-mix() border and rendered as
  // nothing at all, which is invisible in the markup and invisible in review.
  if (!$("#starting .step.now .spinner")) {
    fail("the current stage has no spinner, so which one is running is a matter of weight");
  } else {
    ok("the current stage carries a spinner");
  }
  if ($$("#starting .step:not(.now) .spinner").length) {
    fail("a stage that is not running has a spinner");
  }

  const styled = [...$("#starting").querySelectorAll(".step")].filter(s => {
    const cs = win.getComputedStyle(s);
    return cs.borderStyle && cs.borderStyle !== "none" && cs.borderWidth !== "0px";
  });
  if (styled.length) {
    fail(`${styled.length} stage label(s) are drawn with a border, which reads as a button`);
  } else {
    ok("stages are text, not boxes");
  }
}

console.log("\nwhat stage, and how far through");
{
  const el = $("#starting");
  const now = $("#starting .step.now");
  const done = $$("#starting .step.done").map(s => s.textContent.trim());
  if (!now) {
    fail("no stage is marked current");
  } else if (!/Restore previews/i.test(now.textContent)) {
    fail(`the current step is ${JSON.stringify(now.textContent.trim())}`);
  } else {
    ok(`current step "${now.textContent.trim()}", completed ${JSON.stringify(done)}`);
  }
  // Inside the current step, not trailing the row: "1 of 3" after three stage names was
  // read as counting the stages, of which there are three.
  const inStep = now && /1 of 3/.test(now.textContent);
  if (!inStep) {
    fail(`the count is not part of the current stage: ${JSON.stringify(el.textContent.trim())}`);
  } else {
    ok(`the current stage reads ${JSON.stringify(now.textContent.trim())}`);
  }
  if (!el.textContent.includes("Republishing preview URLs")) {
    fail("the daemon's own note is not shown");
  } else {
    ok("shows the note the daemon sent");
  }

  const bar = $("#starting .starting-bar span");
  const width = bar ? bar.style.width : "";
  if (width !== "33%") {
    fail(`the bar is ${JSON.stringify(width)}, want 33% for 1 of 3`);
  } else {
    ok("the bar is 33%");
  }
}

console.log("\nan uncounted stage does not fake a measurement");
{
  await apply({...starting,
    startup: {stage: "reaping", note: "Clearing shares left by the previous run.",
              done: 0, total: 0}});
  const bar = $("#starting .starting-bar");
  if (!bar.dataset.indeterminate) {
    fail("the reaping stage renders a determinate bar, which would sit at 0% and " +
         "read as stuck");
  } else {
    ok("indeterminate while reaping");
  }
  if (/\d+ of \d+/.test($("#starting").textContent)) {
    fail("the reaping stage invented a denominator");
  } else {
    ok("no count claimed");
  }
}

console.log("\nthe queued build's stopwatch is uniform and moving");
{
  // Back to a counted stage so the row is rendered with the list hidden but present.
  await apply(starting);
  const cell = $(".item .when");
  if (!cell) {
    fail("the queued row has no timestamp");
  } else if (!/^\d\dm \d\ds$/.test(cell.textContent.trim())) {
    fail(`the row reads ${JSON.stringify(cell.textContent.trim())}, want MMm SSs — ` +
         `"1m" stops changing for a whole minute, which is the one number being watched`);
  } else if (!cell.classList.contains("ticking")) {
    fail("the live row's timestamp is not marked as ticking");
  } else {
    ok(`reads ${JSON.stringify(cell.textContent.trim())} and is marked ticking`);
  }
}

console.log("\nit says what it is doing, not only how far along");
{
  await apply({...starting, startup: {
    stage: "restoring", note: "Republishing preview URLs from artifacts on disk.",
    done: 2, total: 13,
    items: ["clearing 1 share(s) nothing claims", "adopted docs-main (#7 main)",
            "adopted docs-main-3fc1a0d"],
  }});
  const lines = $$("#starting .starting-item").map(n => n.textContent.trim());
  if (!lines.length) {
    fail("no activity lines, so a four-minute wait says only 2 of 13");
  } else if (lines[0] !== "adopted docs-main-3fc1a0d") {
    fail(`newest line is ${JSON.stringify(lines[0])}, want the most recent first`);
  } else {
    ok(`${lines.length} lines, newest first: ${JSON.stringify(lines[0])}`);
  }
}

console.log("\nno startup dialog opens by itself");
{
  // Gating this on a marker in sessionStorage, so a reload would not re-announce an old
  // restart, would still open a dialog to dismiss for a startup that took three seconds
  // and went fine — right for a long recovery and wrong for a short one.
  await apply({...starting, starting: false, startup: undefined, pending: 0,
    last_startup: {
      seconds: 3, instance: "20260730-100000.000",
      previews: 2, adopted_previews: 2, created_previews: 0,
      adopted_builds: 11, created_builds: 0, orphans: 0, pending: 0,
    }});
  if (!$("#boot").hidden) fail("a startup opened a dialog nobody asked for");
  else ok("nothing over the page");

  // The footer link is how it is reached, and it carries the duration — a link reading
  // "startup report" makes somebody open a dialog for a number that fits in the link.
  const host = $("#boot-link");
  const btn = $("#boot-open");
  if (!host || host.hidden) fail("no footer link to the startup report");
  else if (!/3s/.test(btn.textContent)) {
    fail(`the link reads ${JSON.stringify(btn.textContent)}, without the duration`);
  } else ok(`footer reads ${JSON.stringify(btn.textContent.trim())}`);

  // And it opens on click, including for a quick, uneventful startup: somebody who
  // clicks wants the report either way.
  btn.dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
  if ($("#boot").hidden) fail("the footer link did not open the report");
  else ok("opens on click, even for a 3-second startup");

  $("#boot-close").dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
  if (!$("#boot").hidden) fail("Close did not close it");
  else ok("closes again");
}

console.log("\nstarted: the banner goes, the page comes back, the report is reachable");
{
  const report = {
    seconds: 74, instance: "20260730-090000.000",
    previews: 2, adopted_previews: 2, created_previews: 0,
    adopted_builds: 11, created_builds: 0, orphans: 1, pending: 0,
    items: ["adopted docs-main (#7 main)", "adopted docs-main-3fc1a0d"],
  };
  await apply({...starting, starting: false, startup: undefined, pending: 0,
               last_startup: report});

  if ($("#starting")) fail("the banner outlived recovery");
  else ok("banner removed");
  if ($(".wrap").hidden) {
    fail(".wrap is hidden on a started daemon, so the dashboard is blank");
  } else {
    ok(".wrap is shown");
  }

  // Opened from the footer, which is now the only way in.
  $("#boot-open").dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();

  const modal = $("#boot");
  if (!modal || modal.hidden) {
    fail("no startup report, so what happened during the wait is unreadable");
  } else {
    const body = $("#boot-body").textContent;
    // The duration must be labelled, not just present. It was rendered as a bare "3s"
    // with no caption and went unread — "i didn't see a total time to start".
    if (!/startup took/i.test(body)) {
      fail("the duration is unlabelled, so the report's headline number reads as decoration");
    } else if (!body.includes("1m 14s")) {
      fail("the duration is missing");
    } else {
      ok(`headline reads ${JSON.stringify($("#boot-hero-v")?.textContent ?? "")}`);
    }
    const checks = [["13", "the adopted count"], ["2", "the preview count"]];
    const missing = checks.filter(([v]) => !body.includes(v)).map(([, w]) => w);
    if (missing.length) fail(`the report omits ${missing.join(", ")}`);
    else ok("reports previews and adopted");
    if (!/none recreated/i.test(body)) {
      fail("an all-adopted startup does not say so, which is the number that matters");
    } else {
      ok("says none were recreated");
    }
    // Zero-valued facts are absent rather than rendered as "0".
    if (/Created/.test(body)) {
      fail("a row for 'Created' appears on a startup that created nothing");
    } else {
      ok("no row for what did not happen");
    }
    // The step list is collapsed: thirteen monospace lines would be the bulk of it.
    const det = $("#boot .boot-details");
    if (!det) {
      fail("no step list");
    } else if (det.open) {
      fail("the step list is expanded by default, which buries the counts");
    } else {
      ok("step list collapsed behind a summary");
    }
  }

  // Closed stays closed across a status tick. The daemon keeps reporting its startup for as
  // long as it runs, and a poll every five seconds must not reopen this over whatever is
  // being read — the failure that the old sessionStorage marker existed to prevent, and
  // which a re-render can reintroduce without one.
  $("#boot-close").dispatchEvent(new win.MouseEvent("click", {bubbles: true}));
  await settle();
  await apply({...starting, starting: false, startup: undefined, last_startup: report});
  if (!$("#boot").hidden) {
    fail("the report reopened on the next status");
  } else {
    ok("stays closed across a status tick");
  }
  // And the link is still there to open it again, rather than being spent on one use.
  if ($("#boot-link").hidden) fail("the footer link went away after the report was closed");
  else ok("still reachable from the footer");
}

console.log(failures ? `\n${failures} failure(s)` : `\nall startup-banner checks OK`);
process.exit(failures ? 1 : 0);
