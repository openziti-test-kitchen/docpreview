package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Google sign-in for the dashboard, done by docpreview rather than by whatever publishes it.
//
// # Why not delegate to the tunnel
//
// The first attempt did: zrok can gate a share on an OAuth provider and an email domain at its
// frontend, which is less code and stronger — an unauthenticated request never reaches this
// process. It was the wrong answer for two reasons that only appear when somebody uses it.
//
// It is not docpreview's page, so it cannot offer a *choice*. The operator wants one login
// screen with a password field and a Google button beside it; what the frontend gives is zrok's
// own sign-in for zrok.io, before docpreview is reached, with no way to type a password instead.
//
// And two independent redirectors on one URL fight. zrok's interstitial redirects to authorize,
// docpreview redirects to its own form, and the browser bounced between them — reported as "just
// redirecting me over and over". Layering one login behind another produces that whenever the
// two disagree about who owns the request.
//
// So the share stays open and this process does the gating.
//
// # What a Google sign-in grants
//
// **Viewer, and only ever viewer.** Admin is password-only, deliberately: it decides what
// command runs on the build host and which credentials it runs with, and a misconfigured OAuth
// application — the wrong client, a domain typo, a Google Workspace that turns out to allow
// external members — should cost read access to build logs rather than the keys.
//
// # The flow, and what is trusted
//
// Authorization code flow. The state parameter is signed with the session key rather than stored,
// so nothing has to be reaped and a restart invalidates any in-flight login — the same trade the
// session cookie makes.
//
// The email comes from Google's userinfo endpoint, called with the access token obtained by
// exchanging the code directly with Google over TLS using the client secret. That is why no JWT
// verification happens here: the response is not a token handed over by the browser, it is an
// answer from Google to a request only this process could have made. Verifying an id_token would
// need JWKS fetching and caching to establish the same thing.
//
// `email_verified` is checked. Without it, a Workspace that permits unverified addresses would
// let somebody claim a colleague's.

// oauthStateTTL bounds how long a login may sit half-finished.
//
// Short, because the only thing between starting and finishing is a Google consent screen. A long
// window is a signed token an attacker has longer to replay into somebody else's browser.
const oauthStateTTL = 10 * time.Minute

// SettingOAuthDomains is the settings key holding the allowed email domains.
//
// In the database rather than the config file so it can be changed from the dashboard, and
// because it is not a secret: it is the list of domains, not the credentials.
const SettingOAuthDomains = "console.oauth_domains"

// googleEndpoints are Google's OAuth endpoints.
//
// Constants rather than a discovery-document fetch. Discovery buys the ability to follow Google
// moving them, at the cost of a network call on the login path and a cache to get wrong; these
// three URLs have been stable for a decade.
const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

// GoogleCredentials is what the ingress needs to run the flow, resolved per request.
//
// A function rather than captured values, for the reason every other credential in this codebase
// is: the vault may be locked when the daemon starts, and it may be unlocked later without a
// restart. `ok` false means no Google sign-in right now, which the login page states rather than
// offering a button that cannot work.
type GoogleCredentials func() (clientID, clientSecret string, ok bool)

// WithGoogleAuth installs the credential resolver.
func (i *Ingress) WithGoogleAuth(fn GoogleCredentials) *Ingress {
	i.google = fn
	return i
}

// googleEnabled reports whether a Google sign-in can be offered.
//
// Three things have to be true: credentials readable, at least one allowed domain, and a
// dashboard URL to come back to. All three are the operator's to configure, and the login page
// says which is missing rather than failing at the redirect.
func (i *Ingress) googleEnabled(ctx context.Context) (bool, string) {
	if i.google == nil {
		return false, "no Google application is wired into this daemon"
	}
	if _, _, ok := i.google(); !ok {
		return false, "no Google application is configured, or the vault is locked: store " +
			"google.oauth_client_id and google.oauth_client_secret"
	}
	if len(i.oauthDomains(ctx)) == 0 {
		return false, "no email domains are allowed: set them on the dashboard, or with " +
			"docpreview console oauth-domains"
	}
	if i.daemon == nil || strings.TrimSpace(i.daemon.cfg.DashboardURL) == "" {
		return false, "dashboard_url is not set, so Google has nowhere to send anybody back to"
	}
	return true, ""
}

// oauthDomains is the allowed email domains, lower-cased and stripped of any leading "@".
func (i *Ingress) oauthDomains(ctx context.Context) []string {
	if i.console == nil {
		return nil
	}
	raw, _, err := i.console.store.Setting(ctx, SettingOAuthDomains)
	if err != nil || raw == "" {
		return nil
	}
	var out []string
	for _, d := range strings.Split(raw, ",") {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// redirectURI is where Google sends the browser back.
//
// Derived from `dashboard_url` because the daemon cannot know it: the listener is loopback, and
// whatever makes the dashboard reachable — a tunnel, a proxy, a VPN — is outside this process.
// It must match a redirect URI registered on the Google application exactly, which is worth
// stating in the error rather than leaving to Google's own message.
func (i *Ingress) redirectURI() string {
	return strings.TrimRight(i.daemon.cfg.DashboardURL, "/") + "/auth/google/callback"
}

// signState signs the state parameter: an expiry, a nonce, and where to go afterwards.
//
// Signed rather than stored, so there is no server-side table to reap. The nonce is what makes
// two logins started in the same second distinguishable, and the expiry is what stops a state
// captured today being replayed next week.
func (i *Ingress) signState(next string, now time.Time) (string, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	body := fmt.Sprintf("%d:%s:%s", now.Add(oauthStateTTL).Unix(),
		base64.RawURLEncoding.EncodeToString(nonce), next)

	i.console.mu.RLock()
	signer := i.console.signer
	i.console.mu.RUnlock()
	mac := hmac.New(sha256.New, signer)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString([]byte(body)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifyState checks a state parameter and returns where the caller wanted to go.
func (i *Ingress) verifyState(state string, now time.Time) (string, bool) {
	encoded, sig, ok := strings.Cut(state, ".")
	if !ok {
		return "", false
	}
	bodyBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	i.console.mu.RLock()
	signer := i.console.signer
	i.console.mu.RUnlock()
	mac := hmac.New(sha256.New, signer)
	mac.Write(bodyBytes)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// Constant-time, for the same reason every other comparison here is.
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return "", false
	}

	parts := strings.SplitN(string(bodyBytes), ":", 3)
	if len(parts) != 3 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return "", false
	}
	return parts[2], true
}

// googleStart sends the browser to Google.
func (i *Ingress) googleStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ok, why := i.googleEnabled(ctx)
	if !ok {
		i.serveLogin(w, "", "Google sign-in is not available: "+why, http.StatusOK)
		return
	}
	clientID, _, _ := i.google()

	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	state, err := i.signState(next, time.Now())
	if err != nil {
		i.log.Error("signing the oauth state", "error", err)
		i.serveLogin(w, next, "Could not start the Google sign-in.", http.StatusInternalServerError)
		return
	}

	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {i.redirectURI()},
		"response_type": {"code"},
		"scope":         {"openid email"},
		"state":         {state},
		// Google will not return a refresh token and does not need to: this exchange happens
		// once per login and the session cookie is what carries the result.
		"access_type": {"online"},
		// `select_account` rather than none, so somebody signed into a personal account is
		// offered the choice instead of being silently refused for the wrong domain.
		"prompt": {"select_account"},
	}
	// `hd` is a hint, never a control. Google honours it in the account chooser, and an
	// attacker can simply not send it — so the domain is still checked on the way back. Sent
	// only when there is exactly one allowed domain, since the parameter takes one value.
	if domains := i.oauthDomains(ctx); len(domains) == 1 {
		q.Set("hd", domains[0])
	}

	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusSeeOther)
}

// googleCallback finishes the flow: exchange the code, read the email, check the domain, issue a
// viewer session.
func (i *Ingress) googleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if ok, why := i.googleEnabled(ctx); !ok {
		i.serveLogin(w, "", "Google sign-in is not available: "+why, http.StatusOK)
		return
	}
	clientID, clientSecret, _ := i.google()

	// Google's own refusal — a closed consent screen, a blocked application. Shown as a
	// message on the form rather than a bare error page, because the next thing the reader
	// wants is the password field.
	if e := r.URL.Query().Get("error"); e != "" {
		i.log.Warn("google refused the sign-in", "error", e)
		i.serveLogin(w, "", "Google did not complete the sign-in: "+e, http.StatusUnauthorized)
		return
	}

	next, ok := i.verifyState(r.URL.Query().Get("state"), time.Now())
	if !ok {
		// A state that does not verify is a callback this process did not start, or one that
		// has expired, or a restart in between. All three are refused the same way: the state
		// is the only thing tying the callback to a login, and accepting an unverified one is
		// accepting a login request from whoever sent the browser here.
		i.log.Warn("rejected an oauth callback with an unverifiable state", "remote", r.RemoteAddr)
		i.serveLogin(w, "", "That sign-in could not be verified. Please try again.",
			http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		i.serveLogin(w, next, "Google returned no authorization code.", http.StatusBadRequest)
		return
	}

	email, verified, err := i.exchangeGoogleCode(ctx, clientID, clientSecret, code)
	if err != nil {
		// The detail goes to the log, not the page: it can carry the redirect URI and Google's
		// own description of a misconfigured application, which is operator information rather
		// than something to show whoever is trying to sign in.
		i.log.Error("google token exchange failed", "error", err, "redirect_uri", i.redirectURI())
		i.serveLogin(w, next, "The Google sign-in could not be completed. "+
			"The daemon log has the detail.", http.StatusBadGateway)
		return
	}
	if !verified {
		i.log.Warn("refused an unverified google address", "email", email)
		i.serveLogin(w, next, "That Google account's email address is not verified.",
			http.StatusForbidden)
		return
	}

	if !domainAllowed(email, i.oauthDomains(ctx)) {
		// The address is logged because an operator's first question is "who tried", and it is
		// not a secret — but the *page* names only the domains, so the form cannot be used to
		// confirm whether a given address exists.
		i.log.Warn("refused a google sign-in from a domain that is not allowed", "email", email)
		i.serveLogin(w, next, fmt.Sprintf("That account is not in %s.",
			strings.Join(i.oauthDomains(ctx), " or ")), http.StatusForbidden)
		return
	}

	// Viewer, always. Admin is password-only — see the note at the top of this file.
	setConsoleCookie(w, i.console.issue(RoleViewer, time.Now()))
	i.log.Info("dashboard login via google", "email", email, "role", RoleViewer)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// exchangeGoogleCode swaps an authorization code for the signed-in address.
func (i *Ingress) exchangeGoogleCode(ctx context.Context, clientID, clientSecret, code string,
) (email string, verified bool, err error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {i.redirectURI()},
		"grant_type":    {"authorization_code"},
	}

	// A timeout on both calls. Without one, a hung Google leaves a browser waiting on a
	// request that never returns, and the reader's only recourse is to start again — into
		// another hung request.
	client := &http.Client{Timeout: 20 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	defer resp.Body.Close()

	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", false, fmt.Errorf("reading the token response: %w", err)
	}
	if token.Error != "" {
		// Google's own words. `redirect_uri_mismatch` is overwhelmingly the one that happens,
		// and it means dashboard_url and the application's registered URI disagree.
		return "", false, fmt.Errorf("google refused the exchange: %s: %s",
			token.Error, token.ErrorDesc)
	}
	if token.AccessToken == "" {
		return "", false, fmt.Errorf("google returned no access token (HTTP %d)", resp.StatusCode)
	}

	// The address, from Google rather than from the browser. This response is trustworthy
	// because the token was obtained by a call only this process could make — see the note at
	// the top of this file on why no JWT verification is involved.
	uReq, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserinfoURL, nil)
	if err != nil {
		return "", false, err
	}
	uReq.Header.Set("Authorization", "Bearer "+token.AccessToken)

	uResp, err := client.Do(uReq)
	if err != nil {
		return "", false, fmt.Errorf("reading the signed-in account: %w", err)
	}
	defer uResp.Body.Close()

	var info struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.NewDecoder(uResp.Body).Decode(&info); err != nil {
		return "", false, fmt.Errorf("reading the account response: %w", err)
	}
	if info.Email == "" {
		return "", false, fmt.Errorf("google reported no email address (HTTP %d)", uResp.StatusCode)
	}
	return info.Email, info.EmailVerified, nil
}

// domainAllowed reports whether an address is in one of the allowed domains.
//
// An exact match on the part after the last "@", case-insensitively. Not a suffix test: a suffix
// would let `evil-example.com` through a list naming `example.com`, which is the classic version
// of this bug.
func domainAllowed(email string, domains []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 || len(domains) == 0 {
		return false
	}
	got := strings.ToLower(email[at+1:])
	for _, d := range domains {
		if got == d {
			return true
		}
	}
	return false
}
