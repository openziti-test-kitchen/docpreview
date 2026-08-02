// Polls /status and reports what the daemon actually sends while a build is queued.
//
// Checks whether the activity feed can show only the queued entry while a build waits,
// with the rest reappearing once it starts. The daemon always sends events.recent(60),
// and rendering a queued event into real history adds a row rather than removing
// fifteen, so this records the evidence instead of guessing at it — run it, push, and
// read what came back.
//
//   $env:DOCPREVIEW_PASSWORD = "<the admin password>"
//   node tools/dashboardtest/watchqueued.mjs
//
// It prints one line per change in (state set, event count) and shouts if the event
// count drops.
//
// The password is needed because /status is behind the login. Without it this exits
// rather than polling: a watcher that reports nothing because it is being refused is
// worse than one that does not start.
import {statusFetcher} from "./live.mjs";

const daemon = process.env.DOCPREVIEW_URL || "http://127.0.0.1:8471";
const status = statusFetcher(daemon);
const every = Number(process.env.INTERVAL_MS || 1500);

let lastKey = "";
let peak = 0;

const stamp = () => new Date().toISOString().slice(11, 19);

console.log(`watching ${daemon}/status every ${every}ms — push something, then read below`);

for (;;) {
  try {
    const s = await status();
    const events = s.events || [];
    const states = {};
    for (const p of s.previews || []) states[p.state] = (states[p.state] || 0) + 1;

    const kinds = {};
    for (const e of events) kinds[e.kind] = (kinds[e.kind] || 0) + 1;

    const key = JSON.stringify([states, events.length]);
    if (key !== lastKey) {
      lastKey = key;
      const busy = states.queued || states.building;
      console.log(`${stamp()}  previews[${Object.entries(states).map(([k, v]) => `${k}:${v}`).join(" ")}]` +
        `  events=${events.length}  [${Object.entries(kinds).map(([k, v]) => `${k}:${v}`).join(" ")}]` +
        (busy ? "  <-- in flight" : ""));

      // The claim being tested. The event log is a ring that only grows until it
      // wraps at 200, so a drop while a build is queued would be the bug, in the
      // daemon rather than in the page.
      if (events.length < peak) {
        console.log(`${stamp()}  *** EVENT COUNT DROPPED ${peak} -> ${events.length} ` +
          `while previews were [${Object.keys(states).join(" ")}] ***`);
      }
      peak = Math.max(peak, events.length);
    }
  } catch (e) {
    console.log(`${stamp()}  poll failed: ${e.message}`);
  }
  await new Promise(r => setTimeout(r, every));
}
