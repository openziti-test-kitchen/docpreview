// Package config loads the two kinds of configuration docpreview reads: the
// server's own settings, and the per-repository settings that ship inside the
// repository being previewed.
//
// The split matters. Server config is trusted — an operator wrote it. Repo
// config arrives from a pull request, which means anyone who can open a PR can
// influence it, so every field is validated and nothing in it is allowed to
// choose what gets executed on the host. See RepoConfig for the details.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/netfoundry/docpreview/internal/vault"
)

// Server is the operator-supplied configuration for the docpreview daemon.
type Server struct {
	// Listen is the address the webhook ingress binds to, e.g. "127.0.0.1:8080".
	//
	// The single-address spelling, kept because it is what every existing
	// config, doc and test says. Listeners is the general form; see
	// resolveListeners for how the two relate.
	Listen string `yaml:"listen"`

	// Listeners is the general form: any number of ingress listeners, TCP or
	// OpenZiti, served by one http.Server.
	//
	// After LoadServer this is always populated — a config that only sets
	// Listen gets a one-element list holding it — so everything downstream can
	// read this field alone.
	Listeners []Listener `yaml:"listeners"`

	// listenGiven records whether the file actually contained a `listen` key,
	// as opposed to inheriting the default. Setting both spellings is refused,
	// and that refusal is only possible if we know which keys were written.
	listenGiven bool

	// DataDir holds the sqlite database, the vault, cloned workspaces, and
	// built preview artifacts.
	DataDir string `yaml:"data_dir"`

	// Workers is the number of builds that may run concurrently.
	Workers int `yaml:"workers"`

	// DashboardURL is the address the dashboard is reachable at, used to link a
	// failed build's comment to its build log.
	//
	// Configured rather than derived, because the daemon cannot know it. The
	// listener is loopback, so the address it binds is the one address a link in
	// a pull request comment must not use — and whatever makes the dashboard
	// reachable, a tunnel or a reverse proxy or a VPN, is outside this process.
	//
	// Empty is the honest default: the comment then names the dashboard without
	// pretending to link to it.
	DashboardURL string `yaml:"dashboard_url"`

	Exposer ExposerConfig  `yaml:"exposer"`
	GitHub  GitHubConfig   `yaml:"github"`
	Local   LocalSCMConfig `yaml:"local"`
	Build   BuildDefaults  `yaml:"build"`
	Preview PreviewConfig  `yaml:"preview"`
	Vault   VaultConfig    `yaml:"vault"`
}

// VaultConfig says where the vault's master key comes from.
type VaultConfig struct {
	// KeySource names where to read the master key at startup. Empty means
	// there is nowhere to read it from, and the daemon boots locked until
	// somebody unlocks it from the dashboard.
	//
	// Empty is the default on purpose. Every alternative puts the key that
	// decrypts the vault somewhere a process can read without a human, which
	// is the whole point of the setting and also its entire cost — see
	// vault.ParseKeySource for the forms and what each one is worth.
	KeySource string `yaml:"key_source"`
}

// UnmarshalYAML decodes a Server and remembers which of the two listener
// spellings the file used.
//
// The `plain` alias is what stops this from recursing: a defined type does not
// inherit its source type's methods, so decoding into it runs the ordinary
// struct decoder. Fields absent from the document keep the values they already
// had, which is what lets LoadServer overlay a file onto DefaultServer.
func (s *Server) UnmarshalYAML(node *yaml.Node) error {
	type plain Server
	if err := node.Decode((*plain)(s)); err != nil {
		return err
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "listen" {
				s.listenGiven = true
			}
		}
	}
	return nil
}

// Listener is one place the HTTP ingress accepts connections.
//
// Exactly one of the two is set. They are separate fields rather than a kind
// string plus a union because the ziti form carries settings the TCP form has
// no use for, and a flat struct would leave an operator guessing which keys
// apply to which kind.
type Listener struct {
	// TCP is a host:port address, e.g. "127.0.0.1:8471".
	TCP string

	// Ziti binds an OpenZiti service instead of a port, so the dashboard and
	// the webhook endpoint have no address on the underlay at all.
	Ziti *ZitiListener
}

// ZitiListener is an ingress listener bound to an OpenZiti service.
//
// It names its own identity rather than reusing exposer.ziti's. The two are
// different grants — hosting previews and hosting the admin surface — and the
// admin surface is the more sensitive of the two, since it enumerates every
// open documentation pull request. Wiring them to one identity by default
// would make separating them later a migration rather than an edit.
type ZitiListener struct {
	// IdentityFile is an enrolled identity named by a Bind policy on Service.
	IdentityFile string `yaml:"identity_file"`

	// Service is the service to bind.
	//
	// Exactly one process may bind a given service: binding creates a
	// terminator, and a second binding creates a second one that the router
	// load-balances against under the default strategy. Two docpreviews
	// sharing a service would each answer about half the requests. Give a
	// second instance its own service.
	Service string `yaml:"service"`
}

// UnmarshalYAML accepts either spelling of a listener entry:
//
//	listeners:
//	  - tcp: "127.0.0.1:8471"
//	  - ziti:
//	      identity_file: "…/docpreview-host.json"
//	      service: "docpreview-admin"
//
// A bare string is also accepted as shorthand for tcp, because a list of
// addresses is the first thing anyone tries.
func (l *Listener) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		l.TCP = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("a listener must be an address or a single-key mapping "+
			"(tcp: or ziti:), got a %s", nodeKind(node))
	}

	var keys []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		keys = append(keys, key)
		switch key {
		case "tcp":
			l.TCP = value.Value
		case "ziti":
			l.Ziti = &ZitiListener{}
			if err := value.Decode(l.Ziti); err != nil {
				return fmt.Errorf("listener ziti: %w", err)
			}
		default:
			return fmt.Errorf("unknown listener kind %q (use tcp or ziti)", key)
		}
	}
	if len(keys) != 1 {
		return fmt.Errorf("a listener names exactly one kind, got %s "+
			"(write them as separate list entries)", strings.Join(keys, " and "))
	}
	return nil
}

func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "list"
	case yaml.MappingNode:
		return "mapping"
	default:
		return "value"
	}
}

// Describe renders a listener for a log line or `docpreview doctor`.
func (l Listener) Describe() string {
	if l.Ziti != nil {
		return fmt.Sprintf("ziti service %s (identity %s)", l.Ziti.Service, l.Ziti.IdentityFile)
	}
	return "tcp " + l.TCP
}

// resolveListeners collapses the two spellings into Listeners and checks each
// entry.
//
// Both at once is refused rather than resolved by precedence. Whichever one
// lost would be a bound address the operator believes is live — a webhook
// endpoint nobody is answering, or an admin surface exposed on a port they
// meant to stop using. There is no reading of that config that is safe to
// guess at.
func (s *Server) resolveListeners() error {
	if len(s.Listeners) == 0 {
		s.Listeners = []Listener{{TCP: s.Listen}}
	} else if s.listenGiven {
		return fmt.Errorf("set either listen or listeners, not both; " +
			"move listen into the list as `- tcp: \"…\"`")
	}

	for i, l := range s.Listeners {
		switch {
		case l.Ziti != nil:
			// Caught here rather than at bind time: an identity that is only
			// consulted on the first request turns a typo in a path into a
			// daemon that looks healthy and answers nothing.
			if l.Ziti.IdentityFile == "" {
				return fmt.Errorf("listeners[%d].ziti.identity_file must be set "+
					"(the enrolled identity that binds the service)", i)
			}
			if l.Ziti.Service == "" {
				return fmt.Errorf("listeners[%d].ziti.service must be set", i)
			}
		case l.TCP != "":
			// A malformed address is otherwise not caught until Serve, which
			// reports it as an opaque address error after every other
			// component has already been validated and connected to.
			if _, _, err := net.SplitHostPort(l.TCP); err != nil {
				return fmt.Errorf("listeners[%d].tcp %q is not a host:port address "+
					"(try 127.0.0.1:8471): %w", i, l.TCP, err)
			}
		default:
			return fmt.Errorf("listeners[%d] is empty; give it a tcp address or a ziti service", i)
		}
	}
	return nil
}

// FirstTCPAddr returns the first TCP address the ingress binds, or "" when the
// ingress is overlay-only. Callers that need to print a reachable URL — `sim`
// and `doctor` — have nothing to print in that case.
func (s Server) FirstTCPAddr() string {
	for _, l := range s.Listeners {
		if l.Ziti == nil && l.TCP != "" {
			return l.TCP
		}
	}
	return ""
}

// LocalSCMConfig configures the local source-control stand-in: bare git
// repositories on this machine instead of a hosted platform.
//
// It exists so the whole flow can be exercised without a GitHub App. Pushing to
// one of these repositories triggers a real build and a real comment; the only
// difference is that the comment is served by docpreview rather than by
// github.com.
type LocalSCMConfig struct {
	// Enabled turns the local platform on. Off by default, because a webhook
	// endpoint that clones and builds whatever it is told to is not something
	// to enable by accident.
	Enabled bool `yaml:"enabled"`

	// ReposDir holds the bare repositories that act as remotes.
	ReposDir string `yaml:"repos_dir"`

	// DefaultBase is the branch changed files are computed against when an
	// event does not name one.
	DefaultBase string `yaml:"default_base"`

	// WebhookSecret, if set, makes the local webhook require the same
	// HMAC-SHA256 signature GitHub sends. Blank means unauthenticated, which is
	// the point for a loopback endpoint you curl by hand.
	WebhookSecret string `yaml:"webhook_secret"`
}

// ExposerConfig selects and configures the mechanism that turns a preview into
// a reachable URL.
type ExposerConfig struct {
	// Kind is "zrok2", "frontdoor", "ziti", or "local".
	Kind string `yaml:"kind"`

	Zrok      ZrokConfig      `yaml:"zrok2"`
	Frontdoor FrontdoorConfig `yaml:"frontdoor"`
	Ziti      ZitiConfig      `yaml:"ziti"`
}

// ZitiConfig configures the OpenZiti exposer, which publishes previews on an
// overlay reachable only by an enrolled tunneler.
//
// Unlike every other exposer there is nothing public here at all, and nothing
// is created per preview: one wildcard service covers all of them and requests
// are separated by Host header. See www/docs/future/ziti-native-previews.md.
type ZitiConfig struct {
	// IdentityFile is docpreview's own enrolled identity, the one the Bind
	// policy names. Produced by `ziti edge enroll <jwt> --out <file>`.
	IdentityFile string `yaml:"identity_file"`

	// Service is the name of the wildcard service to bind.
	Service string `yaml:"service"`

	// Domain is the DNS suffix previews appear under. It must match the
	// addresses in the service's intercept.v1 config, or the tunneler will
	// resolve names docpreview does not answer to — and the mismatch is
	// invisible until someone opens a link.
	Domain string `yaml:"domain"`

	// NameTemplate behaves as described on DefaultNameTemplate.
	NameTemplate string `yaml:"name_template"`
}

// ZrokConfig configures the zrok v2 exposer.
type ZrokConfig struct {
	// Namespace is the zrok namespace token that owns the preview names. Empty
	// means "ask the enabled environment for its default namespace", which is
	// what a stock `zrok2 enable` gives you.
	Namespace string `yaml:"namespace"`

	// NameTemplate behaves as described on DefaultNameTemplate.
	NameTemplate string `yaml:"name_template"`

	// Open makes the share readable by anyone with the URL. When false, zrok's
	// closed permission mode applies and only the accounts in AccessGrants can
	// reach it.
	Open bool `yaml:"open"`

	// AccessGrants lists zrok accounts allowed to reach a closed share.
	AccessGrants []string `yaml:"access_grants"`

	// OauthProvider, when set ("google" or "github"), puts the zrok frontend's
	// OAuth gate in front of every preview.
	OauthProvider string `yaml:"oauth_provider"`

	// OauthEmailDomains restricts OAuth access to matching email globs.
	OauthEmailDomains []string `yaml:"oauth_email_domains"`
}

// FrontdoorConfig configures the NetFoundry Frontdoor exposer.
type FrontdoorConfig struct {
	// APIBase is the Frontdoor gateway root.
	APIBase string `yaml:"api_base"`

	// Frontend is the name of the Frontdoor frontend that will serve previews.
	Frontend string `yaml:"frontend"`

	// AgentReachableHost is the address at which the Frontdoor agent can reach
	// this docpreview instance. Unlike zrok, Frontdoor shares point at a URL
	// rather than dialing an overlay listener we own, so previews must bind a
	// real TCP port that the agent can connect to.
	AgentReachableHost string `yaml:"agent_reachable_host"`

	// NameTemplate behaves as described on DefaultNameTemplate.
	NameTemplate string `yaml:"name_template"`
}

// GitHubConfig holds the non-secret half of the GitHub App configuration. The
// private key and webhook secret live in the vault, never here.
type GitHubConfig struct {
	AppID   int64  `yaml:"app_id"`
	APIBase string `yaml:"api_base"`
}

// BuildDefaults are the fallbacks applied when a repository does not ship its
// own .docpreview.yml.
type BuildDefaults struct {
	// Driver is "docker" (run the build in a throwaway container) or "local" (run
	// it on this host). Docker is the default and local must be enabled first —
	// see AllowLocalDriver.
	Driver string `yaml:"driver"`

	// AllowLocalDriver permits the local driver. False by default, and that is a
	// security decision rather than a preference.
	//
	// The local driver runs the pull request's own build on this machine as the
	// daemon's user. `npm install` executes every dependency's postinstall script
	// and `npm run build` executes whatever the branch's package.json says, so a
	// contributor who can push a branch to a watched repository can run code on the
	// host that holds the GitHub App private key and every project's tokens. That
	// is not a hole to be patched — it is what "run the build here" means.
	//
	// So it cannot be arrived at by accident: not by a missing config file, not by
	// docker being absent at startup, and not by a project row naming it. An
	// operator who wants it says so once, in writing, in the server config.
	//
	// It is a legitimate choice for a daemon that only ever builds repositories
	// whose contributors already have access to the host, which is why it exists at
	// all rather than being deleted.
	AllowLocalDriver bool `yaml:"allow_local_driver"`

	// Image is the container image used by the docker driver.
	Image string `yaml:"image"`

	// Timeout caps a single build.
	Timeout time.Duration `yaml:"timeout"`

	// Secrets maps an environment variable name to a vault key whose value is
	// injected into every build.
	//
	//   build:
	//     secrets:
	//       ALGOLIA_WRITE_KEY: algolia.write_key
	//
	// This lives in the *server* config, never in .docpreview.yml, because a
	// pull request author must not be able to name a secret and have it handed
	// to a script they wrote.
	//
	// Every injected value is registered with the log redactor, so it is
	// replaced with five asterisks anywhere it appears in build output — see
	// internal/redact.
	Secrets map[string]string `yaml:"secrets"`

	// KeepLogs is how long a build log is kept on disk. Logs can contain
	// almost anything a build printed, so they are not kept forever.
	KeepLogs time.Duration `yaml:"keep_logs"`

	// CacheDir holds the package manager caches, shared by every build under the
	// docker driver.
	//
	// Not a per-build directory, which is the point: a workspace is created per
	// commit and deleted with its siblings, so without somewhere outside it to
	// keep downloads, every build fetches the whole dependency tree again — two
	// minutes of network per push for a Docusaurus site.
	//
	// Blank means <data_dir>/cache, set during loading. Set it explicitly to put
	// the cache on a different disk, which is worth doing: it is the largest and
	// most rewritten directory docpreview owns.
	CacheDir string `yaml:"cache_dir"`
}

// PreviewConfig governs preview lifetime.
type PreviewConfig struct {
	// TTL is how long an idle preview survives before the reaper takes it. A
	// preview is refreshed on every rebuild.
	TTL time.Duration `yaml:"ttl"`

	// TeardownOnClose removes the preview when its pull request is closed or
	// merged.
	TeardownOnClose bool `yaml:"teardown_on_close"`

	// KeepBuilds is how many builds of one pull request keep their artifacts, and
	// therefore how many remain openable.
	//
	// Artifacts are per build so an older commit is still there to serve. That
	// makes disk use grow with every push instead of staying at one built site per
	// pull request, so something has to bound it, and a count is the cheapest bound
	// that an operator can reason about.
	//
	// Not zero, and not unlimited: zero would delete the build that just
	// published, and unlimited fills a disk on the one repository somebody pushes
	// to fifty times in an afternoon. The full story — byte caps, a total ceiling,
	// eviction order, exempting the paid exposers — is in TODO.md.
	KeepBuilds int `yaml:"keep_builds"`
}

// DefaultNameTemplate builds the public label for a preview.
//
// A Go text/template over model.PullRequest, plus {{.Name}} holding the
// sanitized branch. The result is sanitized again afterwards, because a
// repository name can contain characters that are legal there and not in a
// hostname.
//
// Project and branch, not branch alone. The branch alone reads better and was
// the original default — "the URL is the branch name" — but it is not unique:
// four projects each with a `new-install-guide` branch all render to the same
// label. Under zrok and Frontdoor that means two previews fighting over one
// name in a shared namespace; under ziti it means one hostname with two
// claimants; under the local exposer it meant each publish silently killing
// another project's listener, which is how it was found. A default that is
// correct for one repository and quietly wrong for two is the wrong default.
//
// Deliberately not the commit SHA. Vercel gives every deployment its own
// immutable URL and a branch alias on top; docpreview has one comment per pull
// request that is edited in place, so the link a reviewer already opened has to
// keep working after the next push. Put {{.HeadSHA}} in the template if you
// want per-commit URLs — the field is there — and accept that every push
// strands the previous one.
//
// Useful variants:
//
//	{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}   several orgs in one namespace
//	pr-{{.Number}}-{{.Repo.Name}}              short and unique, less readable
//	{{.Repo.Name}}-{{.Name}}-{{.HeadSHA}}      immutable per commit
const DefaultNameTemplate = "{{.Repo.Name}}-{{.Name}}"

// The two build drivers, and the command a build runs when nobody trustworthy has
// said otherwise.
//
// DefaultBuildCommand is named rather than repeated because it is also the value the
// daemon falls back to when it discards a branch-supplied command under the local
// driver — two spellings of one string would make that substitution invisible.
const (
	DriverDocker = "docker"
	DriverLocal  = "local"

	DefaultBuildCommand = "npm run build"

	// DefaultImage needs node *and* git: a documentation build that assembles several
	// repositories clones them from inside the container. The -slim images have no git,
	// which made the shipped default fail on the first clone with "git: not found".
	DefaultImage = "node:24-bookworm"
)

// DefaultServer returns a Server with every field populated to a sane value.
// LoadServer starts from this and overlays the file, so an empty config file is
// a valid config file.
func DefaultServer() Server {
	home, _ := os.UserHomeDir()
	return Server{
		Listen:  "127.0.0.1:8471",
		DataDir: filepath.Join(home, ".docpreview"),
		Workers: 2,
		Exposer: ExposerConfig{
			Kind: "zrok2",
			Zrok: ZrokConfig{
				NameTemplate: DefaultNameTemplate,
				Open:         true,
			},
			Ziti: ZitiConfig{
				Service:      "docpreview-svc",
				Domain:       "docpreview.ziti",
				NameTemplate: DefaultNameTemplate,
			},
			Frontdoor: FrontdoorConfig{
				APIBase:  "https://gateway.production.netfoundry.io/frontdoor",
				Frontend: "public",
				// Loopback is correct when the Frontdoor agent runs on this
				// host, which is the arrangement to reach for: nothing is
				// exposed beyond the loopback interface.
				AgentReachableHost: "127.0.0.1",
				NameTemplate:       DefaultNameTemplate,
			},
		},
		GitHub: GitHubConfig{APIBase: "https://api.github.com"},
		Local: LocalSCMConfig{
			ReposDir:    filepath.Join(home, ".docpreview", "repos"),
			DefaultBase: "main",
		},
		Build: BuildDefaults{
			// Docker, because the alternative executes a pull request's own code on
			// this host. The default used to be local, which meant every install that
			// never touched this key was running branch-authored build scripts as the
			// daemon's user. See AllowLocalDriver.
			Driver: DriverDocker,
			// Not the -slim variant, which was the default and has no git in it. A
			// documentation site that assembles content from other repositories clones
			// them from inside the container, and that failed on the first clone with
			// "git: not found" — a default that cannot run the thing this program exists
			// to run. The full image is about twice the size, pulled once, and the
			// operator who wants the smaller one can say so.
			Image:    DefaultImage,
			Timeout:  15 * time.Minute,
			KeepLogs: 7 * 24 * time.Hour,
		},
		Preview: PreviewConfig{
			TTL:             72 * time.Hour,
			TeardownOnClose: true,
			KeepBuilds:      10,
		},
	}
}

// LoadServer reads the server configuration from path, overlaying it on the
// defaults. A missing file is not an error: the defaults are usable.
func LoadServer(path string) (Server, error) {
	cfg := DefaultServer()

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Still resolve, so that Listeners is populated on every path out of
		// this function and no caller has to special-case "no config file".
		return cfg, cfg.resolveListeners()
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("invalid %s: %w", path, err)
	}
	return cfg, nil
}

// validate takes a pointer because it also normalizes: resolveListeners fills
// Listeners in from Listen, so nothing downstream has to know that there are
// two spellings.
func (s *Server) validate() error {
	switch s.Exposer.Kind {
	case "zrok2", "frontdoor", "ziti", "local":
	default:
		return fmt.Errorf("exposer.kind %q must be one of zrok2, frontdoor, ziti, local", s.Exposer.Kind)
	}
	switch s.Build.Driver {
	case "local", "docker":
	default:
		return fmt.Errorf("build.driver %q must be local or docker", s.Build.Driver)
	}
	if s.Workers < 1 {
		return fmt.Errorf("workers must be at least 1, got %d", s.Workers)
	}
	if s.DataDir == "" {
		return fmt.Errorf("data_dir must be set")
	}
	// Written into the field, not merely derivable, because the build package is
	// handed BuildDefaults and never sees DataDir. CacheRoot is the definition; this
	// is where it becomes the value everything downstream reads.
	s.Build.CacheDir = s.CacheRoot()

	if err := s.validateKeySource(); err != nil {
		return err
	}

	if err := s.resolveListeners(); err != nil {
		return err
	}

	// Reject non-positive durations rather than quietly accepting them. Both of
	// these fail in a way that looks like a bug rather than a setting: a zero
	// build timeout expires the context before the first command runs, and a
	// zero preview TTL makes the hourly reaper delete every live preview. A
	// missing key still takes the default — it is an explicit zero that is
	// almost certainly a typo.
	if s.Build.Timeout <= 0 {
		return fmt.Errorf("build.timeout must be positive, got %s", s.Build.Timeout)
	}
	if s.Preview.TTL <= 0 {
		return fmt.Errorf("preview.ttl must be positive, got %s "+
			"(every preview would be reaped on the next sweep)", s.Preview.TTL)
	}
	// An explicit zero would delete the build that just published, taking the
	// preview's artifacts with it. A missing key still takes the default.
	if s.Preview.KeepBuilds < 1 {
		return fmt.Errorf("preview.keep_builds must be at least 1, got %d "+
			"(the build that just finished would be pruned)", s.Preview.KeepBuilds)
	}
	if s.Build.KeepLogs <= 0 {
		return fmt.Errorf("build.keep_logs must be positive, got %s "+
			"(the sweep would be a no-op and build logs would accumulate forever)",
			s.Build.KeepLogs)
	}

	return nil
}

// validateKeySource checks the spelling of vault.key_source, and refuses to put
// the master key inside data_dir.
//
// The placement rule is the whole point of the setting being scrutinised at all.
// data_dir holds vault.age; a key file beside it means one directory read yields
// both the ciphertext and the key that opens it, which is not encryption at rest
// under any reading. Everything that protects a key file is its location and its
// permissions, so a location that defeats it has to be a startup error rather
// than a paragraph in the docs.
func (s *Server) validateKeySource() error {
	src, err := vault.ParseKeySource(s.Vault.KeySource)
	if err != nil {
		return err
	}
	if src.Kind() != vault.SourceKindFile {
		return nil
	}

	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return fmt.Errorf("resolving data_dir: %w", err)
	}
	rel, err := filepath.Rel(dataDir, src.Path())
	if err != nil {
		// Different volumes on Windows. Not inside, then.
		return nil
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("vault.key_source %s is inside data_dir %s: the master key must not "+
			"live beside the vault it decrypts, since anyone who can read that directory "+
			"would have both halves", src.Path(), dataDir)
	}
	return nil
}

// KeySource is where the vault master key is read from, zero when nothing is
// configured and the vault can only be unlocked by a person.
//
// The parse error is dropped because validate already rejected an unparseable
// key_source at load, and every caller here is downstream of that. A zero
// KeySource is also the correct fallback for the one path that skips validation
// — a config file that does not exist at all.
func (s Server) KeySource() vault.KeySource {
	src, _ := vault.ParseKeySource(s.Vault.KeySource)
	return src
}

// Paths derived from DataDir.
func (s Server) VaultPath() string     { return filepath.Join(s.DataDir, "vault.age") }
func (s Server) DatabasePath() string  { return filepath.Join(s.DataDir, "docpreview.db") }
func (s Server) WorkspacesDir() string { return filepath.Join(s.DataDir, "workspaces") }
func (s Server) ArtifactsDir() string  { return filepath.Join(s.DataDir, "artifacts") }
func (s Server) LogsDir() string       { return filepath.Join(s.DataDir, "logs") }

// CacheRoot is where the package manager caches live.
//
// The one definition of the default. `validate` writes it into Build.CacheDir so the
// build package — which is handed BuildDefaults and never sees DataDir — can read it
// from there, and this derives the same answer for anything holding a Server that
// did not come through the loader. Two independent spellings of one default is how
// the daemon ends up clearing a directory the builder is not using.
func (s Server) CacheRoot() string {
	if s.Build.CacheDir != "" {
		return s.Build.CacheDir
	}
	if s.DataDir == "" {
		return ""
	}
	return filepath.Join(s.DataDir, "cache")
}

// PreviewCacheDir is one preview's package manager caches.
//
// Under CacheRoot rather than DataDir, because that one is meant to be moved to a
// disk with room. A preview ID is a hex digest, so it needs no sanitizing to be a
// safe path component — which is most of why the cache is keyed on it rather than on
// names that arrive from a webhook.
func (s Server) PreviewCacheDir(previewID string) string {
	root := s.CacheRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, previewID)
}

// RepoConfigName is the file docpreview looks for in the root of a previewed
// repository.
const RepoConfigName = ".docpreview.yml"

// RepoConfig is the per-repository configuration, read from the *checked-out
// working tree* of the pull request being built.
//
// Everything in this struct is attacker-influenced on a public repository: it
// is whatever the PR author wrote. Two rules follow, and both are enforced in
// validate:
//
//  1. No field may name a path outside the workspace.
//  2. No field may choose the build command when the docker driver is off.
//     Running arbitrary shell from a PR on the host is the whole game; under
//     the docker driver it is merely a container escape away, which is why
//     Command is honored there and ignored otherwise.
type RepoConfig struct {
	Build  RepoBuild  `yaml:"build"`
	Detect RepoDetect `yaml:"detect"`
}

// RepoBuild describes how to turn the repository into a directory of static
// files.
type RepoBuild struct {
	// Dir is the directory containing package.json, relative to the repo root.
	Dir string `yaml:"dir"`

	// Command is the build command. Honored only under the docker driver.
	Command string `yaml:"command"`

	// Output is the build output directory, relative to Dir.
	Output string `yaml:"output"`

	// BaseURL is the path the preview is served under, e.g. "/", "/docs/", or
	// "/zrok/". Docusaurus bakes this into every emitted asset URL at build
	// time, so it has to be decided here and not at serve time.
	//
	// docpreview passes it to the build and then mounts the output at the same
	// prefix, so the two can never disagree.
	BaseURL string `yaml:"base_url"`

	// Env is extra environment handed to the build. Values are literal; no
	// vault lookups, because a PR author must not be able to name a secret.
	Env map[string]string `yaml:"env"`
}

// RepoDetect describes how to decide whether a change is documentation-related.
type RepoDetect struct {
	// Paths are globs matched against the changed-file set. Any match means
	// "build this".
	Paths []string `yaml:"paths"`

	// Script, if set, is a path within the repository that overrides the glob
	// check entirely. Exit 0 means build, exit 78 means skip.
	Script string `yaml:"script"`
}

// DefaultRepoConfig is what a repository gets when it ships no .docpreview.yml.
// The globs cover a stock Docusaurus layout.
func DefaultRepoConfig() RepoConfig {
	return RepoConfig{
		Build: RepoBuild{
			Dir:     ".",
			Command: "npm run build",
			Output:  "build",
			BaseURL: "/",
		},
		Detect: RepoDetect{
			Paths: []string{
				"docs/**",
				"blog/**",
				"src/**",
				"static/**",
				"**/*.md",
				"**/*.mdx",
				"docusaurus.config.*",
				"sidebars.*",
				"package.json",
				"package-lock.json",
				RepoConfigName,
			},
		},
	}
}

// LoadRepoConfig reads .docpreview.yml from the root of a checked-out
// repository. A missing file yields the defaults.
func LoadRepoConfig(repoRoot string) (RepoConfig, error) {
	cfg := DefaultRepoConfig()

	raw, err := os.ReadFile(filepath.Join(repoRoot, RepoConfigName))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", RepoConfigName, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", RepoConfigName, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("invalid %s: %w", RepoConfigName, err)
	}
	return cfg, nil
}

func (c *RepoConfig) validate() error {
	if c.Build.Dir == "" {
		c.Build.Dir = "."
	}
	if c.Build.Output == "" {
		c.Build.Output = "build"
	}
	if c.Build.BaseURL == "" {
		c.Build.BaseURL = "/"
	}
	if c.Build.Command == "" {
		c.Build.Command = DefaultBuildCommand
	}

	for name, p := range map[string]string{
		"build.dir":     c.Build.Dir,
		"build.output":  c.Build.Output,
		"detect.script": c.Detect.Script,
	} {
		if p == "" {
			continue
		}
		if err := checkContained(p); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	base, err := NormalizeBaseURL(c.Build.BaseURL)
	if err != nil {
		return fmt.Errorf("build.base_url: %w", err)
	}
	c.Build.BaseURL = base

	return nil
}

// checkContained rejects paths that could escape the workspace.
func checkContained(p string) error {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return fmt.Errorf("must be relative, got %q", p)
	}
	if len(p) > 1 && p[1] == ':' {
		return fmt.Errorf("must not be a drive-qualified path, got %q", p)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("must not escape the repository root, got %q", p)
	}
	return nil
}

// NormalizeBaseURL canonicalizes a Docusaurus baseUrl to the "/" or "/foo/"
// form that Docusaurus itself requires: a leading slash and a trailing slash.
//
// This is called on both the build side and the serve side from the same
// stored value, which is what guarantees the mount prefix and the baked-in
// asset URLs agree. Getting this wrong is the single most common way a
// self-hosted preview ends up as an unstyled wall of text.
func NormalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "/", nil
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("must be a path, not a URL, got %q", raw)
	}
	if strings.Contains(s, "..") {
		return "", fmt.Errorf("must not contain %q, got %q", "..", raw)
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s, nil
}
