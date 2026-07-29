package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/zitiadmin"
)

func cmdConfigure(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("configure: no subcommand")
	}
	switch args[0] {
	case "ziti":
		return cmdConfigureZiti(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown configure subcommand %q", args[0])
	}
}

// cmdConfigureZiti provisions an OpenZiti network and points docpreview at it.
//
// The target is the four-line trial:
//
//	get the ziti CLI
//	ziti edge quickstart
//	docpreview configure ziti
//	docpreview serve
//
// which means every flag has to default to what the quickstart produces, and
// re-running has to be safe. People run a setup command twice — after reading
// the output, after changing one flag, after a failure halfway through — and a
// command that errors on the second run teaches them to tear the network down
// before every attempt.
//
// It writes the config file as well as the controller objects, because
// provisioning a network docpreview is not configured to use leaves the
// operator to copy four values into YAML by hand, which is exactly the step
// that makes people give up.
func cmdConfigureZiti(args []string) error {
	fs := flag.NewFlagSet("configure ziti", flag.ExitOnError)

	configPath := fs.String("config", defaultConfigPath(), "docpreview config file to write")
	controller := fs.String("controller", "https://localhost:1280", "edge management API of the ziti controller")
	username := fs.String("username", "admin", "controller admin username")
	password := fs.String("password", "admin", "controller admin password")
	domain := fs.String("domain", "docpreview.ziti", "DNS suffix previews are served under")
	service := fs.String("service", "docpreview-svc", "the wildcard service carrying every preview")
	adminService := fs.String("admin-service", "docpreview-admin",
		"service for the dashboard and webhook ingress; empty leaves the ingress on TCP only")
	hostIdentity := fs.String("host-identity", "docpreview-host", "name of docpreview's own hosting identity")
	reviewer := fs.String("reviewer", "reviewer-alice",
		"name of a sample reviewer identity; empty creates none")
	readerRole := fs.String("reader-role", "docpreview-reader",
		"role attribute the Dial policy is keyed on")
	prefix := fs.String("prefix", "docpreview-", "name prefix for the config and policies")
	outDir := fs.String("out-dir", "", "where to write the identity file and reviewer token "+
		"(default: <data_dir>/ziti)")
	noWrite := fs.Bool("no-config", false, "provision the network but leave the config file alone")

	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	if *outDir == "" {
		*outDir = filepath.Join(cfg.DataDir, "ziti")
	}

	log := newLogger("info")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := zitiadmin.Provision(ctx, zitiadmin.Options{
		Controller:   *controller,
		Username:     *username,
		Password:     *password,
		Domain:       *domain,
		Service:      *service,
		AdminService: *adminService,
		HostIdentity: *hostIdentity,
		Reviewer:     *reviewer,
		ReaderRole:   *readerRole,
		Prefix:       *prefix,
		OutDir:       *outDir,
	}, log)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nOn the controller\n-----------------\n")
	for _, what := range result.Created {
		fmt.Fprintf(os.Stderr, "  created  %s\n", what)
	}
	for _, what := range result.Reused {
		fmt.Fprintf(os.Stderr, "  present  %s\n", what)
	}

	// Applied before the -no-config branch so that the YAML it prints is the
	// same settings the file would have received, rather than whatever the
	// existing config happened to say.
	cfg.Exposer.Kind = "ziti"
	cfg.Exposer.Ziti.IdentityFile = result.HostIdentityFile
	cfg.Exposer.Ziti.Service = *service
	cfg.Exposer.Ziti.Domain = *domain

	if *noWrite {
		printZitiNextSteps(cfg, result, *configPath, false)
		return nil
	}

	// The dashboard keeps its loopback address even when it also gets an
	// overlay listener. Removing it would lock the operator out of the thing
	// they just set up until their own tunneler is enrolled and running, which
	// is the wrong order to discover a mistake in.
	if *adminService != "" {
		cfg.Listeners = []config.Listener{
			{TCP: cfg.FirstTCPAddr()},
			{Ziti: &config.ZitiListener{
				IdentityFile: result.HostIdentityFile,
				Service:      *adminService,
			}},
		}
	}

	path, err := filepath.Abs(*configPath)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", *configPath, err)
	}
	if err := writeConfig(path, renderConfig(cfg)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nWrote %s\n", path)

	printZitiNextSteps(cfg, result, path, true)
	return nil
}

// printZitiNextSteps says what to do with the two files that were produced.
//
// The reviewer token is the part nobody guesses: it is not a URL and not a
// password, and the only thing to do with it is import the file into a
// tunneler. Saying where it landed and which dialog eats it is the difference
// between this working and the operator concluding the overlay is broken.
func printZitiNextSteps(cfg config.Server, r *zitiadmin.Result, path string, wrote bool) {
	fmt.Fprintf(os.Stderr, "\nFiles\n-----\n")
	fmt.Fprintf(os.Stderr, "  hosting identity  %s\n", r.HostIdentityFile)
	switch {
	case r.ReviewerJWTFile != "":
		fmt.Fprintf(os.Stderr, "  reviewer token    %s\n", r.ReviewerJWTFile)
	case r.ReviewerEnrolled:
		fmt.Fprintf(os.Stderr, "  reviewer token    already used — the identity is enrolled somewhere.\n"+
			"                    Mint another with a different -reviewer name.\n")
	}

	fmt.Fprintf(os.Stderr, "\nNext\n----\n")
	step := 1
	next := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "%2d. %s\n", step, fmt.Sprintf(format, a...))
		step++
	}

	if !wrote {
		next("Point docpreview at it (-no-config skipped this):\n"+
			"     exposer:\n"+
			"       kind: ziti\n"+
			"       ziti:\n"+
			"         identity_file: %q\n"+
			"         service: %q\n"+
			"         domain: %q",
			r.HostIdentityFile, cfg.Exposer.Ziti.Service, cfg.Exposer.Ziti.Domain)
	}
	if r.ReviewerJWTFile != "" {
		next("Import %s into Ziti Desktop Edge:\n"+
			"     the + button on the identity list, \"Ziti JWT\", pick that file,\n"+
			"     then switch the new identity on. On Linux or macOS:\n"+
			"       ziti-edge-tunnel add --jwt \"$(cat %s)\" --identity reviewer",
			filepath.Base(r.ReviewerJWTFile), r.ReviewerJWTFile)
	}
	next("Check it:         docpreview doctor -config %s", path)
	next("Publish one:      docpreview preview -build ./www")
	next("Run the daemon:   docpreview serve -config %s", path)
	fmt.Fprintf(os.Stderr, "\nPreviews will be at http://<branch>.%s/ "+
		"— only from a machine running the tunneler.\n", cfg.Exposer.Ziti.Domain)
}
