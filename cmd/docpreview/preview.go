package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
	"github.com/netfoundry/docpreview/internal/preview"
)

// cmdPreview publishes one directory through the configured exposer and holds
// it open until interrupted.
//
// This exists because the rest of docpreview cannot be tried without a GitHub
// App, and creating a GitHub App is a ten-minute detour through a web form that
// nobody should have to take on the strength of a README. Here the exposer, the
// build, the baseUrl verification, and the static server are all exercised —
// everything except the webhook and the comment — with no credentials beyond
// whatever the exposer itself needs.
//
// It is also the fastest way to answer "why does my preview look unstyled",
// because it runs the same baseUrl check the daemon does and prints the same
// error, without needing a pull request to trigger it.
func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	logLevel := fs.String("log-level", "info", "debug, info, warn, or error")
	name := fs.String("name", "", "public hostname label (default: the directory name)")
	baseURL := fs.String("base-url", "", "path prefix to serve under (default: from .docpreview.yml, or /)")
	doBuild := fs.Bool("build", false, "run the site build first, instead of serving the directory as-is")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	target := fs.Arg(0)
	if target == "" {
		return fmt.Errorf("usage: docpreview preview [-build] <directory>")
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", target, err)
	}

	w, err := setup(*configPath, *logLevel)
	if err != nil {
		return err
	}
	defer w.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := w.exposer.Validate(ctx); err != nil {
		return fmt.Errorf("exposer %s: %w", w.exposer.Kind(), err)
	}

	serveDir := target
	repoCfg, err := config.LoadRepoConfig(target)
	if err != nil {
		return err
	}
	if *baseURL != "" {
		normalized, err := config.NormalizeBaseURL(*baseURL)
		if err != nil {
			return fmt.Errorf("-base-url: %w", err)
		}
		repoCfg.Build.BaseURL = normalized
	}

	if *doBuild {
		w.log.Info("building", "dir", target, "base_url", repoCfg.Build.BaseURL)
		builder := pipeline.NewBuilder(w.cfg.Build, w.log)
		// Straight to the terminal: this is an interactive command, and the
		// operator watching it is the live tail.
		result, err := builder.Build(ctx, &pipeline.Workspace{Dir: target}, repoCfg, os.Stderr)
		if err != nil {
			return err
		}
		serveDir = result.OutputDir
		w.log.Info("built", "output", serveDir, "took", result.Duration.Round(time.Second))
	}

	site, err := preview.New(serveDir, repoCfg.Build.BaseURL)
	if err != nil {
		return err
	}

	label := *name
	if label == "" {
		label = model.SanitizeName(filepath.Base(target))
	}

	pub, err := w.exposer.Publish(ctx, expose.Spec{
		// A stable ID keeps repeat runs of the same directory from
		// accumulating orphaned shares on the exposer.
		PreviewID: "manual-" + label,
		Name:      label,
		BaseURL:   repoCfg.Build.BaseURL,
	}, site)
	if err != nil {
		return err
	}
	defer pub.Close()

	fmt.Fprintf(os.Stderr, "\n  %s\n\n  serving %s\n  Ctrl-C to stop.\n\n",
		pub.URL, serveDir)

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "withdrawing preview")
	return nil
}
