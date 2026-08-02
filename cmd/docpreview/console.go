package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/daemon"
	"github.com/netfoundry/docpreview/internal/store"
)

// cmdConsole manages the password on the admin surface.
//
// A CLI command rather than only a field on the dashboard, because of an ordering problem that
// the dashboard cannot solve on its own: the surface that would set the first password is the
// surface being protected. On a host reachable only over a network — the case this whole feature
// exists for — there is no first login without this.
//
// It writes to the same sqlite file the daemon is using, and the daemon reads the hash on every
// check rather than caching it, so a password set here takes effect immediately with no
// restart. WAL mode plus busy_timeout makes the concurrent write safe.
func cmdConsole(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("console: no subcommand")
	}

	switch args[0] {
	case "password":
		return cmdConsolePassword(args[1:])
	case "oauth-domains":
		return cmdConsoleOAuthDomains(args[1:])
	case "status":
		return cmdConsoleStatus(args[1:])
	default:
		usage()
		return fmt.Errorf("console: unknown subcommand %q", args[0])
	}
}

// openConsoleStore loads the config and opens the database the daemon is using.
//
// Shared by both subcommands. The daemon may be running: sqlite is in WAL mode with a busy
// timeout, so a concurrent write is safe, and the daemon reads the hashes on every check rather
// than caching them — so a password set here applies without a restart.
func openConsoleStore(configPath string) (config.Server, *store.Store, error) {
	cfg, err := config.LoadServer(configPath)
	if err != nil {
		return config.Server{}, nil, err
	}
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return cfg, nil, err
	}
	return cfg, st, nil
}

// cmdConsolePassword sets or clears the console password.
//
// The password comes from stdin, never from an argument. An argument is in the shell's history,
// in `ps` output for as long as the process runs, and in whatever logs a CI system keeps — the
// same reasoning that makes `vault set` read stdin.
func cmdConsolePassword(args []string) error {
	fs := flag.NewFlagSet("console password", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	roleFlag := fs.String("role", "admin", "which password: admin or viewer")
	clear := fs.Bool("clear", false, "remove this role's password instead of setting one")
	if err := fs.Parse(args); err != nil {
		return err
	}

	role := daemon.Role(strings.ToLower(strings.TrimSpace(*roleFlag)))
	if role != daemon.RoleAdmin && role != daemon.RoleViewer {
		return fmt.Errorf("-role %q: use admin or viewer", *roleFlag)
	}

	_, st, err := openConsoleStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()

	if *clear {
		if err := daemon.ClearConsolePassword(ctx, st, role); err != nil {
			return err
		}
		switch role {
		case daemon.RoleAdmin:
			fmt.Fprintln(os.Stderr, "admin password removed; writes are loopback-only again")
		default:
			fmt.Fprintln(os.Stderr, "viewer password removed; anyone who can reach the "+
				"dashboard can read it")
		}
		return nil
	}

	pw, err := readStdin()
	if err != nil {
		return err
	}
	// The newline a shell appends is not part of the password. Getting this wrong produces a
	// password only a program can type, which is the worst outcome for something whose whole
	// purpose is being typed into a form.
	password := string(trimTrailingNewline(pw))
	if password == "" {
		return errors.New("refusing to set an empty password; use -clear to remove it")
	}

	if err := daemon.SetConsolePassword(ctx, st, role, password); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "%s password set\n", role)
	if role == daemon.RoleViewer {
		fmt.Fprintln(os.Stderr, "the dashboard now asks for a login before showing anything")
	} else {
		fmt.Fprintln(os.Stderr, "projects and credentials now accept a login from anywhere "+
			"this daemon is reachable")
	}
	fmt.Fprintln(os.Stderr, "loopback and named overlay identities still work without one")
	return nil
}

// cmdConsoleOAuthDomains sets the email domains a Google sign-in may come from.
//
// Given no argument it prints the current list. That asymmetry is deliberate: this is the value
// somebody checks more often than they change, and a command that needed a subcommand to read
// its own setting would be one nobody remembers.
//
// A Google sign-in grants **viewer** and never admin, so this list decides who can read the
// dashboard and nothing else. Admin remains password-only.
func cmdConsoleOAuthDomains(args []string) error {
	fs := flag.NewFlagSet("console oauth-domains", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	clear := fs.Bool("clear", false, "allow no domains, which disables Google sign-in")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	_, st, err := openConsoleStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	if *clear {
		if err := st.ClearSetting(ctx, daemon.SettingOAuthDomains); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "no domains allowed; Google sign-in is off")
		return nil
	}

	if fs.NArg() == 0 {
		v, _, err := st.Setting(ctx, daemon.SettingOAuthDomains)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Println("(none — Google sign-in is off)")
			return nil
		}
		fmt.Println(v)
		return nil
	}

	// Accepted as arguments or as one comma-separated value, because both are what somebody
	// types. Normalized to a comma-separated string, and a leading "@" is trimmed: "@acme.com"
	// is how a person writes a domain and refusing it would be pedantry.
	var domains []string
	for _, a := range fs.Args() {
		for _, d := range strings.Split(a, ",") {
			d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
			if d == "" {
				continue
			}
			// A domain, not a pattern. A wildcard here would be read as "any subdomain" and
			// is not implemented — the check is an exact match on the part after the last "@",
			// deliberately, because a suffix test would let evil-acme.com through a list
			// naming acme.com.
			if strings.ContainsAny(d, "*@ /") {
				return fmt.Errorf("%q is not a domain: write it as acme.com", d)
			}
			domains = append(domains, d)
		}
	}
	if len(domains) == 0 {
		return errors.New("no domains given; use -clear to allow none")
	}

	if err := st.SetSetting(ctx, daemon.SettingOAuthDomains, strings.Join(domains, ",")); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Google sign-in will accept an address at: %s\n", strings.Join(domains, ", "))
	fmt.Fprintln(os.Stderr, "it grants viewer; admin still needs the admin password")
	return nil
}

// cmdConsoleStatus says whether a password is set, without revealing anything about it.
//
// Reading the config and reasoning about listeners answers only half of "is my admin surface
// protected", and a hash in a database is not something an operator can eyeball.
func cmdConsoleStatus(args []string) error {
	fs := flag.NewFlagSet("console status", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, st, err := openConsoleStore(*configPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	admin, err := daemon.ConsolePasswordSet(ctx, st, daemon.RoleAdmin)
	if err != nil {
		return err
	}
	viewer, err := daemon.ConsolePasswordSet(ctx, st, daemon.RoleViewer)
	if err != nil {
		return err
	}

	fmt.Printf("admin password:  %s\n", yesNo(admin, "set", "none — writes are loopback-only"))
	fmt.Printf("viewer password: %s\n", yesNo(viewer, "set", "none — reading is open to anyone who can reach it"))

	domains, _, err := st.Setting(ctx, daemon.SettingOAuthDomains)
	if err != nil {
		return err
	}
	fmt.Printf("google sign-in:  %s\n", yesNo(domains != "",
		"grants viewer for an address at "+domains, "off — no domains allowed"))

	// The listeners, because a password is only half the answer. A loopback-only daemon with no
	// password is not exposed; the same daemon behind a tunnel is.
	for _, l := range cfg.Listeners {
		fmt.Printf("listener: %s\n", l.Describe())
		if l.Ziti != nil && len(l.Ziti.AdminIdentities) > 0 {
			fmt.Printf("  admin identities: %s\n", strings.Join(l.Ziti.AdminIdentities, ", "))
		}
	}

	// What is publishing the dashboard decides whether any of the above is reachable, and two
	// of the three exposers can do the authentication themselves. Said here because this is the
	// command somebody runs when asking "is this thing exposed".
	if !viewer {
		fmt.Println()
		fmt.Println("With no viewer password, anything that can reach this daemon can read it.")
		fmt.Println("If a zrok or Frontdoor share publishes the dashboard, gate the share on")
		fmt.Println("OAuth and an email domain instead — see www/docs/reference/security.md.")
	}
	return nil
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
