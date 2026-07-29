package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/vault"
)

const testSecret = "hunter2"

func testClient() *Client {
	return &Client{
		log:           slog.New(slog.DiscardHandler),
		webhookSecret: vault.NewSecretString(testSecret),
	}
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func headers(event, signature string) map[string][]string {
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", "test-delivery")
	if signature != "" {
		h.Set("X-Hub-Signature-256", signature)
	}
	return h
}

const openedPayload = `{
  "action": "opened",
  "number": 42,
  "pull_request": {
    "number": 42,
    "head": {"ref": "feature/new-guide", "sha": "abc123", "repo": {"full_name": "acme/docs"}},
    "base": {"ref": "main"},
    "draft": false
  },
  "repository": {"name": "docs", "owner": {"login": "acme"}, "clone_url": "https://github.com/acme/docs.git"},
  "installation": {"id": 999}
}`

func TestVerifyWebhookRejectsMissingSignature(t *testing.T) {
	c := testClient()
	body := []byte(openedPayload)

	_, err := c.VerifyWebhook(context.Background(), headers("pull_request", ""), body)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unsigned delivery was accepted: %v", err)
	}
}

func TestVerifyWebhookRejectsWrongSignature(t *testing.T) {
	c := testClient()
	body := []byte(openedPayload)

	cases := map[string]string{
		"wrong digest":  "sha256=" + hex.EncodeToString(make([]byte, 32)),
		"wrong prefix":  "sha1=" + hex.EncodeToString(make([]byte, 20)),
		"not hex":       "sha256=zzzz",
		"empty digest":  "sha256=",
		"no prefix":     hex.EncodeToString(make([]byte, 32)),
		"signed as sha": sign([]byte("some other body")),
	}
	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.VerifyWebhook(context.Background(), headers("pull_request", sig), body); err == nil {
				t.Fatal("a bad signature was accepted")
			}
		})
	}
}

func TestVerifyWebhookAcceptsSignedPullRequest(t *testing.T) {
	c := testClient()
	body := []byte(openedPayload)

	events, err := c.VerifyWebhook(context.Background(), headers("pull_request", sign(body)), body)
	if err != nil {
		t.Fatalf("a correctly signed delivery was rejected: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	ev := events[0]
	if ev.Kind != scm.EventBuild {
		t.Errorf("kind = %q, want %q", ev.Kind, scm.EventBuild)
	}
	if ev.PR.Number != 42 {
		t.Errorf("number = %d, want 42", ev.PR.Number)
	}
	if ev.PR.Branch != "feature/new-guide" {
		t.Errorf("branch = %q", ev.PR.Branch)
	}
	if ev.PR.HeadSHA != "abc123" {
		t.Errorf("head sha = %q", ev.PR.HeadSHA)
	}
	if ev.PR.InstallationID != 999 {
		t.Errorf("installation id = %d, want 999", ev.PR.InstallationID)
	}
	if ev.PR.Repo.Slug() != "acme/docs" {
		t.Errorf("repo = %q", ev.PR.Repo.Slug())
	}
}

func TestVerifyWebhookRefusesForkPullRequests(t *testing.T) {
	// Building a fork means cloning and executing an outsider's code under our
	// installation token. There is no version of that which is acceptable here.
	const forked = `{
      "action": "opened",
      "pull_request": {
        "number": 7,
        "head": {"ref": "attack", "sha": "def456", "repo": {"full_name": "mallory/docs"}},
        "base": {"ref": "main"}
      },
      "repository": {"name": "docs", "owner": {"login": "acme"}},
      "installation": {"id": 999}
    }`

	c := testClient()
	body := []byte(forked)

	events, err := c.VerifyWebhook(context.Background(), headers("pull_request", sign(body)), body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a fork pull request produced %d events, want 0", len(events))
	}
}

func TestVerifyWebhookMapsClosedToTeardown(t *testing.T) {
	const closed = `{
      "action": "closed",
      "pull_request": {"number": 42, "head": {"ref": "f", "sha": "s"}, "base": {"ref": "main"}},
      "repository": {"name": "docs", "owner": {"login": "acme"}},
      "installation": {"id": 999}
    }`

	c := testClient()
	body := []byte(closed)

	events, err := c.VerifyWebhook(context.Background(), headers("pull_request", sign(body)), body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if len(events) != 1 || events[0].Kind != scm.EventTeardown {
		t.Fatalf("closed did not produce a teardown: %+v", events)
	}
}

func TestVerifyWebhookIgnoresUninterestingDeliveries(t *testing.T) {
	c := testClient()

	for _, tc := range []struct{ event, body string }{
		{"ping", `{"zen":"hello"}`},
		{"push", `{"ref":"refs/heads/main"}`},
		{"pull_request", `{"action":"labeled","repository":{"name":"docs","owner":{"login":"acme"}}}`},
		{"pull_request", `{"action":"converted_to_draft","repository":{"name":"docs","owner":{"login":"acme"}}}`},
	} {
		body := []byte(tc.body)
		events, err := c.VerifyWebhook(context.Background(), headers(tc.event, sign(body)), body)
		if err != nil {
			t.Errorf("%s/%s: %v", tc.event, tc.body, err)
		}
		if len(events) != 0 {
			t.Errorf("%s/%s produced %d events, want 0", tc.event, tc.body, len(events))
		}
	}
}

func TestVerifyWebhookRejectsBuildEventWithoutHead(t *testing.T) {
	// A payload with no head SHA would clone whatever the branch points at now,
	// which is not necessarily what the event described.
	const headless = `{
      "action": "synchronize",
      "pull_request": {"number": 1, "head": {"ref": "", "sha": ""}, "base": {"ref": "main"}},
      "repository": {"name": "docs", "owner": {"login": "acme"}},
      "installation": {"id": 999}
    }`

	c := testClient()
	body := []byte(headless)

	if _, err := c.VerifyWebhook(context.Background(), headers("pull_request", sign(body)), body); err == nil {
		t.Fatal("a payload with no head was accepted")
	}
}
