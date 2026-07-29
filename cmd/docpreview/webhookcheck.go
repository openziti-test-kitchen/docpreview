package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/vault"
)

// cmdWebhookCheck sends a correctly signed ping to a webhook URL and reports
// whether it was accepted.
//
// # Why this exists
//
// Everything about the delivery path can be verified from outside except the one
// thing that matters: whether a *signed* request is accepted. An unsigned probe
// gets 401, which proves the request reached the daemon and that verification
// ran, and proves nothing about whether it would ever pass. The alternative to
// this command is configuring the App and reading GitHub's Recent Deliveries
// page to find out — which means discovering a broken tunnel or a mismatched
// secret from the far side, after the fact.
//
// A ping is the right probe rather than a synthetic pull_request. Signature
// verification happens before the event type is examined (see
// internal/scm/github/webhook.go), so a signed ping that returns 2xx has already
// proven the signature passed. A fabricated pull_request would additionally
// queue a build for a repository that does not exist, leaving junk state behind
// to prove something this already proves.
//
// # The secret
//
// Read from the vault into this process and used to compute one HMAC. It is
// never printed, never passed as an argument, and never reaches a shell history
// — which is the reason this is a command rather than a documented curl
// invocation with the secret pasted into it.
func cmdWebhookCheck(args []string) error {
	fs := flag.NewFlagSet("webhook-check", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	target := fs.String("url", "", "the webhook URL to test, e.g. https://name.shares.zrok.io/webhook/github")
	timeout := fs.Duration("timeout", 30*time.Second, "how long to wait for a response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return fmt.Errorf("webhook-check: -url is required, e.g. " +
			"-url https://docpreview.shares.zrok.io/webhook/github")
	}

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	v, err := vault.OpenFrom(cfg.VaultPath(), cfg.KeySource())
	if err != nil {
		return fmt.Errorf("opening the vault to read the webhook secret: %w", err)
	}
	secret, err := v.MustGet(vault.KeyGitHubWebhookSec)
	if err != nil {
		return err
	}

	// The shape GitHub actually sends on save. Only the signature matters to the
	// daemon, but a body that does not parse would make a 4xx ambiguous.
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":0}`)

	mac := hmac.New(sha256.New, secret.Reveal())
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	delivery, err := randomDeliveryID()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, *target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("User-Agent", "docpreview-webhook-check")

	fmt.Printf("POST %s\n", *target)
	fmt.Printf("  event     ping\n")
	fmt.Printf("  delivery  %s\n", delivery)

	start := time.Now()
	resp, err := (&http.Client{Timeout: *timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("the webhook URL is not reachable: %w\n"+
			"  Is the tunnel running?  docpreview webhook-only -zrok-name <name>", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	elapsed := time.Since(start).Round(time.Millisecond)

	fmt.Printf("  status    %s in %s\n", resp.Status, elapsed)
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && len(trimmed) < 300 {
		fmt.Printf("  body      %s\n", trimmed)
	}
	fmt.Println()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		fmt.Printf("PASS — a signed delivery is accepted end to end.\n\n" +
			"That covers the public URL, the tunnel, the webhook-only filter, and the daemon's\n" +
			"signature check against the secret in the vault. Give GitHub this URL and the same\n" +
			"webhook secret and its own ping will land the same way.\n")
		return nil

	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("FAIL — the daemon rejected the signature.\n\n" +
			"The request arrived, so the tunnel is fine. The secret in this vault is not the one\n" +
			"the receiving daemon is using. Most likely: -config points at a different vault than\n" +
			"the running daemon, or the secret was rotated after that daemon read it.\n" +
			"  Restart the daemon, or re-store it:  docpreview vault set github.webhook_secret")

	case resp.StatusCode == http.StatusNotImplemented:
		return fmt.Errorf("FAIL — the daemon has no GitHub client (501).\n\n" +
			"github.app_id is unset, or the vault was locked when the daemon started and nothing\n" +
			"has unlocked it since.\n" +
			"  Check:  docpreview doctor")

	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("FAIL — nothing serves that path (404).\n\n" +
			"The tunnel is up but the path is wrong, or webhook-only is forwarding a different one.\n" +
			"  The path must be exactly the one webhook-only was given, /webhook/github by default")

	case resp.StatusCode == http.StatusBadGateway:
		return fmt.Errorf("FAIL — the tunnel is up but nothing is behind it (502).\n\n" +
			"zrok has the share and cannot reach the backend. Either webhook-only is not running,\n" +
			"or it is still attaching its listener — that takes a few seconds after startup.")

	default:
		return fmt.Errorf("FAIL — unexpected status %s", resp.Status)
	}
}

// randomDeliveryID mints a GitHub-shaped delivery id, so the daemon's log line
// for this probe is distinguishable from a real delivery and from a rerun.
func randomDeliveryID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a delivery id: %w", err)
	}
	return "check-" + hex.EncodeToString(buf), nil
}
