package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// applyZrokScope points this process at one of the two zrok environment directories.
//
// Called once from setup, before anything loads a zrok root. The rules, in order:
//
//   - A stored choice wins. That is the operator's decision and nothing here second-guesses it.
//   - With no stored choice and only one root enabled, that one is used and the choice is
//     *recorded*, so the answer does not change under the daemon when the other one later becomes
//     enabled. A silently changing exposer account is the failure this whole mechanism exists to
//     prevent: startup reaps every share it recognises as its own.
//   - With no stored choice and both enabled, the project root is used and a warning says so.
//     Refusing to start would be worse — the page that resolves the ambiguity is served by this
//     daemon, which is the boot-order trap this codebase has now fallen into three times — and
//     the project root is the safer default of the two, because it is the one nothing else uses.
//   - With neither enabled there is nothing to publish through yet. The project root is selected
//     so that enrolling from the dashboard writes beside the vault rather than into a home
//     directory, and nothing is recorded, because no choice has been made.
func applyZrokScope(st *store.Store, cfg config.Server, log *slog.Logger) error {
	ctx := context.Background()

	stored, _, err := st.Setting(ctx, store.SettingZrokScope)
	if err != nil {
		return fmt.Errorf("reading the zrok environment choice: %w", err)
	}
	scope := expose.ZrokScope(strings.TrimSpace(stored))

	if scope != "" {
		if err := expose.UseZrokRoot(scope, cfg.ZrokDir()); err != nil {
			return err
		}
		log.Debug("using the stored zrok environment", "scope", scope)
		return nil
	}

	state, err := expose.InspectZrokRoots(cfg.ZrokDir(), "")
	if err != nil {
		return err
	}

	scope, record, ambiguous := chooseZrokScope(state)
	if ambiguous {
		log.Warn("two zrok environments are enabled and no choice is stored; using this "+
			"installation's own",
			"project", state.Project.Path, "system", state.System.Path,
			"fix", "docpreview zrok use system|project")
	}
	if record {
		// Recorded, not merely used. Without the write, enrolling the other environment later
		// would move this daemon to a different zrok account at the next restart, and the
		// first thing it would do there is reap.
		if err := st.SetSetting(ctx, store.SettingZrokScope, string(scope)); err != nil {
			return fmt.Errorf("recording the zrok environment choice: %w", err)
		}
		log.Info("using the zrok environment that is enabled", "scope", scope)
	}
	return expose.UseZrokRoot(scope, cfg.ZrokDir())
}

// chooseZrokScope decides which environment to adopt when nothing is stored.
//
// Separated from applyZrokScope so the decision can be tested: the alternative touches zrok's
// process-wide root global, which cannot be undone within a test binary.
//
// Returns the scope, whether to record it, and whether the answer was a guess between two
// enabled environments.
func chooseZrokScope(state expose.ZrokEnvState) (scope expose.ZrokScope, record, ambiguous bool) {
	switch {
	case state.System.Enabled && state.Project.Enabled:
		// Not recorded. Recording would turn a guess into the operator's decision, and the
		// panel would stop asking — while one of the two accounts is still being reaped by
		// whatever else uses it. The project root is the safer of the two to guess, because it
		// is the one nothing else can be using.
		return expose.ZrokProject, false, true

	case state.System.Enabled:
		return expose.ZrokSystem, true, false

	case state.Project.Enabled:
		return expose.ZrokProject, true, false

	default:
		// Nothing enrolled anywhere. The project root, so that enrolling from the dashboard
		// writes beside the vault rather than into a home directory — and nothing recorded,
		// because no choice has been made yet.
		return expose.ZrokProject, false, false
	}
}

// useZrokHome points a tunnel command at a zrok environment directory.
//
// For `webhook-only` and `dashboard-only`, which read no config and so cannot derive the daemon's
// choice. An empty dir leaves zrok's own default in place, which keeps every existing invocation
// working — and is why the flag has to be remembered when the daemon moves to its own environment:
// a share created from a different account reserves a name the previews cannot use.
func useZrokHome(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return expose.UseZrokRoot(expose.ZrokProject, dir)
}

// cmdZrok is the zrok account and environment surface on the command line.
//
// The same operations as the dashboard panel, for a host with no browser on it and for the
// ordering problem that panel cannot solve on its own: `zrok use` decides which environment the
// daemon adopts, and the daemon reads it at startup.
func cmdZrok(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("zrok: no subcommand")
	}

	switch args[0] {
	case "status":
		return cmdZrokStatus(args[1:])
	case "use":
		return cmdZrokUse(args[1:])
	case "invite":
		return cmdZrokInvite(args[1:])
	case "register":
		return cmdZrokRegister(args[1:])
	case "enable":
		return cmdZrokEnable(args[1:])
	case "disable":
		return cmdZrokDisable(args[1:])
	default:
		usage()
		return fmt.Errorf("zrok: unknown subcommand %q", args[0])
	}
}

// openZrokStore loads the config and opens the database, as the console commands do.
func openZrokStore(configPath string) (config.Server, *store.Store, error) {
	return openConsoleStore(configPath)
}

// storedZrokScope reads the recorded choice, empty when none has been made.
func storedZrokScope(ctx context.Context, st *store.Store) (expose.ZrokScope, error) {
	v, _, err := st.Setting(ctx, store.SettingZrokScope)
	if err != nil {
		return "", err
	}
	return expose.ZrokScope(strings.TrimSpace(v)), nil
}

// cmdZrokStatus describes both environments and which one is in force.
func cmdZrokStatus(args []string) error {
	fs := flag.NewFlagSet("zrok status", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, st, err := openZrokStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	scope, err := storedZrokScope(ctx, st)
	if err != nil {
		return err
	}
	state, err := expose.InspectZrokRoots(cfg.ZrokDir(), scope)
	if err != nil {
		return err
	}

	if scope == "" {
		fmt.Println("in use: nothing recorded — the daemon decides at startup and records it")
	} else {
		fmt.Printf("in use: %s\n", scope)
	}
	for _, r := range []struct {
		name  string
		which expose.ZrokScope
		info  expose.ZrokRootInfo
	}{
		{"this installation", expose.ZrokProject, state.Project},
		{"this machine", expose.ZrokSystem, state.System},
	} {
		mark := " "
		if scope == r.which {
			mark = "*"
		}
		fmt.Printf("%s %-18s %s\n", mark, r.name, r.info.Path)
		switch {
		case r.info.Enabled:
			fmt.Printf("    enabled against %s", orNone(r.info.APIEndpoint))
			if r.info.Namespace != "" {
				fmt.Printf(", default namespace %s", r.info.Namespace)
			}
			fmt.Println()
		case r.info.Why != "":
			fmt.Printf("    %s\n", r.info.Why)
		default:
			fmt.Println("    nothing here yet")
		}
	}

	if state.MustChoose {
		fmt.Println()
		fmt.Println("Both are enabled and they are different zrok accounts. Pick one:")
		fmt.Println("  docpreview zrok use project     # this installation's own")
		fmt.Println("  docpreview zrok use system      # the one the zrok CLI uses")
		fmt.Println()
		fmt.Println("Startup deletes every share it recognises as its own, so a daemon on the")
		fmt.Println("wrong account deletes whatever else is using that account.")
	}
	return nil
}

// cmdZrokUse records which environment the daemon should adopt.
//
// It writes the setting and nothing else. The daemon reads it once at startup, because zrok's root
// directory is a process-wide global — so this takes effect on the next restart, which the command
// says rather than leaving the operator to discover.
func cmdZrokUse(args []string) error {
	fs := flag.NewFlagSet("zrok use", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: docpreview zrok use system|project")
	}

	scope := expose.ZrokScope(strings.ToLower(strings.TrimSpace(fs.Arg(0))))
	if scope != expose.ZrokSystem && scope != expose.ZrokProject {
		return fmt.Errorf("%q is not a zrok environment: use system or project", fs.Arg(0))
	}

	cfg, st, err := openZrokStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	state, err := expose.InspectZrokRoots(cfg.ZrokDir(), scope)
	if err != nil {
		return err
	}
	chosen := state.Project
	if scope == expose.ZrokSystem {
		chosen = state.System
	}

	if err := st.SetSetting(ctx, store.SettingZrokScope, string(scope)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "the daemon will use the %s zrok environment: %s\n", scope, chosen.Path)
	if !chosen.Enabled {
		// Allowed rather than refused: choosing the empty one and then enrolling into it is the
		// ordinary path for a new installation.
		fmt.Fprintln(os.Stderr, "nothing is enrolled there yet — enrol with:")
		fmt.Fprintln(os.Stderr, "  docpreview zrok enable   (or the zrok panel on the dashboard)")
	}
	fmt.Fprintln(os.Stderr, "restart the daemon for this to take effect")
	return nil
}

// cmdZrokInvite asks the zrok service to email a registration link.
func cmdZrokInvite(args []string) error {
	fs := flag.NewFlagSet("zrok invite", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	endpoint := fs.String("api-endpoint", "", "the zrok service, default "+expose.DefaultZrokAPIEndpoint)
	inviteToken := fs.String("invite-token", "", "for a zrok service that is itself invitation-only")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: docpreview zrok invite <email>")
	}
	email := fs.Arg(0)

	cfg, st, err := openZrokStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	// Refused when an account is already enrolled, which is what was asked for and is also the
	// kinder answer: the service rejects a duplicate email with an opaque 400, and an operator who
	// already has an account usually wants `zrok enable`, not a second account.
	if err := refuseIfZrokEnrolled(ctx, st, cfg); err != nil {
		return err
	}

	if err := expose.ZrokInvite(ctx, *endpoint, email, *inviteToken); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "zrok is emailing %s a registration link\n", email)
	fmt.Fprintln(os.Stderr, "open it, then finish with the token at the end of that link:")
	fmt.Fprintln(os.Stderr, "  docpreview zrok register <link-or-token>")
	return nil
}

// cmdZrokRegister turns a registration token into an account, then enrols this host.
//
// One command for both halves because they are one act from the operator's side, and because the
// account token exists only in the register response — a command that printed it and stopped would
// be a command whose output has to be pasted into the next one, with a credential in it.
func cmdZrokRegister(args []string) error {
	fs := flag.NewFlagSet("zrok register", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	endpoint := fs.String("api-endpoint", "", "the zrok service, default "+expose.DefaultZrokAPIEndpoint)
	description := fs.String("description", "", "how this environment is labelled in the zrok account")
	noEnable := fs.Bool("no-enable", false, "create the account but do not enrol this host")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: docpreview zrok register <link-or-token>")
	}

	cfg, st, err := openZrokStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	if err := refuseIfZrokEnrolled(ctx, st, cfg); err != nil {
		return err
	}

	// The zrok account password, from stdin. Never an argument, for the same reason the console
	// password and every vault value are not: an argument is in the shell history and in `ps`.
	fmt.Fprintln(os.Stderr, "choose a password for the zrok account (it is not stored here):")
	pw, err := readStdin()
	if err != nil {
		return err
	}
	password := string(trimTrailingNewline(pw))
	if password == "" {
		return errors.New("refusing to register with an empty password")
	}

	token, err := expose.ZrokRegister(ctx, *endpoint, fs.Arg(0), password)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "the zrok account was created")

	if err := storeZrokAccountToken(ctx, st, cfg, token); err != nil {
		return err
	}

	if *noEnable {
		fmt.Fprintln(os.Stderr, "not enrolling this host; when you are ready:")
		fmt.Fprintln(os.Stderr, "  docpreview zrok enable")
		return nil
	}
	return enableZrokHere(ctx, st, cfg, token, *description)
}

// cmdZrokEnable enrols this host against an account token.
//
// The token comes from the vault when `zrok register` put one there, and from stdin otherwise —
// which is the path for an operator who already has a zrok account.
func cmdZrokEnable(args []string) error {
	fs := flag.NewFlagSet("zrok enable", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	description := fs.String("description", "", "how this environment is labelled in the zrok account")
	fromStdin := fs.Bool("token-stdin", false, "read the account token from stdin instead of the vault")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, st, err := openZrokStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	var token vault.Secret
	if *fromStdin {
		raw, err := readStdin()
		if err != nil {
			return err
		}
		token = vault.NewSecret(trimTrailingNewline(raw))
		if token.IsZero() {
			return errors.New("no account token on stdin")
		}
		if err := storeZrokAccountToken(ctx, st, cfg, token); err != nil {
			return err
		}
	} else {
		v, err := vault.OpenFrom(cfg.VaultPath(), cfg.KeySource())
		if err != nil {
			return err
		}
		token, err = v.MustGet(vault.KeyZrokAccountToken)
		if err != nil {
			return fmt.Errorf("no zrok account token stored: %w "+
				"(pass -token-stdin to give one, or run: docpreview zrok invite <email>)", err)
		}
	}

	return enableZrokHere(ctx, st, cfg, token, *description)
}

// cmdZrokDisable removes this host's environment from the account.
func cmdZrokDisable(args []string) error {
	fs := flag.NewFlagSet("zrok disable", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	yes := fs.Bool("yes", false, "required: this takes every published preview URL down")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes {
		return errors.New("this deletes every share published through this environment, " +
			"so every preview URL stops answering until the daemon republishes; pass -yes")
	}

	cfg, st, err := openZrokStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	if err := useStoredOrProjectZrok(ctx, st, cfg); err != nil {
		return err
	}
	if err := expose.ZrokDisable(ctx); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "the zrok environment was disabled")
	fmt.Fprintln(os.Stderr, "the reserved names survive, so republishing restores the same URLs")
	return nil
}

// refuseIfZrokEnrolled stops a second signup, which is what was asked for.
//
// Either root being enabled counts. The point is not tidiness: a second account means a second set
// of names, and the previews advertised in every existing pull request comment live on the first.
func refuseIfZrokEnrolled(ctx context.Context, st *store.Store, cfg config.Server) error {
	scope, err := storedZrokScope(ctx, st)
	if err != nil {
		return err
	}
	state, err := expose.InspectZrokRoots(cfg.ZrokDir(), scope)
	if err != nil {
		return err
	}
	switch {
	case state.Project.Enabled:
		return fmt.Errorf("a zrok environment is already enrolled at %s; "+
			"there is no need to sign up again (docpreview zrok status)", state.Project.Path)
	case state.System.Enabled:
		return fmt.Errorf("this machine already has an enrolled zrok environment at %s; "+
			"use it with 'docpreview zrok use system', or disable it first if you want a "+
			"separate account for docpreview", state.System.Path)
	}
	return nil
}

// useStoredOrProjectZrok points this process at the right root for a one-shot command.
//
// The stored choice when there is one, and the project root otherwise — the same default
// applyZrokScope uses for a daemon with nothing enrolled, so a CLI enrolment lands where the
// daemon will look for it.
func useStoredOrProjectZrok(ctx context.Context, st *store.Store, cfg config.Server) error {
	scope, err := storedZrokScope(ctx, st)
	if err != nil {
		return err
	}
	if scope == "" {
		scope = expose.ZrokProject
	}
	return expose.UseZrokRoot(scope, cfg.ZrokDir())
}

// enableZrokHere enrols the selected root and records the choice.
func enableZrokHere(ctx context.Context, st *store.Store, cfg config.Server,
	token vault.Secret, description string) error {

	if err := useStoredOrProjectZrok(ctx, st, cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.ZrokDir(), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", cfg.ZrokDir(), err)
	}
	if err := expose.ZrokEnable(ctx, token, description); err != nil {
		return err
	}

	// Recorded now, because enrolment is the moment the answer stops being a guess.
	scope := expose.ZrokScopeInForce()
	if err := st.SetSetting(ctx, store.SettingZrokScope, string(scope)); err != nil {
		return fmt.Errorf("recording the zrok environment choice: %w", err)
	}

	fmt.Fprintf(os.Stderr, "this host is enrolled in the %s zrok environment\n", scope)
	fmt.Fprintln(os.Stderr, "set exposer.kind: zrok2 in the config, then restart the daemon")
	return nil
}

// storeZrokAccountToken keeps the account token in the vault when the vault is open.
//
// A locked vault is not fatal here. The token has already been created — refusing to continue
// would leave an account whose token exists only in this process, which is worse than not storing
// it. So it warns and says how to store it later.
func storeZrokAccountToken(ctx context.Context, st *store.Store, cfg config.Server,
	token vault.Secret) error {

	v, err := vault.OpenFrom(cfg.VaultPath(), cfg.KeySource())
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not store the zrok account token in the vault: %v\n", err)
		fmt.Fprintln(os.Stderr, "the environment enrolled below still works — zrok keeps its own copy —")
		fmt.Fprintln(os.Stderr, "but store it so a re-enrolment does not need the email again:")
		fmt.Fprintf(os.Stderr, "  docpreview vault set %s\n", vault.KeyZrokAccountToken)
		return nil
	}
	if err := v.Set(vault.KeyZrokAccountToken, token); err != nil {
		return fmt.Errorf("storing the zrok account token: %w", err)
	}
	fmt.Fprintf(os.Stderr, "the account token is in the vault as %s\n", vault.KeyZrokAccountToken)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(no endpoint recorded)"
	}
	return s
}
