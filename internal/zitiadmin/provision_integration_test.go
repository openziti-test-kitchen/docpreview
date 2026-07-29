package zitiadmin

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// These run against a live OpenZiti controller and create real objects, all
// named with a dptest- prefix so they cannot be confused with anything an
// operator or another test made.
//
// They skip unless pointed at one, so `go test ./...` on a machine with no
// controller stays green:
//
//	DOCPREVIEW_ZITI_CONTROLLER=https://localhost:1280
//	DOCPREVIEW_ZITI_USER=admin        (optional, defaults to admin)
//	DOCPREVIEW_ZITI_PASSWORD=admin    (optional, defaults to admin)
func testOptions(t *testing.T, outDir string) Options {
	t.Helper()

	controller := os.Getenv("DOCPREVIEW_ZITI_CONTROLLER")
	if controller == "" {
		t.Skip("set DOCPREVIEW_ZITI_CONTROLLER to run")
	}
	user, password := os.Getenv("DOCPREVIEW_ZITI_USER"), os.Getenv("DOCPREVIEW_ZITI_PASSWORD")
	if user == "" {
		user = "admin"
	}
	if password == "" {
		password = "admin"
	}

	return Options{
		Controller:   controller,
		Username:     user,
		Password:     password,
		Domain:       "dptest.ziti",
		Service:      "dptest-svc",
		AdminService: "dptest-admin",
		HostIdentity: "dptest-host",
		Reviewer:     "dptest-reviewer",
		ReaderRole:   "dptest-reader",
		Prefix:       "dptest-",
		OutDir:       outDir,
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestProvisionIsIdempotent(t *testing.T) {
	// The stated flow is `ziti edge quickstart` then `docpreview configure
	// ziti`, and people run a setup command more than once — after reading the
	// output, after changing a flag, after a failure halfway through. A second
	// run that errors on "a service with that name already exists" teaches
	// them to tear the network down before every attempt.
	dir := t.TempDir()
	opts := testOptions(t, dir)
	ctx := context.Background()

	first, err := Provision(ctx, opts, discard())
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if len(first.Created) == 0 && len(first.Reused) == 0 {
		t.Fatal("Provision reported no objects at all")
	}

	second, err := Provision(ctx, opts, discard())
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}

	// Nothing new the second time. A created object here means a check-then-
	// create that does not actually find what it made, which would leave
	// duplicate policies accumulating on every run.
	if len(second.Created) != 0 {
		t.Errorf("the second run created %v; everything should already exist", second.Created)
	}
	if len(second.Reused) != len(first.Created)+len(first.Reused) {
		t.Errorf("the second run saw %d existing objects, the first touched %d",
			len(second.Reused), len(first.Created)+len(first.Reused))
	}
}

func TestProvisionWritesUsableFiles(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(t, dir)

	result, err := Provision(context.Background(), opts, discard())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// The identity file is what `docpreview serve` loads. An empty or absent
	// one is the difference between the daemon starting and it failing on the
	// line after "configure ziti succeeded".
	info, err := os.Stat(result.HostIdentityFile)
	if err != nil {
		t.Fatalf("the hosting identity was not written: %v", err)
	}
	if info.Size() == 0 {
		t.Error("the hosting identity file is empty")
	}
	if want := filepath.Join(dir, "dptest-host.json"); result.HostIdentityFile != want {
		t.Errorf("identity file = %q, want %q", result.HostIdentityFile, want)
	}

	// The reviewer token is the only thing a reviewer is ever given. If it is
	// missing, there is no way into the overlay at all.
	switch {
	case result.ReviewerEnrolled:
		t.Log("the reviewer identity is already enrolled, so no token was written")
	case result.ReviewerJWTFile == "":
		t.Fatal("no reviewer token and no explanation")
	default:
		jwt, err := os.ReadFile(result.ReviewerJWTFile)
		if err != nil {
			t.Fatalf("reading the reviewer token: %v", err)
		}
		// A JWT is three dot-separated segments. Anything else means we wrote
		// an error page or an id where a token was expected.
		if len(jwt) < 32 || countByte(jwt, '.') != 2 {
			t.Errorf("the reviewer token does not look like a JWT: %q", jwt)
		}
	}
}

func TestProvisionRecreatesAHostIdentityWithNoFile(t *testing.T) {
	// An enrollment token is one-time. An identity on the controller whose
	// file is gone can never authenticate again, so preserving it would leave
	// `configure ziti` reporting success and `serve` unable to start.
	dir := t.TempDir()
	opts := testOptions(t, dir)
	opts.HostIdentity = "dptest-host-orphan"
	ctx := context.Background()

	first, err := Provision(ctx, opts, discard())
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if err := os.Remove(first.HostIdentityFile); err != nil {
		t.Fatal(err)
	}

	second, err := Provision(ctx, opts, discard())
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if _, err := os.Stat(second.HostIdentityFile); err != nil {
		t.Fatalf("the identity file was not restored: %v", err)
	}
}

func countByte(b []byte, c byte) int {
	n := 0
	for _, x := range b {
		if x == c {
			n++
		}
	}
	return n
}
