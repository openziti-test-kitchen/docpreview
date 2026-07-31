// Fetching /status from a running daemon, now that /status needs a login.
//
// # Why this exists
//
// Three harnesses prefer live state to the fixture beside them, for a good reason: the
// fixture cannot contain the history shape nobody thought to invent. Once the dashboard
// went behind a login they all broke the same way and none of them said so — the fetch
// succeeded with a 401, `await res.json()` parsed `{"error":"not logged in"}`, and the
// page rendered against a status object with no `previews` key. The failure surfaced as
// `Cannot read properties of undefined (reading 'filter')` inside the page, which reads
// like a bug in the page.
//
// So: one place that knows how to log in, and that is explicit about which state it
// returned.
//
// # The password
//
// From DOCPREVIEW_PASSWORD, and nowhere else. Not a default, not a prompt: a harness
// that guessed would be a password guesser pointed at the operator's own daemon, and one
// that prompted could not run in a script. Without it, live state is simply unavailable
// and the fixture is used — which is the same behaviour as no daemon at all, and is
// stated on stdout either way.
//
//   $env:DOCPREVIEW_PASSWORD = "..."   # PowerShell
//   DOCPREVIEW_PASSWORD=...            # bash
import {readFileSync} from "node:fs";
import {dirname, join} from "node:path";
import {fileURLToPath} from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

// loginCookie authenticates against the daemon and returns the Set-Cookie value to
// resend, or "" when it could not.
//
// The role is admin because two of the three harnesses drive the admin controls, and a
// viewer session renders a page with those controls absent — which would read as a
// missing feature rather than as a session with fewer rights.
async function loginCookie(daemon, password) {
  const body = new URLSearchParams({username: "admin", password});
  const res = await fetch(`${daemon}/login`, {
    method: "POST",
    headers: {"Content-Type": "application/x-www-form-urlencoded"},
    body,
    // The daemon answers a good login with a 303 to the dashboard. Following it would
    // discard the Set-Cookie header this function exists to read.
    redirect: "manual",
  });
  const raw = res.headers.getSetCookie?.() || [];
  for (const c of raw) {
    if (c.startsWith("docpreview_session=")) return c.split(";")[0];
  }
  return "";
}

// statusFetcher returns a function that fetches /status repeatedly, holding one session.
//
// For the pollers. liveStatus logs in on every call, which is right for a one-shot and
// wrong at one call per second and a half — and a session re-established on a 401 rather
// than pre-emptively also survives the restart that invalidates it, since the cookie is
// signed with a secret the daemon holds in memory.
//
// Throws when it cannot get state, because a poller has nothing to fall back to: a
// fixture that never changes is the opposite of what it is watching for.
export function statusFetcher(daemon) {
  let cookie = "";
  const get = () => fetch(`${daemon}/status`,
    {headers: cookie ? {"Accept": "application/json", cookie} : {"Accept": "application/json"}});

  return async function status() {
    let res = await get();
    if (res.status === 401) {
      const password = process.env.DOCPREVIEW_PASSWORD;
      if (!password) {
        throw new Error("the daemon wants a login; set DOCPREVIEW_PASSWORD to the admin password");
      }
      cookie = await loginCookie(daemon, password);
      if (!cookie) throw new Error("DOCPREVIEW_PASSWORD was refused");
      res = await get();
    }
    if (!res.ok) throw new Error(`the daemon answered ${res.status}`);
    return res.json();
  };
}

// liveStatus returns {status, source}, where source is "live", "fixture" or
// "fixture (login required)" — named rather than implied, because a harness reporting
// live state while running on the fixture is how a passing run means nothing.
export async function liveStatus(daemon) {
  const fixture = () => JSON.parse(readFileSync(join(here, "status.fixture.json"), "utf8"));

  let res;
  try {
    res = await fetch(`${daemon}/status`, {headers: {"Accept": "application/json"}});
  } catch {
    return {status: fixture(), source: `fixture (no daemon on ${daemon})`};
  }

  if (res.status === 401) {
    const password = process.env.DOCPREVIEW_PASSWORD;
    if (!password) {
      return {status: fixture(), source: "fixture (login required; set DOCPREVIEW_PASSWORD)"};
    }
    const cookie = await loginCookie(daemon, password);
    if (!cookie) {
      return {status: fixture(), source: "fixture (DOCPREVIEW_PASSWORD was refused)"};
    }
    res = await fetch(`${daemon}/status`, {headers: {"Accept": "application/json", cookie}});
  }

  // Anything other than a 200 here is not state. Checked because the original bug was
  // exactly this: a non-200 body parsed as JSON and handed on as though it were status.
  if (!res.ok) {
    return {status: fixture(), source: `fixture (the daemon answered ${res.status})`};
  }
  const status = await res.json();
  if (!Array.isArray(status.previews)) {
    return {status: fixture(), source: "fixture (the daemon's answer carried no previews)"};
  }
  return {status, source: `live, from ${daemon}`};
}
