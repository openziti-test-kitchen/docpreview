package daemon

import (
	"net/http"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// The plan is the projects form's answer to "what will this actually do".
//
// Nineteen fields, most of them blank, resolved through a preset and a server default and
// a file in the repository — the form could explain every box and still leave nobody able
// to state the command that would run. So the server states it, from the same
// resolveProject the build calls.
//
// Served rather than computed in the page on purpose. The precedence lives in one
// function; a second copy of it in JavaScript is a form that promises one build command
// and a build that runs another, and the two would drift in the direction nobody tests.

// planLine is one line of the plan: what happens, the value it happens with, and where
// that value came from.
type planLine struct {
	Key   string `json:"key"`
	Value string `json:"value"`

	// From is the source that won, for the badge beside the line: "project" is a field
	// the operator typed, "preset" is the framework's, "server" is the daemon's config,
	// "repo" is the repository's own .docpreview.yml, and "build" is decided on the build
	// host when the tree is there to look at.
	From string `json:"from"`

	// Note is the one thing worth saying about the line that the value does not say.
	Note string `json:"note,omitempty"`
}

// planView is the whole plan, plus what the page should say above and below it.
type planView struct {
	Lines []planLine `json:"lines"`

	// Name is the preview name this installation would ask its exposer for, rendered
	// through the real name template and the real prefix.
	//
	// A name rather than a URL, and that is not a shortcut: under zrok the hostname comes
	// back from the controller when the share is created, so a URL printed here would be a
	// guess that stays right only until somebody changes a namespace. The name is exact.
	Name string `json:"name,omitempty"`

	// NameWhy says which branch the name was rendered for, because every pull request
	// gets its own and a single example otherwise reads as the one address.
	NameWhy string `json:"name_why,omitempty"`

	// Warn is a resolved value that will not work — today, only a build driver this
	// daemon will refuse.
	Warn string `json:"warn,omitempty"`
}

// planBranch is the branch the example name is rendered for. "main" rather than the
// project's real default branch, which is not known until the platform is asked and is
// not worth a network call to decorate a form.
const planBranch = "main"

// plan resolves an unsaved project row and returns what a build of it would do.
//
// The row comes from the request body and is never stored: this is the form's current
// state, including fields that have been typed and not yet saved, which is the whole
// point. The plan has to answer for what is on the screen rather than for what is in the
// database.
func (a *ProjectsAdmin) plan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Platform     string `json:"platform"`
		Owner        string `json:"owner"`
		Repo         string `json:"repo"`
		Framework    string `json:"framework"`
		BuildDir     string `json:"build_dir"`
		BuildCommand string `json:"build_command"`
		BuildOutput  string `json:"build_output"`
		BaseURL      string `json:"base_url"`
		Driver       string `json:"driver"`
		Image        string `json:"image"`
		Timeout      string `json:"timeout"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	p := store.Project{
		Platform:     strings.TrimSpace(body.Platform),
		Owner:        strings.TrimSpace(body.Owner),
		Repo:         strings.TrimSpace(body.Repo),
		Framework:    strings.TrimSpace(body.Framework),
		BuildDir:     strings.TrimSpace(body.BuildDir),
		BuildCommand: strings.TrimSpace(body.BuildCommand),
		BuildOutput:  strings.TrimSpace(body.BuildOutput),
		BaseURL:      strings.TrimSpace(body.BaseURL),
		Driver:       strings.TrimSpace(body.Driver),
		Image:        strings.TrimSpace(body.Image),
		Timeout:      strings.TrimSpace(body.Timeout),
	}
	writeJSON(w, http.StatusOK, a.planFor(p))
}

// planFor is plan's body with the decoding taken off, so it can be tested as a function
// of a row rather than of a request.
func (a *ProjectsAdmin) planFor(p store.Project) planView {
	// An unparseable timeout is dropped by resolveProject with a warning that goes
	// nowhere here. The form refuses one on save, so the only way to reach this with one
	// is mid-typing — and a plan showing the server default while "4" is in the box is
	// telling the truth about what that row would do.
	cfg := resolveProject(config.DefaultRepoConfig(), p, func(string, ...any) {})

	preset, hasPreset := config.FrameworkByID(p.Framework)

	// Which source won, field by field. Computed rather than inferred by comparing the
	// resolved value against each candidate: a project field that happens to equal the
	// preset's would otherwise be reported as the preset's, and this badge exists to tell
	// the operator whether their own typing is in play.
	from := func(own, presetValue string) string {
		switch {
		case own != "":
			return "project"
		case hasPreset && presetValue != "":
			return "preset"
		default:
			return "repo"
		}
	}
	const repoNote = "blank here, so the repository's own .docpreview.yml decides; " +
		"this is what it gets if that file says nothing"
	noteFor := func(src string) string {
		if src == "repo" {
			return repoNote
		}
		return ""
	}

	var v planView
	line := func(key, value, src, note string) {
		v.Lines = append(v.Lines, planLine{Key: key, Value: value, From: src, Note: note})
	}

	if p.Platform != "" && p.Owner != "" && p.Repo != "" {
		line("clone", p.Platform+":"+p.Owner+"/"+p.Repo, "project", "")
	}

	dirFrom := from(p.BuildDir, preset.Dir)
	line("cd", cfg.Build.Dir, dirFrom, noteFor(dirFrom))

	// Not resolvable from here, and not worth pretending otherwise: the package manager
	// is whichever lockfile is committed in that directory, which is a fact about a tree
	// this process has not cloned.
	line("install", "from the lockfile in that directory", "build",
		"yarn.lock, pnpm-lock.yaml or package-lock.json, checked in that order")

	cmdFrom := from(p.BuildCommand, preset.BuildCommand)
	line("build", cfg.Build.Command, cmdFrom, noteFor(cmdFrom))

	outFrom := from(p.BuildOutput, preset.Output)
	line("serve", cfg.Build.Output, outFrom, noteFor(outFrom))

	if cfg.Build.BaseURL != "" && cfg.Build.BaseURL != "/" {
		baseFrom := "repo"
		if p.BaseURL != "" {
			baseFrom = "project"
		}
		line("base url", cfg.Build.BaseURL, baseFrom, "")
	}

	driver, driverFrom := p.Driver, "project"
	if driver == "" {
		driver, driverFrom = a.cfg.Build.Driver, "server"
	}
	line("driver", driver, driverFrom, "")
	// The local driver runs the pull request's own build scripts on this host, so it is
	// refused unless the operator wrote it down. Said here rather than an hour into the
	// first build that fails for it.
	if driver == config.DriverLocal && !a.cfg.Build.AllowLocalDriver {
		v.Warn = "the local driver is not enabled on this daemon, so every build would " +
			"fail: set build.allow_local_driver in the server config, or use docker"
	}

	image, imageFrom := p.Image, "project"
	if image == "" {
		image, imageFrom = a.cfg.Build.Image, "server"
	}
	if image != "" {
		line("image", image, imageFrom, "")
	}

	// cfg.Build.Timeout is zero unless the row set a usable one. The server value is
	// never written into it, because a repository must not be able to decide how long it
	// may occupy a worker.
	if cfg.Build.Timeout > 0 {
		line("timeout", cfg.Build.Timeout.String(), "project", "")
	} else {
		line("timeout", a.cfg.Build.Timeout.String(), "server", "")
	}

	if p.Repo != "" && a.planNamer != nil {
		pr := model.PullRequest{
			Repo: model.Repo{
				Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo,
			},
			Branch: planBranch,
		}
		if name, err := a.planNamer(pr); err == nil {
			v.Name = name
			v.NameWhy = "for branch " + planBranch + "; every pull request gets its own"
		}
	}
	return v
}
