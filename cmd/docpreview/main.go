// Command docpreview builds documentation previews for pull requests and posts
// the resulting URL back to the pull request.
//
// It is a single binary with no runtime dependencies beyond git, a Node
// toolchain (or Docker), and whichever exposer is configured. Run
// `docpreview serve` to start the daemon; the vault and doctor subcommands
// exist to make the first ten minutes of setup survivable.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/daemon"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/scm/github"
	"github.com/netfoundry/docpreview/internal/scm/local"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "docpreview: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "configure":
		return cmdConfigure(args[1:])
	case "preview":
		return cmdPreview(args[1:])
	case "sim":
		return cmdSim(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "webhook-only":
		return cmdWebhookOnly(args[1:])
	case "webhook-check":
		return cmdWebhookCheck(args[1:])
	case "dashboard-only":
		return cmdDashboardOnly(args[1:])
	case "vault":
		return cmdVault(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `docpreview — documentation previews for pull requests

Usage:
  docpreview init      [-advanced] [-yes] One question, then write a config
  docpreview configure ziti               Provision an OpenZiti network and use it
  docpreview preview [-build] <dir>       Publish one directory, no GitHub needed
  docpreview sim     <subcommand>         Local git remotes that trigger builds
  docpreview serve   [-config FILE]       Run the webhook daemon
  docpreview doctor  [-config FILE]       Check the configuration and exit
  docpreview vault   <subcommand>         Manage stored credentials

Reaching a loopback daemon from the internet. All three take -zrok-name, never
-config, and each publishes one thing rather than the whole daemon:

  docpreview webhook-only   -zrok-name N   Publish POST /webhook/* and nothing else
  docpreview dashboard-only -zrok-name N   Publish the dashboard, read-only
  docpreview webhook-check  [-config FILE] Send one signed ping and report the answer

Sharing the daemon itself would publish /api/secrets, whose write gate only asks
whether the daemon's own listeners are loopback — and they are.

Publish one directory, no source control at all:

  docpreview init
  docpreview preview -build ./www

Or previews that are reachable only through an OpenZiti tunneler, from nothing:

  ziti edge quickstart            # in another terminal, leave it running
  docpreview configure ziti       # every object, both identities, the config
  docpreview serve

Or feel the whole push-build-comment flow, still with no GitHub App:

  docpreview sim init mydocs      # a bare repo whose pushes trigger builds
  docpreview serve                # in another terminal
  git remote add preview <path>
  git push preview my-branch      # watch http://127.0.0.1:8471/pr

Vault subcommands:
  docpreview vault keygen [-out FILE]     Mint a new master key
  docpreview vault list   [-config FILE]  List stored secret names
  docpreview vault set    <key> [-file F] Store a secret (from -file, or stdin)
  docpreview vault delete <key>           Remove a secret

The master key comes from vault.key_source in the config — a file, or a command
that prints it — then $DOCPREVIEW_MASTER_KEY, then a prompt on a terminal. With
none of the three the daemon starts locked and is unlocked from the dashboard.

  vault:
    key_source: "file:/etc/docpreview/master.key"
    key_source: "exec:op read op://ops/docpreview/master-key"

Mint one straight into a file, keeping it out of your shell history:

  docpreview vault keygen -out /etc/docpreview/master.key
`)
}

func defaultConfigPath() string {
	if p := os.Getenv("DOCPREVIEW_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "docpreview.yml"
	}
	return filepath.Join(home, ".docpreview", "config.yml")
}

func newLogger(level string) *slog.Logger {
	return newLoggerTo(level, "")
}

// newLoggerTo builds the logger, optionally teeing it to a file.
//
// stderr alone means the daemon's own log is readable by whoever started the process and
// by nobody else — which is most of the time, since it is normally started in a terminal
// somebody has since closed. "Are there logs for this?" had no good answer.
//
// Both, not either. A file only would take the output away from the terminal it is being
// watched in, and this is the one process where somebody is often watching both.
//
// A failure to open the file is a warning on the logger that does work, not a refusal to
// start: a daemon that will not boot because it cannot write a log is worse than a daemon
// with no log.
func newLoggerTo(level, path string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}

	var w io.Writer = os.Stderr
	var openErr error
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			openErr = err
		} else if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			openErr = err
		} else {
			// Never closed, deliberately: it lives as long as the process, and closing it
			// on any path short of exit would leave the logger writing to a closed file.
			// Appended to rather than truncated, because the interesting content is
			// usually from before the restart that is being investigated.
			w = io.MultiWriter(os.Stderr, f)
		}
	}

	log := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
	if openErr != nil {
		log.Warn("log_file could not be opened; logging to stderr only",
			"path", path, "error", openErr)
	}
	return log
}

// wiring is everything a command needs after configuration is resolved.
type wiring struct {
	cfg     config.Server
	log     *slog.Logger
	store   *store.Store
	exposer expose.Exposer
	clients map[model.Platform]scm.Client

	// vault is opened on first use, not at startup. Opening it means producing
	// a master key, and a setup with the local exposer and no source-control
	// integration needs no secrets at all — demanding a passphrase to serve a
	// directory on loopback would be pure ceremony.
	vaultOnce sync.Once
	vaultRef  *vault.Vault
	vaultErr  error
	vaultPath string

	// unlockedMu guards unlocked, which is written once during cmdServe wiring
	// and read from request and build goroutines afterwards.
	unlockedMu sync.Mutex
	// unlocked returns a vault opened after startup, by the setup page. Nil when
	// nothing can unlock one — every command except serve.
	unlocked func() *vault.Vault
}

// setUnlockSource installs the runtime source of an unlocked vault.
func (w *wiring) setUnlockSource(f func() *vault.Vault) {
	w.unlockedMu.Lock()
	defer w.unlockedMu.Unlock()
	w.unlocked = f
}

// CurrentVault returns an open vault, preferring one unlocked at runtime.
//
// Vault caches its result — including its error — forever, which is right for a
// one-shot command and wrong for a daemon: a vault that was locked at boot must
// not still look locked here after somebody unlocks it from the dashboard. So
// the runtime source is consulted first, and the once-opened one is the
// fallback for every command that has no dashboard.
func (w *wiring) CurrentVault() (*vault.Vault, error) {
	w.unlockedMu.Lock()
	f := w.unlocked
	w.unlockedMu.Unlock()

	if f != nil {
		if v := f(); v != nil {
			return v, nil
		}
	}
	return w.Vault()
}

func (w *wiring) Close() {
	if w.exposer != nil {
		w.exposer.Close()
	}
	if w.store != nil {
		w.store.Close()
	}
}

// Vault opens the credential store, once.
func (w *wiring) Vault() (*vault.Vault, error) {
	w.vaultOnce.Do(func() {
		w.vaultRef, w.vaultErr = vault.OpenFrom(w.vaultPath, w.cfg.KeySource())
	})
	return w.vaultRef, w.vaultErr
}

// setup builds every component, in dependency order, failing on the first
// problem with a message that names the fix.
//
// Source-control integration is optional. An App ID of zero means "not wired up
// yet", which is the state everyone is in for their first ten minutes and the
// permanent state of anyone using Bitbucket. Refusing to start in that state
// would make `docpreview preview` — the whole point of which is to try this
// before committing to a GitHub App — impossible to run.
func setup(configPath, logLevel string) (*wiring, error) {
	cfg, err := config.LoadServer(configPath)
	if err != nil {
		return nil, err
	}
	// Teed to a file when the config names one. After LoadServer, because the path comes
	// from the config — so a failure to parse the config still logs, to stderr.
	log := newLoggerTo(logLevel, cfg.LogFile)

	for _, dir := range []string{cfg.DataDir, cfg.WorkspacesDir(), cfg.ArtifactsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}

	w := &wiring{
		cfg:       cfg,
		log:       log,
		store:     st,
		clients:   map[model.Platform]scm.Client{},
		vaultPath: cfg.VaultPath(),
	}

	w.exposer, err = buildExposer(w, cfg, log)
	if err != nil {
		w.Close()
		return nil, err
	}

	if cfg.GitHub.AppID != 0 {
		v, err := w.Vault()
		switch {
		case errors.Is(err, vault.ErrLocked):
			// A locked vault is not a configuration error, it is a daemon that
			// has not been unlocked yet — and the page that unlocks it is
			// served by this process. Failing here made that page unreachable
			// and left the terminal as the only way in.
			//
			// Start without a GitHub client. serve installs one the moment the
			// vault opens, and until then /webhook/github answers 501.
			log.Warn("github is configured but the vault is locked, so no GitHub client was built",
				"fix", "unlock the vault from the dashboard, or set vault.key_source",
				"key_source", cfg.KeySource().Describe(),
				"vault", w.vaultPath)
		case err != nil:
			w.Close()
			return nil, err
		default:
			gh, err := github.New(cfg.GitHub, v, log)
			if err != nil {
				w.Close()
				return nil, fmt.Errorf("configuring github: %w", err)
			}
			w.clients[model.PlatformGitHub] = gh
		}
	}

	if cfg.Local.Enabled {
		lc, err := local.New(cfg.Local, st, log)
		if err != nil {
			w.Close()
			return nil, fmt.Errorf("configuring the local platform: %w", err)
		}
		w.clients[model.PlatformLocal] = lc
	}

	return w, nil
}

// hasSCM reports whether any source-control platform is configured.
//
// Configured, not built: an App ID with a locked vault has no client yet, but
// warning "no source control is configured" in that state would name the wrong
// problem — setup already logged the real one.
func (w *wiring) hasSCM() bool { return len(w.clients) > 0 || w.cfg.GitHub.AppID != 0 }

// localOrigin is the scheme and authority the daemon answers on, for building
// preview URLs under the local exposer.
//
// The first TCP listener wins. A wildcard bind is rewritten to loopback,
// because "http://0.0.0.0:8471/" is not an address a browser can use — it means
// "every interface" to a listener and nothing at all to a client.
func localOrigin(cfg config.Server) string {
	for _, l := range cfg.Listeners {
		if l.TCP == "" {
			continue
		}
		host, port, err := net.SplitHostPort(l.TCP)
		if err != nil {
			continue
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		return "http://" + net.JoinHostPort(host, port)
	}
	return ""
}

func buildExposer(w *wiring, cfg config.Server, log *slog.Logger) (expose.Exposer, error) {
	switch cfg.Exposer.Kind {
	case "local":
		// Previews are served from the daemon's own listener, so their URLs
		// need the address it answers on. A ziti-only listener has no such
		// address, and localOrigin returns "" — the comment then carries a
		// site-relative link, which is correct from a browser already on the
		// dashboard and useless from anywhere else. That is a property of
		// choosing the local exposer, not something to paper over.
		return expose.NewLocal(log, localOrigin(cfg)), nil
	case "zrok2":
		return expose.NewZrok(cfg.Exposer.Zrok, log)
	case "ziti":
		return expose.NewZiti(cfg.Exposer.Ziti, log)
	case "frontdoor":
		// A provider, not the token. Reading the vault here made a locked vault
		// fatal, and the page that unlocks it is served by this daemon — the
		// same boot-order trap the GitHub client was just lifted out of.
		return expose.NewFrontdoor(cfg.Exposer.Frontdoor, func() (vault.Secret, error) {
			v, err := w.CurrentVault()
			if err != nil {
				return vault.Secret{}, err
			}
			return v.MustGet(vault.KeyFrontdoorToken)
		}, log)
	default:
		return nil, fmt.Errorf("unknown exposer %q", cfg.Exposer.Kind)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	logLevel := fs.String("log-level", "info", "debug, info, warn, or error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	w, err := setup(*configPath, *logLevel)
	if err != nil {
		return err
	}
	defer w.Close()

	// This used to be fatal, on the reasoning that a daemon nothing can reach is
	// the worst kind of failure: healthy-looking and inert.
	//
	// It made the setup page unreachable. Configuring GitHub means storing a
	// private key and a webhook secret, the dashboard is where you now do that,
	// and refusing to boot until it is done leaves the only route back to a
	// terminal — which is the ceremony the page exists to remove.
	//
	// So: start, and say plainly on the way up that nothing can arrive yet.
	if !w.hasSCM() {
		w.log.Warn("no source control is configured, so no webhooks can arrive",
			"fix", "add github.app_id, or set local.enabled for the git simulator",
			"config", *configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Validate before binding a port. Discovering that the zrok environment is
	// not enabled after the first webhook arrives means a pull request comment
	// that never appears and no obvious place to look.
	if err := validate(ctx, w); err != nil {
		return err
	}

	secrets, err := buildSecrets(w)
	if err != nil {
		return err
	}

	// Probe once, at startup, and say so in the log either way. Which driver runs is
	// the difference between a pull request's build scripts executing in a container
	// and executing on this host, and an operator should be able to answer "which one
	// am I getting?" from the log rather than from a build.
	dockerOK, dockerWhy := pipeline.ProbeDocker(ctx)
	if dockerOK {
		w.log.Info("docker is available", "server_version", dockerWhy)
	} else {
		w.log.Warn("docker is not available, so no build can be isolated from this host",
			"reason", dockerWhy)
	}

	// No silent downgrade. Falling back to the local driver here would run branch
	// code on the host because a daemon failed to start, which is the one thing
	// build.allow_local_driver exists to make impossible — so builds fail instead,
	// with driverAllowed's message, and the operator decides.
	if !dockerOK && w.cfg.Build.Driver == config.DriverDocker {
		if w.cfg.Build.AllowLocalDriver {
			w.log.Warn("no docker, and the local driver is enabled; builds will run on this host",
				"fix", "start docker, or set build.driver: local to stop seeing this warning")
		} else {
			w.log.Error("no usable build driver: docker is unavailable and the local driver "+
				"is not enabled, so every build will fail",
				"fix", "start docker, or set build.allow_local_driver: true and accept that "+
					"pull request build scripts then run on this host")
		}
	}

	d := daemon.New(w.cfg, w.store, w.exposer, w.clients, w.log).WithBuildSecrets(secrets)

	// The setup page can change the vault while the daemon is serving, so it
	// gets a callback that re-derives everything read out of the vault at wiring
	// time: build.secrets and the redactor compiled from them, and the GitHub
	// client, which holds a parsed private key and the webhook secret.
	//
	// ingress is declared here rather than at its assignment because the
	// callback installs a client on it, and the callback has to exist before
	// NewIngress can be handed it.
	var admin *daemon.SecretsAdmin
	var ingress *daemon.Ingress
	admin = daemon.NewSecretsAdmin(w.cfg, w.log, func(changed string) {
		v := admin.Vault()
		if v == nil {
			return
		}
		next := map[string]string{}
		// Every shell-shaped key, injected under its own name. The same rule startup
		// uses, and it has to be the same rule: a token stored from the page would
		// otherwise apply only after a restart, which is the trap this whole callback
		// exists to avoid.
		for _, key := range v.Keys() {
			if !vault.IsBuildEnvKey(key) {
				continue
			}
			if s, err := v.Get(key); err == nil {
				next[key] = s.RevealString()
			}
		}
		for env, key := range w.cfg.Build.Secrets {
			s, err := v.Get(key)
			if err != nil {
				// A build secret that is configured but absent is a real
				// problem, and it already fails startup. At runtime the
				// operator is mid-setup, so log it and carry the rest rather
				// than taking the daemon down.
				w.log.Warn("build secret is not in the vault", "env", env, "key", key)
				continue
			}
			next[env] = s.RevealString()
		}
		d.SetBuildSecrets(next)
		rewireGitHub(w, d, ingress, v, changed)
		revalidateExposer(w, changed)
	})

	// From here on, anything that needs the vault sees the one the setup page
	// opened rather than the locked answer cached at boot.
	w.setUnlockSource(admin.Vault)

	// A project's own environment variables come out of the same vault, read at the
	// start of each build rather than cached here. That is deliberate: it needs no
	// entry in the rearm callback, and a token added from the projects page applies
	// to the next build — which is what the operator expects, since they are usually
	// looking at the build that just failed without it.
	d.SetProjectSecrets(func(platform, owner, repo string) map[string]string {
		v := admin.Vault()
		if v == nil {
			return nil
		}
		return v.RevealPrefix(vault.ProjectSecretPrefix(platform, owner, repo))
	})

	ingress = daemon.NewIngress(d, w.clients, w.store, w.log).
		WithSecrets(admin).
		WithProjects(daemon.NewProjectsAdmin(w.store, w.cfg, w.log).
			WithVault(admin.Vault).
			// So adding a project builds what is already open, instead of waiting for
			// somebody to push to a repository that was added precisely because nobody
			// had.
			WithScanner(d.ScanRepo).
			// So a build wedged on a slow registry can be stopped without restarting the
			// daemon, which was the only remedy.
			WithCanceller(d.CancelBuild).
			// Rebuilds the commit already on the row, which is what fixes a build that
			// failed for a reason outside the branch: a bad cache entry, a timeout, an
			// image since corrected.
			WithRebuilder(d.RebuildPreview).
			// So the form can grey out a driver that would fail rather than offering
			// it and letting the operator discover the refusal from a failed build.
			WithDocker(dockerOK, dockerWhy))

	listeners, err := daemon.Open(w.cfg.Listeners, w.log)
	if err != nil {
		return err
	}
	defer listeners.Close()

	srv := &http.Server{
		Handler:           ingress.Handler(),
		ReadHeaderTimeout: 10 * time.Second,

		// Hand the signal context to every request, so cancelling it unblocks
		// handlers that are deliberately long-lived.
		//
		// Shutdown waits for in-flight requests and does not cancel their
		// contexts. The server-sent-events handlers block until their client
		// disconnects, and the dashboard opens two of them the moment it loads
		// — so without this, one open browser tab makes every Ctrl-C sit for
		// the full 30-second shutdown timeout and then log a failure. Before
		// streaming existed every handler returned in milliseconds and this
		// never came up.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// One http.Server, several listeners, one goroutine each. Serve returns as
	// soon as its listener fails, and a failure on any one of them is fatal:
	// the remaining listeners would keep the process looking healthy while the
	// address someone is actually pointed at has gone away. Buffered by the
	// number of listeners so that a goroutine losing the race to report still
	// exits rather than blocking forever on send.
	errCh := make(chan error, len(listeners.Net))
	for i, ln := range listeners.Net {
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", listeners.Descriptions[i], err)
			}
		}()
	}
	w.log.Info("listening", "listeners", strings.Join(listeners.Descriptions, ", "),
		"exposer", w.exposer.Kind())

	daemonErr := make(chan error, 1)
	go func() { daemonErr <- d.Run(ctx) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		w.log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		w.log.Error("shutting down http server", "error", err)
	}
	return <-daemonErr
}

// rewireGitHub builds a GitHub client from the vault and installs it on the
// running daemon and ingress.
//
// This is the answer to the boot order. github.New reads the App private key and
// the webhook secret out of the vault, so a daemon started with a locked vault
// has no GitHub client — and the page that unlocks the vault is served by that
// daemon. So the client is built here instead, after the fact, whenever the
// vault gains or changes something it depends on.
//
// changed is the key that moved, empty when the vault was just unlocked. Both
// credentials are read at construction, so either one changing means rebuilding:
// a rotated webhook secret that left the old client in place would reject every
// delivery, and a rotated private key would fail every API call.
//
// A rebuild drops the cached installation token. That is the point — the token
// was minted by the key being replaced.
func rewireGitHub(w *wiring, d *daemon.Daemon, ingress *daemon.Ingress, v *vault.Vault, changed string) {
	if w.cfg.GitHub.AppID == 0 {
		return
	}
	switch changed {
	case "", vault.KeyGitHubPrivateKey, vault.KeyGitHubWebhookSec:
	default:
		return
	}

	gh, err := github.New(w.cfg.GitHub, v, w.log)
	if err != nil {
		// Not an error. Storing one of the two credentials and not yet the other
		// is the normal middle of setup, and this fires on the first of the two
		// every time. The page shows which are still missing.
		w.log.Info("github client not built yet", "reason", err)
		return
	}

	d.SetClient(model.PlatformGitHub, gh)
	ingress.SetClient(model.PlatformGitHub, gh)
	w.log.Info("github client installed", "app", w.cfg.GitHub.AppID)
}

// revalidateExposer re-runs the exposer's startup check after the vault changes.
//
// Startup downgrades a locked-vault validation failure to a warning, which means
// nothing has yet confirmed that the frontdoor gateway accepts this token. This
// is where that gets confirmed — at unlock, or when the token itself is stored.
//
// Advisory: it logs and returns. The alternative is taking a serving daemon down
// because a credential the operator is in the middle of typing is not right yet,
// and a publish that needs the token will report its own failure anyway.
func revalidateExposer(w *wiring, changed string) {
	switch changed {
	case "", vault.KeyFrontdoorToken:
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := w.exposer.Validate(ctx); err != nil {
		w.log.Warn("the exposer still does not validate", "exposer", w.exposer.Kind(), "error", err)
		return
	}
	w.log.Info("exposer validated after unlock", "exposer", w.exposer.Kind())
}

// validate runs every component's startup check.
//
// A check that cannot run because the vault is locked is not a failure. The
// frontdoor exposer authenticates with a token from the vault, so validating it
// on a locked daemon asks a question that has no answer yet — and failing here
// is the same trap as building the GitHub client during wiring: it hides the page
// that unlocks the vault behind a daemon that will not start. serve revalidates
// after unlock; see revalidateExposer.
func validate(ctx context.Context, w *wiring) error {
	if err := w.exposer.Validate(ctx); err != nil {
		if errors.Is(err, vault.ErrLocked) {
			w.log.Warn("the exposer could not be validated because the vault is locked",
				"exposer", w.exposer.Kind(),
				"fix", "unlock the vault from the dashboard, or set vault.key_source")
		} else {
			return fmt.Errorf("exposer %s: %w", w.exposer.Kind(), err)
		}
	}
	if gh, ok := w.clients[model.PlatformGitHub].(*github.Client); ok {
		if err := gh.Validate(ctx); err != nil {
			return err
		}
	}
	if lc, ok := w.clients[model.PlatformLocal].(*local.Client); ok {
		if err := lc.Validate(ctx); err != nil {
			return err
		}
	}
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf("config:  %s\n", *configPath)

	w, err := setup(*configPath, "info")
	if err != nil {
		return err
	}
	defer w.Close()

	fmt.Printf("data:    %s\n", w.cfg.DataDir)
	fmt.Printf("exposer: %s\n", w.cfg.Exposer.Kind)
	fmt.Printf("driver:  %s\n", w.cfg.Build.Driver)

	for i, l := range w.cfg.Listeners {
		label := "listen: "
		if i > 0 {
			label = "        "
		}
		fmt.Printf("%s %s\n", label, l.Describe())
	}

	// Report the vault only if something needed it. Opening it here purely to
	// count the keys would prompt for a passphrase during a health check that
	// otherwise requires no secrets.
	if v, err := w.Vault(); err == nil {
		fmt.Printf("vault:   %s (%d secrets)\n", w.cfg.VaultPath(), len(v.Keys()))
	}

	// Whether a restart needs a person is the thing an operator most wants to
	// know and cannot see from anywhere else.
	if src := w.cfg.KeySource(); src.IsZero() {
		fmt.Printf("key:     none — the daemon starts locked and is unlocked from the dashboard\n")
	} else {
		fmt.Printf("key:     %s\n", src.Describe())
	}

	// Read from the config rather than from w.clients. With app_id set and the
	// vault locked there is no client, and reporting "none — set github.app_id"
	// to somebody who has set github.app_id sends them to fix the wrong thing.
	switch {
	case !w.hasSCM():
		fmt.Printf("scm:     none — set local.enabled or github.app_id to receive webhooks\n")
	default:
		var names []string
		if w.cfg.GitHub.AppID != 0 {
			state := ""
			if _, ok := w.clients[model.PlatformGitHub]; !ok {
				state = ", not wired up: the vault is locked"
			}
			names = append(names, fmt.Sprintf("github (app %d%s)", w.cfg.GitHub.AppID, state))
		}
		if w.cfg.Local.Enabled {
			names = append(names, fmt.Sprintf("local (%s)", w.cfg.Local.ReposDir))
		}
		fmt.Printf("scm:     %s\n", strings.Join(names, ", "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := validate(ctx, w); err != nil {
		return err
	}
	if err := checkZitiListeners(w); err != nil {
		return err
	}
	fmt.Println("\nall checks passed")
	return nil
}

// checkZitiListeners proves each overlay listener can be bound, then lets it go.
//
// Worth the round trip because the two ways it fails — an identity that will
// not authenticate, and a service with no Bind policy naming it — are both
// invisible from the config file and both present at runtime as a dashboard
// that simply cannot be reached.
//
// TCP listeners are deliberately not bound. A port already in use is the
// expected state when the daemon is running, and failing a health check for
// that would be actively misleading. The overlay bind has the opposite
// problem — it will succeed against a running daemon and briefly create a
// second terminator — so doctor closes it immediately.
func checkZitiListeners(w *wiring) error {
	for i, l := range w.cfg.Listeners {
		if l.Ziti == nil {
			continue
		}
		open, err := daemon.Open([]config.Listener{l}, w.log)
		if err != nil {
			return fmt.Errorf("listeners[%d]: %w", i, err)
		}
		open.Close()
		fmt.Printf("bind:    %s ok\n", l.Describe())
	}
	return nil
}

func cmdVault(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("vault: no subcommand")
	}

	switch args[0] {
	case "keygen":
		return cmdVaultKeygen(args[1:])

	case "list":
		fs := flag.NewFlagSet("vault list", flag.ExitOnError)
		configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		v, err := openVault(*configPath)
		if err != nil {
			return err
		}
		keys := v.Keys()
		if len(keys) == 0 {
			fmt.Println("(vault is empty)")
			return nil
		}
		for _, k := range keys {
			fmt.Println(k)
		}
		return nil

	case "set":
		if len(args) < 2 {
			return errors.New("vault set: no key given")
		}
		key := args[1]
		fs := flag.NewFlagSet("vault set", flag.ExitOnError)
		configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
		file := fs.String("file", "", "read the value from this file instead of stdin")
		if err := fs.Parse(hoistFlags(fs, args[2:])); err != nil {
			return err
		}

		var value []byte
		var err error
		if *file != "" {
			value, err = os.ReadFile(*file)
			if err != nil {
				return fmt.Errorf("reading %s: %w", *file, err)
			}
		} else {
			value, err = readStdin()
			if err != nil {
				return err
			}
		}
		// Trailing newlines are what a shell adds, not what the credential is.
		// A PEM key keeps its internal newlines; a token loses the one the
		// terminal appended.
		value = trimTrailingNewline(value)
		if len(value) == 0 {
			return fmt.Errorf("refusing to store an empty value for %s", key)
		}

		v, err := openVault(*configPath)
		if err != nil {
			return err
		}
		if err := v.Set(key, vault.NewSecret(value)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "stored %s (%d bytes)\n", key, len(value))
		return nil

	case "delete":
		if len(args) < 2 {
			return errors.New("vault delete: no key given")
		}
		key := args[1]
		fs := flag.NewFlagSet("vault delete", flag.ExitOnError)
		configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
		if err := fs.Parse(hoistFlags(fs, args[2:])); err != nil {
			return err
		}
		v, err := openVault(*configPath)
		if err != nil {
			return err
		}
		if err := v.Delete(key); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "deleted %s\n", key)
		return nil

	default:
		usage()
		return fmt.Errorf("unknown vault subcommand %q", args[0])
	}
}

// cmdVaultKeygen mints a vault master key.
//
// Three output shapes, because there are three things people do with a fresh
// key. All three keep it out of shell history.
//
// With -out, it is written to a file at 0600 and never printed. This is the
// shape that pairs with vault.key_source, and therefore the shape that lets the
// daemon come back from a reboot without a person.
//
// Bare, the key goes to stdout and the guidance to stderr, so it can be piped
// straight into a password manager without capturing the prose. Pair with
// key_source: "exec:..." to read it back out of that manager at startup.
//
// With -shell, stdout is a single assignment statement in the shell's own
// syntax, so it can be evaluated directly:
//
//	docpreview vault keygen -shell | Invoke-Expression      # PowerShell
//	eval "$(docpreview vault keygen -shell)"                # sh
//
// That form is not merely less typing. Copy-pasting an assignment puts the key
// itself into shell history; piping puts only the pipeline there. It sets
// $DOCPREVIEW_MASTER_KEY, which still works and is the least preferred of the
// three — see vault.MasterKeyEnv.
func cmdVaultKeygen(args []string) error {
	fs := flag.NewFlagSet("vault keygen", flag.ExitOnError)
	shellFlag := fs.String("shell", "",
		"emit a shell assignment instead of a bare key: auto, powershell, sh, fish, or cmd")
	out := fs.String("out", "",
		"write the key to this file, for vault.key_source, instead of printing it")
	quiet := fs.Bool("quiet", false, "suppress the explanatory output on stderr")
	if err := fs.Parse(normalizeShellFlag(args)); err != nil {
		return err
	}
	if *out != "" && isFlagSet(fs, "shell") {
		return errors.New("vault keygen: -out and -shell do the same job two ways; pick one")
	}

	key, err := vault.GenerateIdentity()
	if err != nil {
		return err
	}

	if *out != "" {
		return writeKeyFile(*out, key, *quiet)
	}

	// The flag was not given at all: keep the pipe-to-a-password-manager shape.
	if !isFlagSet(fs, "shell") {
		if !*quiet {
			fmt.Fprintf(os.Stderr,
				"Store this somewhere safe. It is the only thing that can decrypt the vault,\n"+
					"and it is not saved anywhere by this command.\n\n"+
					"  PowerShell:  %s\n"+
					"  sh:          %s\n\n"+
					"Or skip the copy-paste entirely:\n\n"+
					"  PowerShell:  %s\n"+
					"  sh:          %s\n\n",
				exportStatement(shellPowerShell, vault.MasterKeyEnv, "<key>"),
				exportStatement(shellPosix, vault.MasterKeyEnv, "<key>"),
				evalHint(shellPowerShell, "docpreview vault keygen -shell"),
				evalHint(shellPosix, "docpreview vault keygen -shell"))
		}
		fmt.Println(key)
		return nil
	}

	kind, err := parseShell(*shellFlag)
	if err != nil {
		return err
	}

	// stdout carries the statement and nothing else, because everything on it
	// is about to be executed by a shell.
	fmt.Println(exportStatement(kind, vault.MasterKeyEnv, key))

	if !*quiet {
		fmt.Fprintf(os.Stderr,
			"Set %s for this session only (%s syntax).\n"+
				"Nothing was written to disk. Save the key in a password manager now —\n"+
				"it is the only thing that can decrypt the vault, and it is gone when this shell exits.\n\n"+
				"To see the key itself:  docpreview vault keygen\n",
			vault.MasterKeyEnv, kind)
	}
	return nil
}

// writeKeyFile writes a freshly minted master key to path for vault.key_source.
//
// The key never appears on stdout, in a clipboard, or in a shell history — which
// is the point, and the reason this is a flag rather than `keygen > file`: a
// redirect leaves the key in the scrollback of any shell that echoes, and gives
// the file whatever umask happens to be in force.
//
// Refusing to overwrite is not politeness. The existing file may be the only
// copy of the key to a vault full of credentials, and clobbering it destroys
// every secret in that vault with no error and nothing to restore from.
func writeKeyFile(path, key string, quiet bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(abs), err)
	}

	// O_EXCL is the refusal, and it is atomic — a Stat-then-create would lose a
	// race with a second keygen against the same path.
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return fmt.Errorf("%s already exists: it may be the only key to an existing vault, "+
			"and overwriting it would make every secret in that vault unreadable. "+
			"Move it aside first if you mean to replace it", abs)
	}
	if err != nil {
		return fmt.Errorf("creating %s: %w", abs, err)
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", abs, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", abs, err)
	}

	if !quiet {
		fmt.Fprintf(os.Stderr,
			"Wrote a new master key to %s (mode 0600).\n\n"+
				"Point the daemon at it:\n\n"+
				"  vault:\n"+
				"    key_source: \"file:%s\"\n\n"+
				"This file is now the only thing that can decrypt the vault. Back it up somewhere\n"+
				"that is not this machine, keep it outside data_dir, and prefer a secret manager\n"+
				"over a file where you have one:\n\n"+
				"  vault:\n"+
				"    key_source: \"exec:op read op://ops/docpreview/master-key\"\n",
			abs, abs)
	}
	return nil
}

// isFlagSet reports whether a flag was given on the command line, as opposed to
// left at its zero value. This is how -shell with no value can mean "auto"
// while an absent -shell means "print a bare key".
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// openVault loads only the config and the vault, skipping the rest of the
// wiring so that credentials can be stored before anything else works.
func openVault(configPath string) (*vault.Vault, error) {
	cfg, err := config.LoadServer(configPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", cfg.DataDir, err)
	}
	return vault.OpenFrom(cfg.VaultPath(), cfg.KeySource())
}

// buildSecrets resolves config's build.secrets — a map of environment variable
// name to vault key — into the values themselves.
//
// Failing loudly on a missing key is deliberate. The alternative is a build
// that runs with the variable unset, produces a site missing whatever the
// credential was for, and reports success; and worse, a redactor built from
// one fewer value than the operator believes, which is the one failure mode
// this package exists to prevent.
//
// The vault is opened only when there is something to look up, so a server
// with no build secrets still starts without a passphrase.
// buildSecrets resolves the environment every build gets.
//
// Two sources, and the second is the one somebody can reach from a browser:
//
//   - `build.secrets` in the config, mapping an environment variable name to a vault key
//     whose name is something else. Still supported, and still fails startup when the key
//     is missing, because a configured credential that is absent is a misconfiguration.
//   - **Every vault entry whose key is already shell-shaped**, injected under its own
//     name. This is what makes the credential page mean something: a token stored there
//     reaches every build without also editing a YAML file on the host.
//
// The second exists because the first, alone, made the dashboard lie. Storing
// BB_REPO_TOKEN_ONPREM from the page did nothing at all — the page said "set", and the
// next build behaved exactly as if it were absent, with no error anywhere to explain it.
//
// A project's own variables override these by name; see Daemon.SetProjectSecrets.
func buildSecrets(w *wiring) (map[string]string, error) {
	v, err := w.Vault()
	if err != nil {
		if len(w.cfg.Build.Secrets) == 0 {
			// No vault and nothing configured to need one. A daemon serving a directory
			// on loopback needs no credentials at all, and demanding one here would make
			// the simplest setup the one that does not start.
			return nil, nil
		}
		return nil, fmt.Errorf("build.secrets is set, so the vault must open: %w", err)
	}

	out := map[string]string{}

	// Shell-shaped keys first, so an explicit build.secrets mapping wins over a bare key
	// of the same name — the config file is the deliberate statement.
	for _, key := range v.Keys() {
		if !vault.IsBuildEnvKey(key) {
			continue
		}
		s, err := v.Get(key)
		if err != nil {
			continue
		}
		out[key] = s.RevealString()
	}

	for env, key := range w.cfg.Build.Secrets {
		s, err := v.Get(key)
		if err != nil {
			return nil, fmt.Errorf("build.secrets[%s]: %w\n\nstore it with:\n    docpreview vault set %s",
				env, err, key)
		}
		out[env] = s.RevealString()
	}
	return out, nil
}

func readStdin() ([]byte, error) {
	// 1 MiB is far more than any credential and stops a mistyped redirect from
	// reading a whole disk image into memory.
	const limit = 1 << 20
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for len(buf) < limit {
		n, err := os.Stdin.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if len(buf) >= limit {
		return nil, errors.New("input too large to be a credential")
	}
	return buf, nil
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
