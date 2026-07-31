package daemon

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/expose"
)

// The login surface: a middleware over the whole ingress, and the two routes it needs.
//
// # What is protected, and what deliberately is not
//
// Everything the dashboard serves — the page, `/status`, `/events`, every build log, `/pr` —
// requires at least a viewer session once a viewer password is set. Four things do not, and each
// exemption is load-bearing rather than convenience:
//
//   - **`/webhook/*`** is authenticated already, by an HMAC over the body, and its callers are
//     GitHub and Bitbucket. A cookie gate here would break every delivery and the failure would
//     read as a signature problem.
//   - **`/healthz`** is what a supervisor or a container healthcheck polls. Gating it means a
//     password change silently restarts the daemon in a loop.
//   - **`/readyz`** is what a restart script waits on. Gating it turned a restart into a wait
//     loop that never returned, because the only payload reporting recovery was `/status`. It
//     carries counts and a stage name and nothing that identifies a repository or a preview —
//     see the note on it in ingress.go, which is the rule for anything added to it.
//   - **The previews themselves** — `expose.MountPrefix`, under the `local` exposer. A preview
//     URL is meant to be pasted into a pull request and opened by a reviewer who has no login
//     and should not need one. Previews served by zrok, Frontdoor or ziti never reach this mux
//     at all, so this exemption only makes the local exposer behave like the others.
//   - **`/login` and `/logout`**, for the obvious reason.
//
// # Loopback is not exempt
//
// It was, briefly: a request from the machine itself was admitted as admin with no session, on
// the reasoning that anyone who can reach `127.0.0.1` can already run the binary. That is true
// and it is still the wrong rule, because "the machine itself" is not a property this daemon can
// establish. A tunnel in proxy mode connects from a local process, so `RemoteAddr` is loopback
// for a request that came from the internet — the same hole documented at length for the
// credential surface, which is the reason `isLocalRequest` also demands the absence of
// forwarding headers. Betting the whole login on it makes every visitor an admin the moment
// something proxies without setting one.
//
// The bootstrap does not need the exemption. `loginRequired` is false until a password exists,
// so a fresh installation is open at 127.0.0.1 exactly as before, and the CLI that sets the
// first password reads the store directly rather than going through this mux. Once a password
// exists, the local operator types it like everyone else.

// roleKey types the role a middleware records for the request. Unexported and an empty struct,
// so nothing outside this package can claim a role by writing a context value.
type roleKey struct{}

// roleOfContext is the session role the middleware recorded, RoleNone when there was none.
func roleOfContext(ctx context.Context) Role {
	r, _ := ctx.Value(roleKey{}).(Role)
	return r
}

// openPath reports whether a path is served without a login.
func openPath(p string) bool {
	switch {
	case p == "/healthz", p == "/readyz", p == "/login", p == "/logout":
		return true
	case strings.HasPrefix(p, "/auth/"):
		// The Google flow. Both halves have to be reachable without a session, since their
		// whole purpose is to produce one — and the callback is entered by a redirect from
		// Google, which carries no cookie of ours at all.
		return true
	case strings.HasPrefix(p, "/webhook/"):
		return true
	case strings.HasPrefix(p, expose.MountPrefix):
		return true
	default:
		return false
	}
}

// requireLogin wraps the whole mux.
//
// The order of the checks is the design. A session is read first because it is the cheap,
// unambiguous answer; then the paths that are open by definition; then the case where no
// password has been set at all. Only after those is the request refused — which for a browser
// means the login page and for everything else means a 401 with a sentence, because a fetch that
// receives an HTML form has no way to report what went wrong.
//
// There is deliberately no check on where the request came from. See the note on loopback above.
func (i *Ingress) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i.console == nil {
			next.ServeHTTP(w, r)
			return
		}

		// The role travels on the request either way, because the write gates read it: a
		// request with an admin session must be allowed to write even though this middleware
		// lets every authenticated role through.
		role := i.console.roleOfRequest(r)
		if role != RoleNone {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleKey{}, role)))
			return
		}

		if openPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// No viewer password means reading is open, which is what every installation had
		// before this existed. The write surfaces are still gated by their own rules.
		if !i.console.loginRequired(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}

		if wantsJSON(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "not logged in", "login": "/login"})
			return
		}
		// 303 rather than rendering the form in place, so the address bar says /login and a
		// reload does not re-submit whatever the reader was originally fetching.
		http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

// requireAdmin is the second half, for the routes that change something.
//
// Not used as middleware: the two admin surfaces have their own gates, which already combine
// the daemon-wide and per-request checks, and this feeds those rather than replacing them. See
// isLocalRequest.

// wantsJSON reports whether a caller would rather have an error than a page.
//
// The dashboard's own fetches send `Accept: application/json`; a browser navigating sends
// `text/html`. An `/api/` prefix is checked as well, because a script using the API with no
// Accept header at all is the commonest way to be surprised by an HTML response.
func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return true
	}
	return !strings.Contains(accept, "text/html")
}

// urlQueryEscape is url.QueryEscape without the import, kept local because the only value it
// ever sees is a path this process produced.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~', r == '/':
			b.WriteRune(r)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}

// login serves the form and handles the submission.
func (i *Ingress) login(w http.ResponseWriter, r *http.Request) {
	if i.console == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()

	// Nothing to log into. Saying so beats a form that refuses every password, which is what
	// this looked like before the check existed.
	if !i.console.anyPasswordSet(ctx) {
		i.serveLogin(w, "", "No password is set on this daemon, so there is nothing to log in to. "+
			"Set one with: docpreview console password -role viewer", http.StatusOK)
		return
	}

	if r.Method == http.MethodGet {
		i.serveLogin(w, r.URL.Query().Get("next"), "", http.StatusOK)
		return
	}

	if err := r.ParseForm(); err != nil {
		i.serveLogin(w, "", "That form could not be read.", http.StatusBadRequest)
		return
	}
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")
	role := RoleFromUsername(r.PostFormValue("username"))

	if !i.console.Verify(ctx, role, password) {
		// One message for every way this fails: an unknown username, a role with no password
		// set, and the wrong password. The log carries which was attempted; the page does not
		// carry an oracle for whether `admin` exists or whether a viewer password is
		// configured.
		i.log.Warn("failed dashboard login",
			"username", r.PostFormValue("username"), "remote", r.RemoteAddr)
		i.serveLogin(w, next, "That username and password were not accepted.",
			http.StatusUnauthorized)
		return
	}

	setConsoleCookie(w, i.console.issue(role, time.Now()))
	i.log.Info("dashboard login", "role", role, "remote", r.RemoteAddr)

	// Only a path this daemon serves. A `next` from the query string is attacker-supplied, so
	// an absolute URL there is an open redirect — the classic phishing primitive, made worse
	// here because the victim has just typed a password.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout clears the session.
//
// POST only. A GET would let any page on the internet log an operator out with an `<img>` tag —
// harmless as mischief, and the kind of thing that wastes an afternoon being diagnosed.
func (i *Ingress) logout(w http.ResponseWriter, r *http.Request) {
	clearConsoleCookie(w)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// serveLogin renders the form.
//
// Its own page rather than a panel on the dashboard, and deliberately tiny: it is the one thing
// served to an unauthenticated caller, so it carries no build data, no project names and no
// JavaScript. What it says about the daemon is its name.
//
// # Every comment about this page belongs here, not in the markup
//
// An HTML comment is served. The rationale for these fields was originally written inline and
// therefore printed the two valid usernames into the page source of a login form reachable from
// the internet — the same defect as the placeholder that was removed from the username field,
// arriving by a route nobody looks at. Explanations live in Go; the template carries markup.
//
// # The username field
//
// A username as well as a password, and the username *is* the role. Two reasons. A password-only
// form gives a password manager nothing to label the entry with, so an operator's admin and
// viewer passwords collapse into one indistinguishable item — the reported complaint. And it
// makes the role something the person asked for rather than something inferred from which of two
// hashes happened to match, which is clearer and leaves less to get wrong.
//
// `autocomplete="username"` beside `current-password` is what makes a manager treat the two as
// one credential and offer to save it.
//
// **Nothing on the page names the valid usernames.** There are exactly two, and printing them
// would hand an attacker the account list — half the work, before they start on the password.
// Somebody who belongs here was told which one they have.
//
// The styles are inline for the same reason the dashboard is one embedded file — there is no
// asset pipeline here, and a login page that depends on a second request is a login page that
// renders unstyled when that request is the one being redirected.
func (i *Ingress) serveLogin(w http.ResponseWriter, next, message string, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Nothing about this page is cacheable, and a cached login form served after a logout is
	// a page that appears to have worked and has not.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)

	note := ""
	if message != "" {
		note = `<p class="note">` + html.EscapeString(message) + `</p>`
	}

	// The Google button, when a Google application is configured and the vault is open enough
	// to read it. Absent rather than disabled otherwise: a button that explains why it cannot
	// work is a worse answer than a page that simply asks for the password it can accept.
	//
	// Above the password field, because for most people it is the answer — and it says which
	// domains it will accept, so somebody signed into a personal account learns that here
	// rather than from a refusal three redirects later.
	google := ""
	if ok, _ := i.googleEnabled(context.Background()); ok {
		domains := strings.Join(i.oauthDomains(context.Background()), ", ")
		google = fmt.Sprintf(`
  <a class="google" href="/auth/google?next=%s">
    <svg viewBox="0 0 18 18" width="16" height="16" aria-hidden="true"><path fill="#4285F4" d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.71-1.57 2.68-3.88 2.68-6.62z"/><path fill="#34A853" d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.81.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.02-3.7H.96v2.34A8.99 8.99 0 0 0 9 18z"/><path fill="#FBBC05" d="M3.98 10.72a5.41 5.41 0 0 1 0-3.44V4.94H.96a8.99 8.99 0 0 0 0 8.12l3.02-2.34z"/><path fill="#EA4335" d="M9 3.58c1.32 0 2.5.45 3.44 1.35l2.58-2.59A8.99 8.99 0 0 0 .96 4.94l3.02 2.34C4.68 5.16 6.66 3.58 9 3.58z"/></svg>
    Sign in with Google
  </a>
  <p class="hint">for an address at %s</p>
  <div class="or"><span>or</span></div>`,
			html.EscapeString(urlQueryEscape(next)), html.EscapeString(domains))
	}

	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>docpreview</title>
<style>
:root { color-scheme: light dark }
body {
  margin: 0; min-height: 100vh; display: grid; place-items: center;
  background: #f9f9f7; color: #0b0b0b;
  font: 15px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
}
@media (prefers-color-scheme: dark) { body { background: #0d0d0d; color: #fff } }
form {
  width: min(22rem, 92vw); padding: 1.4rem;
  border: 1px solid rgba(128,128,128,0.3); border-radius: 12px;
  background: color-mix(in srgb, currentColor 4%%, transparent);
}
h1 { font-size: 1rem; margin: 0 0 0.2rem; letter-spacing: -0.01em }
p.sub { margin: 0 0 1rem; font-size: 0.82rem; opacity: 0.7 }
label { display: block; font-size: 0.78rem; opacity: 0.8; margin: 0.6rem 0 0.25rem }
label:first-of-type { margin-top: 0 }
input {
  width: 100%%; padding: 0.45rem 0.5rem; font: inherit; box-sizing: border-box;
  border: 1px solid rgba(128,128,128,0.4); border-radius: 6px;
  background: transparent; color: inherit;
}
button {
  margin-top: 0.8rem; width: 100%%; padding: 0.45rem; font: inherit; font-weight: 600;
  border: 0; border-radius: 6px; background: #035ce6; color: #fff; cursor: pointer;
}
p.note {
  margin: 0 0 0.9rem; padding: 0.5rem 0.6rem; font-size: 0.82rem;
  border-left: 3px solid #d03b3b; background: rgba(208,59,59,0.08);
}
/* Google's own guidance: white button, their mark at the left, their wording. Deliberately
   not restyled to match the rest — a sign-in button people recognise is worth more than one
   that matches the form it sits above. */
a.google {
  display: flex; align-items: center; justify-content: center; gap: 0.5rem;
  padding: 0.45rem; text-decoration: none; font-weight: 500;
  border: 1px solid rgba(128,128,128,0.4); border-radius: 6px;
  background: #fff; color: #1f1f1f;
}
a.google:hover { background: #f7f7f7 }
p.hint { margin: 0.35rem 0 0; font-size: 0.72rem; opacity: 0.6; text-align: center }
/* A rule with the word in the middle, so the two ways in are visibly alternatives rather
   than steps. */
.or {
  display: flex; align-items: center; gap: 0.6rem;
  margin: 1rem 0 0.9rem; font-size: 0.72rem; opacity: 0.6;
}
.or::before, .or::after {
  content: ""; flex: 1; height: 1px; background: rgba(128,128,128,0.35);
}
</style></head><body>
<form method="post" action="/login">
  <h1>docpreview</h1>
  <p class="sub">Documentation previews for pull requests</p>
  %s%s
  <label for="username">Username</label>
  <input id="username" name="username" type="text" autocomplete="username"
         autocapitalize="none" spellcheck="false" autofocus required>
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autocomplete="current-password" required>
  <input type="hidden" name="next" value="%s">
  <button type="submit">Sign in</button>
</form>
</body></html>
`, note, google, html.EscapeString(next))
}
