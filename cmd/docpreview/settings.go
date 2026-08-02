package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// cmdSettings reads and writes the settings the daemon keeps in its database rather than in the
// config file.
//
// These have a dashboard field each, and this command exists for the same reason `console
// password` does: the dashboard is behind a login, so a script cannot reach it, and every field
// on it is a field somebody eventually wants to set from a provisioning script or a restart
// wrapper such as `Restart-Docpreview.ps1 -Prefix a`, which cannot follow a redirect to a login
// form the way a browser can.
func cmdSettings(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("settings: no subcommand")
	}

	switch args[0] {
	case "prefix":
		return cmdSettingsPrefix(args[1:])
	default:
		usage()
		return fmt.Errorf("settings: unknown subcommand %q", args[0])
	}
}

// cmdSettingsPrefix reads or sets the installation's hostname prefix.
//
// Given no argument it prints the current value, the same asymmetry as `console oauth-domains`
// and for the same reason: this is checked far more often than it is changed.
//
// **A running daemon does not pick this up.** It reads the prefix once at startup and holds it in
// memory, because it is consulted on every publish and a database read per publish would be a
// pointless one. So this is a set-then-start command, which is exactly how the restart script
// uses it. The dashboard field remains the way to change it on a daemon that is already up.
func cmdSettingsPrefix(args []string) error {
	fs := flag.NewFlagSet("settings prefix", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	clear := fs.Bool("clear", false, "publish with no prefix, the default")
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
		if err := st.ClearSetting(ctx, store.SettingNamePrefix); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "prefix cleared; the next publish uses the bare preview name")
		fmt.Fprintln(os.Stderr, "previews already published keep their names until they rebuild")
		return nil
	}

	if fs.NArg() == 0 {
		v, _, err := st.Setting(ctx, store.SettingNamePrefix)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Println("(none — previews publish under their bare names)")
			return nil
		}
		fmt.Println(v)
		return nil
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("settings prefix takes one value, got %d", fs.NArg())
	}

	// model.NamePrefix does the normalizing — including the trailing "-" that everybody types,
	// which is not part of the value: the separator is added by whatever builds the hostname, so
	// storing "a-" would publish "a--docs-main". Validated by the same function the API route
	// uses, so the command and the dashboard field cannot disagree about what is acceptable.
	prefix := model.NamePrefix(fs.Arg(0))
	if why := model.ValidPrefix(prefix); why != "" {
		return errors.New(why)
	}

	if err := st.SetSetting(ctx, store.SettingNamePrefix, prefix); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prefix set to %q; previews publish as %s-<name>\n", prefix, prefix)
	fmt.Fprintln(os.Stderr, "a running daemon reads this at startup — restart it, or set the field "+
		"on the dashboard instead")

	// Said every time, because the whole point of the prefix is two installations on one exposer
	// account and the failure it prevents is silent: without distinct prefixes they collide on
	// names, and each startup reaps the other's live shares.
	fmt.Fprintln(os.Stderr, "give each installation sharing an exposer account a different prefix")
	return nil
}
