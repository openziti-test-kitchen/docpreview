package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/redact"
)

// Result is a finished build.
type Result struct {
	// OutputDir is the directory of static files to serve.
	OutputDir string

	// Log is the combined build output.
	Log string

	// Duration is how long the build took.
	Duration time.Duration
}

// Builder turns a checked-out workspace into a directory of static files.
type Builder struct {
	defaults config.BuildDefaults
	log      *slog.Logger

	// secrets are environment variables injected into every build, from the
	// server config rather than from the repository.
	secrets map[string]string

	// redactor scrubs those values out of everything this package emits — the
	// build log, and the text of every error it returns. Both end up in a pull
	// request comment, which is the most public place a leaked credential
	// could land.
	redactor *redact.Redactor
}

// NewBuilder returns a Builder with no injected secrets.
func NewBuilder(defaults config.BuildDefaults, log *slog.Logger) *Builder {
	r, _ := redact.New(nil)
	return &Builder{defaults: defaults, log: log.With("component", "build"), redactor: r}
}

// NewBuilderWithSecrets returns a Builder that injects secrets into each build
// and redacts their values from all of its output.
//
// The redactor is built from the values, not the names. A build that prints its
// own environment — which npm does on failure, and any script run under `set -x`
// does always — therefore produces asterisks rather than a credential.
func NewBuilderWithSecrets(defaults config.BuildDefaults, secrets map[string]string, log *slog.Logger) *Builder {
	log = log.With("component", "build")

	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		values = append(values, v)
	}

	r, tooShort := redact.New(values)
	if len(tooShort) > 0 {
		// Naming the variable would be useful and is exactly the wrong thing to
		// print, since the name is the lookup key into the vault. The count is
		// enough to prompt a look at the config.
		log.Warn("some build secrets are too short to redact and will appear in logs verbatim",
			"count", len(tooShort))
	}
	if r.Active() {
		log.Info("build log redaction armed", "patterns", r.Count())
	}

	return &Builder{defaults: defaults, log: log, secrets: secrets, redactor: r}
}

// Redactor exposes the scrubber so callers can apply it to text that passes
// through them — a report's log excerpt, an error on its way to a comment.
func (b *Builder) Redactor() *redact.Redactor { return b.redactor }

// Build runs the site build and verifies the result is servable at the
// configured base URL.
//
// Every value this returns is scrubbed of injected secrets — the log on the
// success path and the error text on all five failure paths. That is done once,
// here, in a deferred wrapper rather than at each return: a redaction that has
// to be remembered at every `return` is a redaction that will be missed the
// next time somebody adds one.
// sink, when non-nil, receives build output as it is produced. That is what a
// live tail reads from; it is separate from the returned log because the return
// value is what goes into a comment and has to be complete, whereas the sink
// only has to be timely.
func (b *Builder) Build(ctx context.Context, ws *Workspace, cfg config.RepoConfig, sink io.Writer) (result *Result, err error) {
	defer func() {
		if !b.redactor.Active() {
			return
		}
		if result != nil {
			result.Log = b.redactor.Scrub(result.Log)
		}
		err = b.redactor.ScrubError(err)
	}()

	started := time.Now()

	buildDir := filepath.Join(ws.Dir, filepath.FromSlash(cfg.Build.Dir))
	if _, err := os.Stat(filepath.Join(buildDir, "package.json")); err != nil {
		return nil, fmt.Errorf("no package.json in %q; set build.dir in %s: %w",
			cfg.Build.Dir, config.RepoConfigName, err)
	}

	timeout := b.defaults.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var out string
	switch b.defaults.Driver {
	case "docker":
		out, err = b.buildDocker(ctx, ws, buildDir, cfg, sink)
	default:
		out, err = b.buildLocal(ctx, ws, buildDir, cfg, sink)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("build timed out after %s:\n%s", timeout, out)
		}
		return nil, fmt.Errorf("%w\n%s", err, out)
	}

	outputDir := filepath.Join(buildDir, filepath.FromSlash(cfg.Build.Output))
	if stat, statErr := os.Stat(outputDir); statErr != nil || !stat.IsDir() {
		return nil, fmt.Errorf("build produced no output at %q; set build.output in %s\n%s",
			cfg.Build.Output, config.RepoConfigName, out)
	}

	if err := verifyBaseURL(outputDir, cfg.Build.BaseURL); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, out)
	}

	return &Result{
		OutputDir: outputDir,
		Log:       out,
		Duration:  time.Since(started),
	}, nil
}

// buildEnv is the environment handed to a site build.
//
// DOCUSAURUS_BASE_URL and DOCUSAURUS_URL are the conventional names a
// Docusaurus config reads to make baseUrl configurable; DOCPREVIEW_BASE_URL is
// ours, for configs that would rather not pretend to be generic. All three are
// set because we cannot know which one a given repository honors, and setting
// an unread variable costs nothing.
//
// Repository-supplied Env entries are applied last but cannot override the
// base URL variables: letting a pull request set DOCUSAURUS_BASE_URL would let
// it break its own preview in a way that looks like our bug.
func (b *Builder) buildEnv(pr model.PullRequest, cfg config.RepoConfig, base []string) []string {
	reserved := map[string]bool{
		"DOCUSAURUS_BASE_URL": true,
		"DOCPREVIEW_BASE_URL": true,
		"DOCPREVIEW":          true,
		"DOCPREVIEW_COMMIT":   true,
		"DOCPREVIEW_BRANCH":   true,
		"DOCPREVIEW_PR":       true,
		"DOCPREVIEW_REPO":     true,
	}

	// Operator-supplied secrets are reserved too. A repository must not be able
	// to shadow ALGOLIA_WRITE_KEY with a value of its own and watch what the
	// build does differently — and more importantly, must not be able to set it
	// to a value the redactor does not know about.
	for name := range b.secrets {
		reserved[strings.ToUpper(name)] = true
	}

	env := append([]string{}, base...)
	keys := make([]string, 0, len(cfg.Build.Env))
	for k := range cfg.Build.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic, so a build is reproducible
	for _, k := range keys {
		if reserved[strings.ToUpper(k)] {
			continue
		}
		env = append(env, k+"="+cfg.Build.Env[k])
	}

	// Operator secrets last, so nothing above can have overridden them.
	names := make([]string, 0, len(b.secrets))
	for name := range b.secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+b.secrets[name])
	}

	return append(env,
		"DOCPREVIEW=1",
		"DOCPREVIEW_BASE_URL="+cfg.Build.BaseURL,
		"DOCUSAURUS_BASE_URL="+cfg.Build.BaseURL,

		// What is being built. A generated site otherwise has no way to know
		// its own commit, so a preview cannot say which push produced it — and
		// "is this the old one or the new one?" is the question a reviewer
		// staring at a preview during a rebuild actually has. Vercel exposes
		// the same three under VERCEL_GIT_*.
		"DOCPREVIEW_COMMIT="+pr.HeadSHA,
		"DOCPREVIEW_BRANCH="+pr.Branch,
		"DOCPREVIEW_REPO="+pr.Repo.String(),
		"DOCPREVIEW_PR="+strconv.Itoa(pr.Number),

		// CI makes npm and Docusaurus quieter and non-interactive.
		"CI=true",
	)
}

// buildLocal runs the build directly on this host.
//
// This executes package.json scripts written by whoever opened the pull
// request, with this process's privileges. That is fine for a repository whose
// contributors you already trust — the case docpreview is built for — and
// unacceptable for a public one. For public repositories use the docker driver,
// which is why fork pull requests are refused outright at the webhook layer.
func (b *Builder) buildLocal(ctx context.Context, ws *Workspace, buildDir string, cfg config.RepoConfig, sink io.Writer) (string, error) {
	env := b.buildEnv(ws.PR, cfg, os.Environ())

	install := installCommand(buildDir)

	var log bytes.Buffer
	out := tee(&log, sink)

	for _, step := range []struct {
		name string
		cmd  string
	}{
		{"install", install},
		{"build", cfg.Build.Command},
	} {
		fmt.Fprintf(out, "$ %s\n", step.cmd)
		if err := runShell(ctx, buildDir, env, step.cmd, out); err != nil {
			return log.String(), fmt.Errorf("%s step failed: %w", step.name, err)
		}
		fmt.Fprintln(out)
	}

	return log.String(), nil
}

// installCommand picks the dependency install command from the lockfile the
// repository committed.
//
// Running the wrong package manager is not merely slower: `npm ci` against a
// tree with only a yarn.lock fails outright, and `npm install` there resolves a
// different dependency graph than the one the author tested. The lockfile is
// the author's statement of which manager owns this tree, so it decides.
//
// Each branch keeps a fallback because the strict flag differs by major
// version and is rejected outright by the other: --frozen-lockfile is yarn 1,
// --immutable is yarn 2+. Falling back to a plain install is worse than failing
// on a drifted lockfile, but better than refusing to build at all.
func installCommand(dir string) string {
	switch {
	case exists(filepath.Join(dir, "yarn.lock")):
		return "yarn install --frozen-lockfile --non-interactive || yarn install --immutable || yarn install"
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm install --frozen-lockfile || pnpm install"
	case exists(filepath.Join(dir, "package-lock.json")):
		return "npm ci --no-audit --no-fund"
	default:
		// npm ci requires a lockfile and fails flatly without one.
		return "npm install --no-audit --no-fund"
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// buildDocker runs the build inside a throwaway container.
//
// The container gets the workspace bind-mounted and nothing else: no network
// namespace sharing, no host environment, a read-only root filesystem would be
// nice but npm needs to write, so instead the mount is the only writable path
// that outlives the container. This is the driver to use for repositories whose
// contributors are not fully trusted.
func (b *Builder) buildDocker(ctx context.Context, ws *Workspace, buildDir string, cfg config.RepoConfig, sink io.Writer) (string, error) {
	image := b.defaults.Image
	if image == "" {
		image = "node:24-bookworm-slim"
	}

	// The workspace-relative path of the build directory, as the container will
	// see it.
	rel, err := filepath.Rel(ws.Dir, buildDir)
	if err != nil {
		return "", fmt.Errorf("locating build dir inside workspace: %w", err)
	}
	containerDir := "/workspace/" + filepath.ToSlash(rel)
	containerDir = strings.TrimSuffix(containerDir, "/.")

	hostPath, err := filepath.Abs(ws.Dir)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path: %w", err)
	}
	if runtime.GOOS == "windows" {
		// Docker Desktop accepts forward-slashed Windows paths; backslashes are
		// parsed as part of the mount option string and produce a baffling
		// "invalid reference format".
		hostPath = filepath.ToSlash(hostPath)
	}

	args := []string{
		"run", "--rm",
		"--workdir", containerDir,
		"--volume", hostPath + ":/workspace",
		// Containers that outlive their build are the usual way a small build
		// host runs out of memory.
		"--memory", "4g",
		"--cpus", "2",
	}
	for _, kv := range b.buildEnv(ws.PR, cfg, nil) {
		args = append(args, "--env", kv)
	}
	args = append(args, image, "sh", "-lc",
		installCommand(buildDir)+" && "+cfg.Build.Command)

	var log bytes.Buffer
	out := tee(&log, sink)
	fmt.Fprintf(out, "$ docker run %s ...\n", image)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		return log.String(), fmt.Errorf("docker build failed: %w", err)
	}
	return log.String(), nil
}

// tee returns a writer feeding both the in-memory log and the live sink.
//
// The buffer is what the caller returns and what ends up in a comment; the sink
// is what a browser is tailing. Both need every byte, and the sink may be nil
// when nobody is capturing.
func tee(buf *bytes.Buffer, sink io.Writer) io.Writer {
	if sink == nil {
		return buf
	}
	return io.MultiWriter(buf, sink)
}

// runShell executes a command line through the platform's shell, streaming its
// output.
//
// Streaming rather than CombinedOutput because a live tail is the point: a
// build that buffers until it exits gives a browser nothing to show for the two
// minutes it is running, which is exactly the two minutes somebody is watching.
//
// stdout and stderr both go to the same writer, so interleaving matches what a
// terminal would show. That makes the writer's own concurrency safety a
// requirement rather than a nicety — os/exec gives each stream its own
// goroutine when the destination is not an *os.File.
func runShell(ctx context.Context, dir string, env []string, line string, out io.Writer) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", line)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", line)
	}
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out

	return cmd.Run()
}

// absoluteRef matches href and src attributes holding a root-relative path.
//
// All three quoting forms are handled, and the unquoted one is not a
// curiosity — it is what Docusaurus actually emits. Its production build
// minifies the HTML and drops quotes wherever the value has no spaces, so real
// output looks like:
//
//	<link href=/zrok/assets/css/styles.160a45a5.css rel=stylesheet>
//
// A pattern that only matched href="..." found zero references in every real
// build, which meant the base URL check silently passed on exactly the sites it
// exists to protect.
var absoluteRef = regexp.MustCompile(`(?:href|src)=(?:"(/[^"]*)"|'(/[^']*)'|(/[^\s>]*))`)

// refsFrom extracts the root-relative href and src values from HTML.
func refsFrom(html string) []string {
	var out []string
	for _, m := range absoluteRef.FindAllStringSubmatch(html, -1) {
		// Exactly one of the three alternatives captured.
		for _, group := range m[1:] {
			if group == "" {
				continue
			}
			// Protocol-relative URLs point at another host entirely.
			if strings.HasPrefix(group, "//") {
				break
			}
			out = append(out, group)
			break
		}
	}
	return out
}

// verifyBaseURL checks that the built site's asset URLs agree with the base URL
// the preview will be served at.
//
// This is the single most common way a self-hosted documentation preview goes
// wrong, and it fails in the most misleading way possible. Docusaurus bakes
// baseUrl into every emitted href and src at build time. If the repository's
// docusaurus.config.js hardcodes "/my-project/" — which it does, because that
// is what deploying to GitHub Pages requires — and we serve the output at "/",
// then index.html loads, every stylesheet and script 404s, and the reviewer
// sees an unstyled wall of text and reports that "the preview is broken".
// Nothing in the build log hints at the cause.
//
// So we check, and refuse to publish a preview we know is broken. The error
// says which value the site was built with and how to fix it, which turns a
// half-hour of confusion into a one-line config change.
//
// Exported as VerifyBaseURL because the check is needed twice. A build knows the
// value it just used; a **republish** at startup does not — it takes the base URL
// from a stored row, and that row can disagree with the artifacts beside it. It
// does so whenever the exposer changes, since a path-mounting exposer folds its
// prefix into the base URL and a host-per-preview one does not. Republishing
// without checking serves a site built for one prefix at another, which is
// precisely the unstyled-wall-of-text this function exists to prevent.
func VerifyBaseURL(outputDir, baseURL string) error { return verifyBaseURL(outputDir, baseURL) }

func verifyBaseURL(outputDir, baseURL string) error {
	index := filepath.Join(outputDir, "index.html")
	raw, err := os.ReadFile(index)
	if err != nil {
		// Not every static site generator emits a root index.html, and a site
		// that does not is not necessarily broken — it just cannot be checked
		// this way.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", index, err)
	}

	refs := refsFrom(string(raw))
	if len(refs) < minRefsToInfer {
		// A site built with relative URLs, or with too few absolute ones to
		// draw a conclusion from, works at any prefix.
		return nil
	}

	// Two questions, and only one of them can be asked at a time.
	//
	// When a prefix is expected, the direct question is answerable: do the
	// references actually start with it? Inference cannot answer this, because
	// it only ever reports the first path segment — so a site correctly built
	// for "/preview/handbook-new-install-guide/" infers as "/preview/" and gets
	// rejected for a mismatch that does not exist. That is not hypothetical; it
	// failed every build the first time previews moved onto a path.
	//
	// When "/" is expected there is nothing to prefix-match — every absolute
	// path starts with "/" — so the check has to run the other way round and
	// infer whether the site really wanted a prefix nobody configured. An
	// earlier version used prefix matching for both, which made the "/" case
	// vacuously true and let the trap it exists to catch through.
	if baseURL != "" && baseURL != "/" {
		// A majority, not all of them. A hand-written href="/" in a footer is
		// legal and common, and failing a correct build over one of them would
		// make the check the problem. The same threshold as inference, for the
		// same reason: the two distributions this separates are nowhere near
		// each other, so the exact number does not matter.
		var matched int
		var bad string
		for _, ref := range refs {
			if strings.HasPrefix(ref, baseURL) {
				matched++
			} else if bad == "" {
				bad = ref
			}
		}
		if float64(matched)/float64(len(refs)) >= dominantShare {
			return nil
		}
		return baseURLMismatch(baseURL, inferBaseURL(refs), bad)
	}

	built := inferBaseURL(refs)
	if built == baseURL || built == "/" {
		return nil
	}
	return baseURLMismatch(baseURL, built, refs[0])
}

func baseURLMismatch(baseURL, built, sample string) error {

	return fmt.Errorf(
		"the site was built for a different base URL than the preview will serve.\n"+
			"  preview base URL:    %s\n"+
			"  built-in base URL:   %s\n"+
			"  asset in index.html: %s\n"+
			"\nServing it anyway would load index.html and 404 every stylesheet and script,\n"+
			"which looks like a broken preview rather than a configuration mismatch.\n"+
			"\nFix it either way round:\n"+
			"  - set build.base_url to %q in %s, or\n"+
			"  - make docusaurus.config read the environment:\n"+
			"        baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '%s',\n",
		baseURL, built, sample, built, config.RepoConfigName, built)
}

// minRefsToInfer is how many absolute references index.html must contain before
// the base URL can be inferred from them. Below this the sample is too small to
// tell a base path from an ordinary directory.
const minRefsToInfer = 3

// dominantShare is the fraction of references that must share a first path
// segment for that segment to be read as the base URL.
//
// A root-mounted Docusaurus site scatters its references across /assets, /img,
// /docs, and /blog, so no single segment comes close. A site built for
// /my-project/ puts every single reference under that one segment. The gap
// between those two distributions is enormous, so the exact threshold barely
// matters; 0.6 sits comfortably in the middle and tolerates a stray
// hand-written link or two.
const dominantShare = 0.6

// inferBaseURL works out what base URL a built site actually used, by looking
// at the first path segment of its absolute references.
//
// The obvious implementation — longest common prefix — is wrong in both
// directions. A single hand-written href="/" in a footer collapses the prefix
// to "/" and hides a real mismatch, while a site whose every asset happens to
// live under /assets/ reports its base URL as "/assets/". Counting segments
// instead is robust to both.
func inferBaseURL(refs []string) string {
	counts := map[string]int{}
	for _, ref := range refs {
		trimmed := strings.Trim(ref, "/")
		if trimmed == "" {
			// A bare "/" is a link to the site root; it says nothing about the
			// base path, but it does count towards the total.
			continue
		}
		first, _, _ := strings.Cut(trimmed, "/")
		counts[first]++
	}

	top, topCount := "", 0
	for seg, n := range counts {
		if n > topCount {
			top, topCount = seg, n
		}
	}

	if topCount < 2 || float64(topCount)/float64(len(refs)) < dominantShare {
		return "/"
	}
	return "/" + top + "/"
}
