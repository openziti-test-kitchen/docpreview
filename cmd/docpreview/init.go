package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/vault"
)

// cmdInit asks the setup questions and writes a config file.
//
// Two questions by default, not thirteen.
//
// The first version asked about every field, each with a working default, and
// the result was a page of prompts answered by holding Enter — which teaches
// people that the questions do not matter and that setup wizards are something
// to skip. A question earns its place only if a reasonable person would answer
// differently from the default. Exactly two clear that bar: which exposer, and
// the GitHub App ID, which has no sensible default because it is a number
// GitHub assigns.
//
// Everything else is a default that -advanced will let you change and the
// summary at the end will show you. Nothing is hidden; it is just not asked.
//
// The output is a *commented* file rather than a marshalled struct. A generated
// config that someone has to read the documentation to modify is barely better
// than no config, and the comments are the part that survives being copied to a
// second machine six months later.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "where to write the config file")
	force := fs.Bool("force", false, "overwrite an existing config file")
	advanced := fs.Bool("advanced", false, "ask about every setting, not just the two that matter")
	acceptAll := fs.Bool("yes", false, "ask nothing; take every default")
	exposerFlag := fs.String("exposer", "", "zrok2, frontdoor, or local (skips that question)")
	appIDFlag := fs.Int64("app-id", -1, "GitHub App ID (skips that question)")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	path, err := filepath.Abs(*configPath)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", *configPath, err)
	}

	p := newPrompter()
	cfg := config.DefaultServer()

	if _, err := os.Stat(path); err == nil && !*force {
		if *acceptAll {
			return fmt.Errorf("%s already exists; pass -force to overwrite", path)
		}
		fmt.Fprintf(os.Stderr, "%s already exists.\n", path)
		if !p.yesNo("Overwrite it?", false) {
			return fmt.Errorf("not overwriting %s", path)
		}
	}

	// ask reports whether a question should be put to the operator at all.
	ask := func(essential bool) bool {
		if *acceptAll {
			return false
		}
		return essential || *advanced
	}

	if !*acceptAll {
		fmt.Fprintf(os.Stderr, "\nWriting to %s\n", path)
		if !*advanced {
			fmt.Fprintf(os.Stderr, "One question. Everything else takes a default "+
				"(run with -advanced to see them all).\n")
		} else {
			fmt.Fprintf(os.Stderr, "Press Enter to accept the value in brackets.\n")
		}
	}

	// --- Exposer: a real decision ------------------------------------------
	if *exposerFlag != "" {
		if err := checkExposerKind(*exposerFlag); err != nil {
			return err
		}
		cfg.Exposer.Kind = *exposerFlag
	} else if ask(true) {
		fmt.Fprintln(os.Stderr, "\nHow should previews become reachable?")
		fmt.Fprintln(os.Stderr, "  zrok2      OpenZiti overlay. Binds no local port. Start here.")
		fmt.Fprintln(os.Stderr, "  frontdoor  NetFoundry Frontdoor. Adds a WAF and IdP-enforced access.")
		fmt.Fprintln(os.Stderr, "  local      Loopback only. Try the pipeline without an account.")
		cfg.Exposer.Kind = p.choice("Exposer", []string{"zrok2", "frontdoor", "local"}, cfg.Exposer.Kind)
	}

	// --- GitHub App ID -----------------------------------------------------
	//
	// Not asked by default. Source control is the *last* thing you wire up, not
	// the first: at init you have not created the App yet, and if you are using
	// Bitbucket you never will. Zero means "not configured", which the daemon
	// now handles by telling you so rather than refusing to start, and the
	// checklist below reminds you to come back to it.
	if *appIDFlag >= 0 {
		cfg.GitHub.AppID = *appIDFlag
	} else if ask(false) {
		p.section("GitHub")
		p.note("The numeric App ID from the App's settings page.")
		p.note("Leave it 0 if you have not created the App yet, or are using Bitbucket.")
		cfg.GitHub.AppID = p.number("App ID", cfg.GitHub.AppID)
	}

	// --- Everything below is -advanced only --------------------------------
	if ask(false) {
		p.section("Webhook ingress")
		p.note("Loopback is right for almost everyone: GitHub reaches it through a zrok share,")
		p.note("so nothing needs to be bound to the network.")
		cfg.Listen = p.validated("Listen address", cfg.Listen, checkListenAddr)

		p.note("")
		p.note("Holds the encrypted vault, the sqlite database, clones, and built previews.")
		cfg.DataDir = p.text("Data directory", cfg.DataDir)

		p.note("")
		p.note("Concurrent builds. Each is an npm install, so this is bounded by disk and RAM.")
		cfg.Workers = int(p.number("Workers", int64(cfg.Workers)))

		switch cfg.Exposer.Kind {
		case "zrok2":
			p.section("zrok v2")
			p.note("Blank uses whatever 'zrok2 enable' set as the default namespace.")
			cfg.Exposer.Zrok.Namespace = p.text("Namespace token", cfg.Exposer.Zrok.Namespace)

			p.note("")
			p.note("The public hostname label. {{.Name}} is the branch, as a DNS label.")
			p.note("Watching more than one repository? Use {{.Repo.Name}}-{{.Name}} — zrok names")
			p.note("are unique per namespace, so two repos with a main branch would collide.")
			cfg.Exposer.Zrok.NameTemplate = p.validated("Name template",
				cfg.Exposer.Zrok.NameTemplate, checkNameTemplate)

			p.note("")
			cfg.Exposer.Zrok.Open = p.yesNo("Previews readable by anyone with the link?",
				cfg.Exposer.Zrok.Open)
			if !cfg.Exposer.Zrok.Open {
				p.note("Comma-separated zrok accounts allowed to reach a closed share.")
				cfg.Exposer.Zrok.AccessGrants = splitList(p.text("Access grants", ""))
			}

			p.note("")
			p.note("Optionally put an identity provider in front of every preview.")
			if provider := p.choice("OAuth provider",
				[]string{"none", "google", "github"}, "none"); provider != "none" {
				cfg.Exposer.Zrok.OauthProvider = provider
				p.note("Comma-separated email globs, e.g. *@example.com. Blank allows any account.")
				cfg.Exposer.Zrok.OauthEmailDomains = splitList(p.text("Allowed email domains", ""))
			}

		case "frontdoor":
			p.section("NetFoundry Frontdoor")
			cfg.Exposer.Frontdoor.APIBase = p.text("API base", cfg.Exposer.Frontdoor.APIBase)
			cfg.Exposer.Frontdoor.Frontend = p.text("Frontend name", cfg.Exposer.Frontdoor.Frontend)

			p.note("")
			p.note("Frontdoor's agent dials OUT to each preview, so unlike zrok this binds a real")
			p.note("port. Must be an address the agent can reach; loopback if it is on this host.")
			cfg.Exposer.Frontdoor.AgentReachableHost = p.text("Agent-reachable host",
				cfg.Exposer.Frontdoor.AgentReachableHost)
			cfg.Exposer.Frontdoor.NameTemplate = p.validated("Name template",
				cfg.Exposer.Frontdoor.NameTemplate, checkNameTemplate)
		}

		if p.yesNo("Using GitHub Enterprise?", false) {
			p.note("The API root, e.g. https://ghe.example.com/api/v3")
			cfg.GitHub.APIBase = p.text("API base", cfg.GitHub.APIBase)
		}

		p.section("Builds")
		p.note("  docker  Run the build in a capped throwaway container. The default.")
		p.note("  local   Run it on this host. This runs the pull request's own build scripts")
		p.note("          here, as this user — npm install executes every dependency's install")
		p.note("          script — so it needs allow_local_driver as well, and it is only right")
		p.note("          when every contributor is someone you would give a shell to.")
		cfg.Build.Driver = p.choice("Build driver",
			[]string{config.DriverDocker, config.DriverLocal}, cfg.Build.Driver)
		if cfg.Build.Driver == config.DriverDocker {
			cfg.Build.Image = p.text("Container image", cfg.Build.Image)
		} else {
			// Asked rather than assumed. Choosing the driver and accepting what it does
			// are two decisions, and the second is the one worth typing out.
			cfg.Build.AllowLocalDriver = p.yesNo(
				"Confirm: run pull request build scripts on this host as this user?",
				cfg.Build.AllowLocalDriver)
		}
		cfg.Build.Timeout = p.duration("Build timeout", cfg.Build.Timeout)

		p.section("Preview lifetime")
		p.note("Idle lifetime, refreshed on every rebuild, so an active PR never expires.")
		cfg.Preview.TTL = p.duration("Preview TTL", cfg.Preview.TTL)
		cfg.Preview.TeardownOnClose = p.yesNo("Remove a preview when its pull request closes?",
			cfg.Preview.TeardownOnClose)
	}

	// --- Write -------------------------------------------------------------
	if err := writeConfig(path, renderConfig(cfg)); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nWrote %s\n", path)
	printSummary(cfg, *advanced)
	printNextSteps(cfg, path)
	return nil
}

// writeConfig validates the rendered config and then installs it.
//
// The ordering is the point. `init` promises to write a valid config or fail
// without writing, and an earlier version broke that promise: it wrote the
// file, read it back, and reported the error — leaving the operator with a
// clobbered config *and* a failure. Overwriting something that worked with
// something that does not is the one outcome this command must never produce.
//
// So the candidate goes to a temporary file beside the target, is loaded
// through the real loader from there, and only then renamed into place. The
// rename is atomic, so an interrupted run cannot leave a half-written config
// either.
func writeConfig(path, body string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yml.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := io.WriteString(tmp, body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing a temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing a temporary config: %w", err)
	}

	if _, err := config.LoadServer(tmpName); err != nil {
		return fmt.Errorf("the generated config did not validate, so %s was left alone: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing %s: %w", path, err)
	}
	return nil
}

// printSummary shows the settings that were not asked about.
//
// Skipping eleven questions is only an improvement if the answers stay visible.
// Six lines here replace a page of prompts and still leave nothing to discover
// later by surprise.
func printSummary(cfg config.Server, advanced bool) {
	fmt.Fprintln(os.Stderr, "\nSettings\n--------")
	fmt.Fprintf(os.Stderr, "  exposer      %s\n", cfg.Exposer.Kind)
	if cfg.Exposer.Kind == "zrok2" {
		fmt.Fprintf(os.Stderr, "  preview URL  %s.<your zrok namespace>\n",
			cfg.Exposer.Zrok.NameTemplate)
		fmt.Fprintf(os.Stderr, "  visibility   %s\n", visibility(cfg.Exposer.Zrok))
	}
	fmt.Fprintf(os.Stderr, "  github app   %s\n", appIDText(cfg.GitHub.AppID))
	if len(cfg.Listeners) <= 1 {
		fmt.Fprintf(os.Stderr, "  listen       %s\n", cfg.Listen)
	} else {
		for i, l := range cfg.Listeners {
			label := "  listen      "
			if i > 0 {
				label = "              "
			}
			fmt.Fprintf(os.Stderr, "%s %s\n", label, l.Describe())
		}
	}
	fmt.Fprintf(os.Stderr, "  data dir     %s\n", cfg.DataDir)
	fmt.Fprintf(os.Stderr, "  builds       %s driver, %s timeout, %d at a time\n",
		cfg.Build.Driver, cfg.Build.Timeout, cfg.Workers)
	fmt.Fprintf(os.Stderr, "  preview ttl  %s\n", cfg.Preview.TTL)

	if !advanced {
		fmt.Fprintln(os.Stderr, "\nEdit the file to change any of these, or re-run with -advanced.")
	}
}

func visibility(z config.ZrokConfig) string {
	switch {
	case z.OauthProvider != "":
		return "behind " + z.OauthProvider + " OAuth"
	case z.Open:
		return "anyone with the link"
	case len(z.AccessGrants) > 0:
		return fmt.Sprintf("%d granted account(s)", len(z.AccessGrants))
	default:
		return "closed, no grants yet"
	}
}

func appIDText(id int64) string {
	if id == 0 {
		return "not set yet"
	}
	return fmt.Sprintf("%d", id)
}

func checkListenAddr(s string) error {
	if _, _, err := net.SplitHostPort(s); err != nil {
		return fmt.Errorf("not a host:port address (try 127.0.0.1:8471)")
	}
	return nil
}

func checkExposerKind(kind string) error {
	switch kind {
	case "zrok2", "frontdoor", "local":
		return nil
	default:
		return fmt.Errorf("unknown exposer %q (use zrok2, frontdoor, or local)", kind)
	}
}

// checkNameTemplate renders a name template against a sample pull request.
//
// A template that does not parse, or that names a field that does not exist,
// otherwise fails on the first real build — long after the person who typed it
// has stopped watching. Rendering it here against a stand-in catches both.
func checkNameTemplate(tmpl string) error {
	sample := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42,
		Branch: "feature/example",
	}
	name, err := expose.RenderName(tmpl, sample)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  -> a branch named feature/example would be published as %q\n", name)
	return nil
}

// splitList turns "a, b ,c" into []string{"a","b","c"}, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// printNextSteps tells the operator what is still missing, in order.
//
// Computed from the config rather than printed wholesale, so it does not tell
// someone who chose the local exposer to go enable zrok.
func printNextSteps(cfg config.Server, path string) {
	// Two lists, because they are two different commitments. The first gets a
	// preview on screen in about a minute and needs no accounts beyond the
	// exposer's. The second is the ten-minute web form, and nobody should be
	// asked to do it before they have seen the thing work.
	var now, later []string

	if cfg.Exposer.Kind == "zrok2" {
		now = append(now, "Enable a zrok environment if you have not:  zrok2 enable <account-token>")
	}
	if cfg.Exposer.Kind == "frontdoor" {
		now = append(now,
			"Set a vault master key and store the Frontdoor token:\n"+
				"     docpreview vault keygen -out <path outside data_dir>\n"+
				"     set vault.key_source to \"file:<that path>\", then:\n"+
				"     docpreview vault set "+vault.KeyFrontdoorToken)
	}
	now = append(now,
		"Check it:                docpreview doctor",
		"Publish something:       docpreview preview -build ./www")

	later = append(later,
		"Create a GitHub App and set app_id in "+path+":\n"+
			"     https://github.com/settings/apps/new\n"+
			"     Permissions: Contents read, Pull requests write, Checks write.\n"+
			"     Events: Pull request.",
		"Set a vault master key, or skip this and unlock from the dashboard:\n"+
			"     docpreview vault keygen -out <path outside data_dir>\n"+
			"     then set vault.key_source to \"file:<that path>\" in "+path+".\n"+
			"     Back the file up somewhere else — it is the only thing that decrypts the vault.",
		"Store the credentials:\n"+
			"     docpreview vault set "+vault.KeyGitHubPrivateKey+" -file <downloaded>.pem\n"+
			"     docpreview vault set "+vault.KeyGitHubWebhookSec,
		"Run the daemon:          docpreview serve")

	fmt.Fprintln(os.Stderr, "\nTry it now\n----------")
	for i, step := range now {
		fmt.Fprintf(os.Stderr, "%2d. %s\n", i+1, step)
	}

	if cfg.GitHub.AppID != 0 {
		// Already wired up; the remaining work is just the credentials.
		fmt.Fprintln(os.Stderr, "\nThen, to receive webhooks\n-------------------------")
		fmt.Fprintf(os.Stderr, " 1. %s\n 2. %s\n", later[1], later[2])
		fmt.Fprintf(os.Stderr, " 3. %s\n", later[3])
		fmt.Fprintln(os.Stderr)
		return
	}

	fmt.Fprintln(os.Stderr, "\nWhen you want previews on pull requests\n"+
		"---------------------------------------")
	for i, step := range later {
		fmt.Fprintf(os.Stderr, "%2d. %s\n", i+1, step)
	}
	fmt.Fprintln(os.Stderr)
}

// renderConfig writes the config as commented YAML.
func renderConfig(c config.Server) string {
	var b strings.Builder

	b.WriteString("# docpreview server configuration.\n")
	b.WriteString("# Generated by docpreview. Edit freely; every field is documented at\n")
	b.WriteString("# www/docs/reference/configuration.md.\n\n")

	renderListeners(&b, c)

	b.WriteString("# Vault, sqlite database, workspaces, and build artifacts. Created 0700.\n")
	fmt.Fprintf(&b, "data_dir: %s\n\n", yamlString(c.DataDir))

	b.WriteString("# Concurrent builds. Each one is an npm install.\n")
	fmt.Fprintf(&b, "workers: %d\n\n", c.Workers)

	b.WriteString("exposer:\n")
	b.WriteString("  # zrok2 | frontdoor | local\n")
	fmt.Fprintf(&b, "  kind: %s\n\n", yamlString(c.Exposer.Kind))

	b.WriteString("  zrok2:\n")
	b.WriteString("    # Blank uses the enabled environment's default namespace.\n")
	fmt.Fprintf(&b, "    namespace: %s\n\n", yamlString(c.Exposer.Zrok.Namespace))
	b.WriteString("    # {{.Name}} is the branch, sanitized into a DNS label. Use\n")
	b.WriteString("    # \"{{.Repo.Name}}-{{.Name}}\" when watching more than one repository:\n")
	b.WriteString("    # zrok names are unique per namespace.\n")
	fmt.Fprintf(&b, "    name_template: %s\n\n", yamlString(c.Exposer.Zrok.NameTemplate))
	b.WriteString("    # true: anyone with the link. false: only the accounts in access_grants.\n")
	fmt.Fprintf(&b, "    open: %t\n", c.Exposer.Zrok.Open)
	fmt.Fprintf(&b, "    access_grants: %s\n\n", yamlList(c.Exposer.Zrok.AccessGrants))
	b.WriteString("    # Gate previews behind an identity provider: \"google\" or \"github\".\n")
	fmt.Fprintf(&b, "    oauth_provider: %s\n", yamlString(c.Exposer.Zrok.OauthProvider))
	fmt.Fprintf(&b, "    oauth_email_domains: %s\n\n", yamlList(c.Exposer.Zrok.OauthEmailDomains))

	b.WriteString("  ziti:\n")
	b.WriteString("    # docpreview's own enrolled identity, named by a Bind policy on the service.\n")
	fmt.Fprintf(&b, "    identity_file: %s\n", yamlString(c.Exposer.Ziti.IdentityFile))
	b.WriteString("    # One wildcard service carries every preview; requests are separated by Host.\n")
	b.WriteString("    # Exactly one docpreview may bind it.\n")
	fmt.Fprintf(&b, "    service: %s\n", yamlString(c.Exposer.Ziti.Service))
	b.WriteString("    # Must match the addresses in the service's intercept.v1 config, or the\n")
	b.WriteString("    # tunneler resolves names docpreview does not answer to.\n")
	fmt.Fprintf(&b, "    domain: %s\n", yamlString(c.Exposer.Ziti.Domain))
	fmt.Fprintf(&b, "    name_template: %s\n\n", yamlString(c.Exposer.Ziti.NameTemplate))

	b.WriteString("  frontdoor:\n")
	fmt.Fprintf(&b, "    api_base: %s\n", yamlString(c.Exposer.Frontdoor.APIBase))
	fmt.Fprintf(&b, "    frontend: %s\n", yamlString(c.Exposer.Frontdoor.Frontend))
	b.WriteString("    # Frontdoor's agent dials OUT to the preview, so unlike zrok this binds a\n")
	b.WriteString("    # real TCP port. Must be an address the agent can reach.\n")
	fmt.Fprintf(&b, "    agent_reachable_host: %s\n", yamlString(c.Exposer.Frontdoor.AgentReachableHost))
	fmt.Fprintf(&b, "    name_template: %s\n\n", yamlString(c.Exposer.Frontdoor.NameTemplate))

	b.WriteString("github:\n")
	b.WriteString("  # From the App settings page. The private key and webhook secret are NOT here;\n")
	b.WriteString("  # they live in the encrypted vault:\n")
	fmt.Fprintf(&b, "  #   docpreview vault set %s -file key.pem\n", vault.KeyGitHubPrivateKey)
	fmt.Fprintf(&b, "  #   docpreview vault set %s\n", vault.KeyGitHubWebhookSec)
	fmt.Fprintf(&b, "  app_id: %d\n", c.GitHub.AppID)
	b.WriteString("  # https://your-ghe-host/api/v3 for GitHub Enterprise.\n")
	fmt.Fprintf(&b, "  api_base: %s\n\n", yamlString(c.GitHub.APIBase))

	// Rendered even when disabled, because this function is also how
	// `configure ziti` rewrites an existing config: a section it omitted would
	// be a setting silently turned off by a command that had nothing to do
	// with it.
	b.WriteString("local:\n")
	b.WriteString("  # Bare git repositories on this machine that trigger builds when pushed to.\n")
	b.WriteString("  # A webhook endpoint that clones and builds whatever it is told to, so it is\n")
	b.WriteString("  # off unless you asked for it.\n")
	fmt.Fprintf(&b, "  enabled: %t\n", c.Local.Enabled)
	fmt.Fprintf(&b, "  repos_dir: %s\n", yamlString(c.Local.ReposDir))
	fmt.Fprintf(&b, "  default_base: %s\n", yamlString(c.Local.DefaultBase))
	b.WriteString("  # Blank means unauthenticated, which is the point for a loopback endpoint\n")
	b.WriteString("  # you curl by hand.\n")
	fmt.Fprintf(&b, "  webhook_secret: %s\n\n", yamlString(c.Local.WebhookSecret))

	b.WriteString("build:\n")
	b.WriteString("  # docker: run the build in a capped throwaway container. local: run it on\n")
	b.WriteString("  # this host, which means the pull request's own build scripts run here as\n")
	b.WriteString("  # this user — allow_local_driver below has to say so before that works.\n")
	fmt.Fprintf(&b, "  driver: %s\n", yamlString(c.Build.Driver))
	if c.Build.AllowLocalDriver {
		b.WriteString("  # Enabled deliberately. npm install runs every dependency's postinstall\n")
		b.WriteString("  # script, and npm run build runs whatever the branch says, as this user.\n")
		b.WriteString("  allow_local_driver: true\n")
	} else {
		b.WriteString("  # Uncomment only if every contributor to every watched repository is\n")
		b.WriteString("  # someone you would give a shell on this machine.\n")
		b.WriteString("  # allow_local_driver: true\n")
	}
	fmt.Fprintf(&b, "  image: %s\n", yamlString(c.Build.Image))
	fmt.Fprintf(&b, "  timeout: %s\n", c.Build.Timeout)
	b.WriteString("  # Build output can contain anything a build printed, so it is not kept forever.\n")
	fmt.Fprintf(&b, "  keep_logs: %s\n", c.Build.KeepLogs)
	b.WriteString("  # ENV_VAR: vault.key — injected into every build and redacted from its output.\n")
	b.WriteString("  # Never in .docpreview.yml: a pull request author must not be able to name a\n")
	b.WriteString("  # secret and have it handed to a script they wrote.\n")
	renderSecrets(&b, c.Build.Secrets)

	b.WriteString("preview:\n")
	b.WriteString("  # Idle lifetime, refreshed on every rebuild.\n")
	fmt.Fprintf(&b, "  ttl: %s\n", c.Preview.TTL)
	fmt.Fprintf(&b, "  teardown_on_close: %t\n\n", c.Preview.TeardownOnClose)

	b.WriteString("vault:\n")
	b.WriteString("  # Where the master key that decrypts the vault comes from. Unset — the\n")
	b.WriteString("  # default — means nowhere: serve starts with a locked vault and you unlock\n")
	b.WriteString("  # it from the dashboard. That is the only setting with no key at rest, and\n")
	b.WriteString("  # the price is that a restart needs a person.\n")
	b.WriteString("  #\n")
	b.WriteString("  # A command, so the key exists in this process and nowhere else on the box:\n")
	b.WriteString("  #   key_source: \"exec:op read op://ops/docpreview/master-key\"\n")
	b.WriteString("  # Or a file, which must live OUTSIDE data_dir — a key beside the vault it\n")
	b.WriteString("  # decrypts is not encryption at rest, and startup refuses it:\n")
	b.WriteString("  #   docpreview vault keygen -out /etc/docpreview/master.key\n")
	b.WriteString("  #   key_source: \"file:/etc/docpreview/master.key\"\n")
	renderKeySource(&b, c.Vault.KeySource)

	return b.String()
}

// renderListeners writes whichever of the two spellings fits.
//
// Exactly one of them, never both — the loader refuses a config that sets
// `listen` and `listeners` together, and writeConfig runs the candidate
// through that loader, so emitting both would turn every rewrite into a
// failure. A lone TCP listener keeps the short spelling, which is what almost
// every config has and what the documentation shows.
func renderListeners(b *strings.Builder, c config.Server) {
	single := c.Listen
	if len(c.Listeners) == 1 && c.Listeners[0].Ziti == nil {
		single = c.Listeners[0].TCP
	}
	if len(c.Listeners) <= 1 {
		b.WriteString("# Where the webhook ingress binds. Loopback plus a zrok share means nothing is\n")
		b.WriteString("# exposed to the network directly.\n")
		fmt.Fprintf(b, "listen: %s\n\n", yamlString(single))
		return
	}

	b.WriteString("# Every address the dashboard and webhook endpoint answer on. A ziti listener\n")
	b.WriteString("# binds an overlay service instead of a port, so the admin surface — which\n")
	b.WriteString("# lists every open documentation pull request — has no address on the network.\n")
	b.WriteString("listeners:\n")
	for _, l := range c.Listeners {
		if l.Ziti != nil {
			b.WriteString("  - ziti:\n")
			fmt.Fprintf(b, "      identity_file: %s\n", yamlString(l.Ziti.IdentityFile))
			fmt.Fprintf(b, "      service: %s\n", yamlString(l.Ziti.Service))
			continue
		}
		fmt.Fprintf(b, "  - tcp: %s\n", yamlString(l.TCP))
	}
	b.WriteString("\n")
}

// yamlString renders a value as a YAML double-quoted scalar.
//
// Quoting matters for values YAML would otherwise reinterpret: an empty string,
// a leading "*", anything that looks like a number or a boolean. Escaping
// matters for everything else. An earlier version escaped only backslash and
// double quote, which is fine until an answer contains a newline or a tab —
// and every free-form answer in `init -advanced` is operator-typed, so a
// pasted path with a stray control character produced a config file that YAML
// could not parse.
//
// encoding/json does the escaping exactly. YAML 1.2's double-quoted scalars
// accept JSON's escape set, so a JSON string literal is always a valid YAML
// one. HTML escaping is turned off because < in a config file is
// unreadable and buys nothing here.
func yamlString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// json cannot fail on a string, but a silent empty value in a config
		// file would be worse than an obviously wrong one.
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// renderSecrets writes build.secrets, sorted so that rewriting a config twice
// produces the same file rather than a diff of shuffled map keys.
// renderKeySource writes key_source, commented out when there is none.
//
// Commented rather than empty-stringed: `key_source: ""` reads as a setting
// somebody cleared, and a rewrite of this file has to round-trip through the
// loader, which would then have to accept a spelling that means the same as
// absent. Absent is the default and stays spelled that way.
func renderKeySource(b *strings.Builder, source string) {
	if source == "" {
		b.WriteString("  # key_source: \"\"\n")
		return
	}
	fmt.Fprintf(b, "  key_source: %s\n", yamlString(source))
}

func renderSecrets(b *strings.Builder, secrets map[string]string) {
	if len(secrets) == 0 {
		b.WriteString("  secrets: {}\n\n")
		return
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	b.WriteString("  secrets:\n")
	for _, name := range names {
		fmt.Fprintf(b, "    %s: %s\n", name, yamlString(secrets[name]))
	}
	b.WriteString("\n")
}

// yamlList renders a flow-style sequence.
func yamlList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = yamlString(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
