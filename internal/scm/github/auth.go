// Package github implements the scm.Client interface against GitHub, as a
// GitHub App.
//
// An App rather than a personal access token, for three reasons that matter to
// a service like this one. Its permissions are scoped to the repositories it is
// installed on rather than to everything a human can see. Its comments and
// check runs are attributed to the app, not to whoever generated the token. And
// its credentials are short-lived by construction: the private key mints a
// ten-minute JWT, which mints a one-hour installation token, so a token that
// leaks out of a log expires on its own.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/netfoundry/docpreview/internal/vault"
)

// appJWTLifetime is how long the App-level JWT is valid.
//
// GitHub rejects anything over ten minutes. Nine leaves room for the clock skew
// allowance below without brushing against the limit.
const appJWTLifetime = 9 * time.Minute

// clockSkew backdates the JWT's issued-at claim.
//
// GitHub rejects a JWT whose iat is in the future according to *their* clock. A
// host running a minute fast — routine on a laptop that has been suspended —
// would otherwise see every single API call fail with a 401 that says nothing
// about clocks. Sixty seconds is the value GitHub's own documentation
// recommends.
const clockSkew = 60 * time.Second

// installationTokenTTL is how long GitHub installation tokens last.
const installationTokenTTL = time.Hour

// tokenRefreshMargin is how far before expiry a cached token is discarded.
// A token that expires mid-request is indistinguishable from a revoked one, and
// far more annoying.
const tokenRefreshMargin = 5 * time.Minute

// authenticator mints and caches GitHub App credentials.
type authenticator struct {
	appID   int64
	apiBase string
	key     *rsa.PrivateKey
	http    *http.Client

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

func newAuthenticator(appID int64, apiBase string, pem vault.Secret, hc *http.Client) (*authenticator, error) {
	if appID == 0 {
		return nil, fmt.Errorf("github.app_id is not set")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem.Reveal())
	if err != nil {
		return nil, fmt.Errorf("parsing GitHub App private key "+
			"(it must be the PEM file GitHub generated, stored with: docpreview vault set %s --file key.pem): %w",
			vault.KeyGitHubPrivateKey, err)
	}
	return &authenticator{
		appID:   appID,
		apiBase: apiBase,
		key:     key,
		http:    hc,
		tokens:  map[int64]cachedToken{},
	}, nil
}

// appJWT mints a short-lived JWT identifying the App itself. It is used only to
// exchange for installation tokens and to read App-level metadata.
func (a *authenticator) appJWT() (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-clockSkew)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTLifetime)),
		Issuer:    fmt.Sprintf("%d", a.appID),
	})
	signed, err := tok.SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("signing App JWT: %w", err)
	}
	return signed, nil
}

// installationToken returns a token authorized for one installation, minting a
// fresh one when the cached token is absent or close to expiry.
func (a *authenticator) installationToken(ctx context.Context, installationID int64) (string, error) {
	if installationID == 0 {
		return "", fmt.Errorf("no installation id on this event; " +
			"the webhook payload was missing installation.id, which means the delivery did not come from an App installation")
	}

	a.mu.Lock()
	cached, ok := a.tokens[installationID]
	a.mu.Unlock()
	if ok && time.Until(cached.expiresAt) > tokenRefreshMargin {
		return cached.value, nil
	}

	appJWT, err := a.appJWT()
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("requesting installation token for %d: %w",
			installationID, errorFromResponse(resp))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding installation token: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("github returned an empty installation token for %d", installationID)
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = time.Now().Add(installationTokenTTL)
	}

	a.mu.Lock()
	a.tokens[installationID] = cachedToken{value: out.Token, expiresAt: out.ExpiresAt}
	a.mu.Unlock()

	return out.Token, nil
}

// invalidate drops the cached token for an installation.
//
// Expiry is not the only way a token dies. It is revoked when the App's
// permissions change, when the installation is suspended, and when a user
// reinstalls — and none of those move the clock, so `tokenRefreshMargin` never
// notices. Without this the daemon holds a dead token and every request 401s
// until the cached expiry finally passes, which can be the better part of an
// hour of a repository looking broken for no visible reason.
//
// Called from the request path on a 401. That is the only signal GitHub gives.
func (a *authenticator) invalidate(installationID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, installationID)
}
