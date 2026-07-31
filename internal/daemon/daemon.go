// Package daemon wires the pieces together: it receives webhooks, runs builds,
// publishes previews, and keeps the pull request comment current.
//
// The shape is a queue with a fixed worker pool rather than a goroutine per
// webhook. A documentation repository can receive a dozen pushes in a minute
// during a review, and each build is an npm install; letting them all run at
// once turns a laptop into a space heater and makes every build slower than
// running them two at a time.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/netfoundry/docpreview/internal/buildlog"
	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
	"github.com/netfoundry/docpreview/internal/preview"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// pollInterval is how often an idle worker checks the queue.
//
// Workers are also woken directly when a job is enqueued, so this is only the
// fallback that recovers jobs left in the database by a previous process. A
// second is short enough that a restart is not noticeable and long enough that
// an idle daemon is not spinning.
const pollInterval = time.Second

// Daemon is the running service.
type Daemon struct {
	cfg     config.Server
	log     *slog.Logger
	store   *store.Store
	exposer expose.Exposer

	// clients maps a platform to the client that talks to it. Guarded by mu:
	// the GitHub client cannot be built until the vault is unlocked, which the
	// setup page can do long after the daemon started serving. See SetClient.
	clients map[model.Platform]scm.Client

	cloner   *pipeline.Cloner
	detector *pipeline.Detector
	builder  *pipeline.Builder

	// secrets are injected into every build, from the server config.
	secrets map[string]string

	// starting is true from construction until recovery finishes. Atomic rather than under
	// mu: it is read by every /status while recovery holds no lock, and written once.
	starting atomic.Bool

	// startupDone and startupTotal count exposer round trips during recovery, for the
	// banner's progress bar. The total is written once, before any goroutine starts, and
	// read-only afterwards; the counter is atomic because every restore increments it.
	startupDone  atomic.Int64
	startupTotal int

	// startupItems is what recovery has actually done, most recent last, for the banner.
	//
	// A count and a stage answer "how far" and not "what is it doing" — which is the
	// question a four-minute wait provokes, and the one that got asked. Guarded by its
	// own mutex rather than d.mu: it is written from every restore goroutine and read by
	// every /status, and none of that should queue behind a build's locking.
	itemsMu      sync.Mutex
	startupItems []string

	// Outcome counters for the startup report, split by adopted and created because the
	// difference between them is the difference between a ten-second restart and a
	// four-minute one. Atomic: every restore goroutine increments them.
	adoptedPreviews atomic.Int64
	createdPreviews atomic.Int64
	adoptedBuilds   atomic.Int64
	createdBuilds   atomic.Int64

	// startupOrphans is how many published shares the database no longer claimed. Written
	// once during recovery, before the restore goroutines start, so it needs no lock.
	startupOrphans int

	// adoptable is what the exposer already has published, keyed by publication key,
	// read during recovery to decide between taking a share over and creating one.
	// Written once before the restore loops start and read-only afterwards.
	adoptable map[string]expose.Adoptable

	// lastStartup is the report of the most recent recovery, kept after it finishes so
	// the dashboard can show what happened. Retained rather than cleared: the interesting
	// moment is immediately *after* a startup nobody could see the middle of.
	lastStartup atomic.Pointer[StartupSummary]

	// startup is the stage recovery is in, for the banner. A pointer swapped whole
	// rather than fields mutated in place, for the same reason `starting` is atomic:
	// every /status reads it while recovery is holding no lock, and a half-updated
	// stage would render a count against the wrong phase.
	startup atomic.Pointer[StartupProgress]

	// namePrefix is what every published name starts with, so two installations can share
	// one exposer account. The config file's value, unless an operator has overridden it
	// from the dashboard.
	//
	// Held here rather than read from the store per build, because `previewName` is on the
	// build path and this changes about once in the life of an installation. Atomic because
	// the dashboard writes it while builds read it.
	namePrefix atomic.Pointer[string]

	// removeCacheVolumes deletes a preview's docker cache volumes. A field because it
	// shells out to docker: a test running the real one spends a subprocess per teardown,
	// and the version of this that deleted volumes by name deleted live ones. Defaulted in
	// New.
	removeCacheVolumes func(ctx context.Context, previewID string) error

	// projectSecretsFn resolves the environment variables belonging to one project,
	// which are not the same for two projects and are not in the server config at
	// all. Guarded by mu; see SetProjectSecrets for why it is a function.
	projectSecretsFn func(platform, owner, repo string) map[string]string

	// events is a window onto what just happened, for the dashboard.
	events *eventLog

	// logs owns build output: on disk for download, and fanned out live.
	logs *buildlog.Store

	// wake nudges an idle worker. Buffered and non-blocking: a send that would
	// block means a worker is already about to look at the queue anyway.
	wake chan struct{}

	// mu guards live, running, commit, clients, builder and secrets.
	mu sync.Mutex
	// live maps preview ID to its publication, so a rebuild can replace it.
	live map[string]*expose.Publication
	// liveBuilds maps "<preview>/<build>" to that build's own publication, held
	// separately from live so the branch share's lifecycle — supersede, teardown,
	// the commit lock — is untouched by however many build shares exist beside it.
	liveBuilds map[string]*expose.Publication
	// running maps preview ID to the in-flight build for that pull request, so
	// a newer push can abandon an older build rather than queue behind it.
	running map[string]*build
	// commit serializes the publishing phase per preview. See commitLock.
	commit map[string]*sync.Mutex
	// reported is the furthest lifecycle state reported per preview, so a late
	// report cannot move a comment backwards. See staleReport.
	reported map[string]reportMark

	// publisher coalesces platform writes. Not guarded by mu — it has its own.
	publisher *publisher

	// instance identifies this process, so an open dashboard can notice that the
	// daemon behind it has restarted and its own code is stale. Set once at
	// construction and never written again, so it needs no lock.
	instance string
}

// build is one generation of work for a pull request.
//
// It exists as a pointer so that identity is comparable. "Am I still the
// current build for this preview?" cannot be answered by comparing cancel
// functions — func values are not comparable in Go — and comparing anything
// else would make a superseded build indistinguishable from the one that
// replaced it.
type build struct {
	cancel context.CancelFunc

	// pr and started describe what is running, for Status. A build in flight
	// has no preview record until it commits — the first build of a branch
	// exists nowhere else — so without this the dashboard cannot show a row
	// for it at all.
	pr      model.PullRequest
	started time.Time
}

// New builds a Daemon.
//
// A log store that cannot be created is not fatal: the daemon degrades to
// building without capturing output rather than refusing to start, since a full
// disk should not take previews down entirely.
func New(
	cfg config.Server,
	st *store.Store,
	exposer expose.Exposer,
	clients map[model.Platform]scm.Client,
	log *slog.Logger,
) *Daemon {
	logs, err := buildlog.NewStore(cfg.LogsDir())
	if err != nil {
		log.Error("build logs are disabled", "error", err)
		logs = nil
	}

	d := &Daemon{
		cfg:     cfg,
		log:     log,
		store:   st,
		exposer: exposer,
		// Copied, not aliased. The caller hands the same map to Ingress, and
		// each now owns its own so that SetClient on one is not an
		// unsynchronized write into the other's reads.
		clients:    maps.Clone(clients),
		cloner:     pipeline.NewCloner(cfg.WorkspacesDir(), log),
		detector:   pipeline.NewDetector(log),
		builder:    pipeline.NewBuilder(cfg.Build, log),
		secrets:    map[string]string{},
		logs:       logs,
		wake:       make(chan struct{}, 1),
		instance:   time.Now().UTC().Format("20060102-150405.000"),
		live:       map[string]*expose.Publication{},
		liveBuilds: map[string]*expose.Publication{},
		running:    map[string]*build{},
		commit:     map[string]*sync.Mutex{},
		reported:   map[string]reportMark{},
		events:     newEventLog(),

		removeCacheVolumes: pipeline.RemoveCacheVolumes,
	}
	d.publisher = newPublisher(reportDebounce, d.publishReport)
	return d
}

// recordFailure makes a failed build visible without destroying a working one.
//
// Two things are true at once after a failed build, and the obvious
// implementations each throw one of them away.
//
// Overwriting the preview row with state "failed" loses the fact that a
// previous build is *still serving* — the old preview is live and correct, and
// on the next restart the recovery pass would skip a non-ready row and the
// reaper would then delete a share that was working fine.
//
// Writing nothing at all, which is what happened before, means a failure has no
// row, so it never appears on the dashboard and there is nothing to click to
// reach its log. Which is exactly when somebody wants the log.
//
// So: leave an existing preview alone — it is still true — and insert a row only
// when there is nothing there, which is the first-build-fails case. The build
// log is written either way and is reachable by preview ID.
func (d *Daemon) recordFailure(ctx context.Context, pr model.PullRequest, buildErr error) {
	id := pr.PreviewID()

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	existing, err := d.store.ListPreviews(saveCtx)
	if err != nil {
		d.log.Warn("checking for an existing preview after a failure", "error", err)
		return
	}
	for _, p := range existing {
		if p.PreviewID == id {
			// A working preview survives a failed rebuild. Saying otherwise
			// would be a lie the reaper acts on.
			return
		}
	}

	if err := d.store.SavePreview(saveCtx, store.Preview{
		PreviewID: id,
		PR:        pr,
		Commit:    pr.HeadSHA,
		State:     scm.StateFailed,
		Reason:    d.scrub(firstLine(buildErr.Error())),
		UpdatedAt: time.Now(),
	}); err != nil {
		d.log.Warn("recording a failed build", "error", err)
	}
}

// recordSkip records a pull request that was considered and not built.
//
// Without a row a skipped pull request appears only in the activity feed, so the
// list shows nothing for it and the preview picker cannot offer it — while a
// *failed* build does get a row and does appear. That inconsistency is the bug:
// both outcomes are "docpreview looked at this pull request and produced no
// preview", and both are things somebody needs to see to know the webhook is
// working at all.
//
// A row with no name, URL or artifact directory. Recovery only republishes `ready`
// rows and drops any whose artifact directory is missing, so this cannot resurrect
// as a broken preview; the reaper ages it out on preview.ttl like any other.
//
// An existing preview survives a skip, for the same reason it survives a failed
// rebuild: a branch whose latest push touched no documentation still has a working
// preview of the documentation it did touch, and overwriting that row would make
// the reaper delete a live share.
func (d *Daemon) recordSkip(ctx context.Context, pr model.PullRequest, reason string) {
	id := pr.PreviewID()

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	existing, err := d.store.ListPreviews(saveCtx)
	if err != nil {
		d.log.Warn("checking for an existing preview before recording a skip", "error", err)
		return
	}
	for _, p := range existing {
		if p.PreviewID == id {
			return
		}
	}

	if err := d.store.SavePreview(saveCtx, store.Preview{
		PreviewID: id,
		PR:        pr,
		Commit:    pr.HeadSHA,
		State:     scm.StateSkipped,
		Reason:    reason,
		UpdatedAt: time.Now(),
	}); err != nil {
		d.log.Warn("recording a skipped pull request", "error", err)
	}
}

// logBuildID names a build's log file.
//
// Commit plus timestamp: the commit alone collides when the same commit is
// rebuilt, which happens on every retry and every reopened pull request, and a
// timestamp alone sorts correctly but tells you nothing about what was built.
func logBuildID(sha string) string {
	return time.Now().UTC().Format("20060102-150405") + "-" + shortSHA(sha)
}

// Logs exposes the build log store, for the HTTP layer.
func (d *Daemon) Logs() *buildlog.Store { return d.logs }

// Builds returns a preview's recorded build attempts, newest first.
func (d *Daemon) Builds(ctx context.Context, previewID string) ([]store.Build, error) {
	return d.store.BuildsFor(ctx, previewID)
}

// Exposer exposes the publisher, for the HTTP layer to mount a path-serving
// implementation on its own listener.
func (d *Daemon) Exposer() expose.Exposer { return d.exposer }

// previewBaseURL is the baseUrl a preview must be built with.
//
// Under a host-per-preview exposer that is whatever the repository asked for.
// Under a path-mounting one the site lives beneath a prefix, and Docusaurus
// bakes baseUrl in at build time — so the prefix has to be folded in here,
// before the build, or the site returns its HTML and 404s every asset.
func (d *Daemon) previewBaseURL(name, repoBase string) string {
	pe, ok := d.exposer.(expose.PathExposer)
	if !ok {
		return repoBase
	}
	mount := pe.MountPath(name)
	if repoBase == "" || repoBase == "/" {
		return mount
	}
	return mount + strings.TrimPrefix(repoBase, "/")
}

// scrub removes injected secrets from text on its way to a pull request.
func (d *Daemon) scrub(s string) string { return d.currentBuilder().Redactor().Scrub(s) }

// currentBuilder reads the builder under the lock.
//
// It became necessary when the setup page gained the ability to change a secret
// at runtime: before that the field was written once during wiring and read
// forever after, so an unlocked read was safe. It is not any more.
func (d *Daemon) currentBuilder() *pipeline.Builder {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.builder
}

// SetClient installs the client for a platform, replacing any already there.
//
// Callable at runtime, because a GitHub client needs the App private key and
// the vault holding it may still be locked when the daemon starts. Building it
// during wiring meant the daemon refused to start until the vault was open —
// while the page that opens the vault lives inside that daemon.
//
// A build already in flight resolved its client when it started, so a client
// installed now takes effect from the next report onwards.
func (d *Daemon) SetClient(platform model.Platform, c scm.Client) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clients == nil {
		d.clients = map[model.Platform]scm.Client{}
	}
	d.clients[platform] = c
}

// client resolves the client for a platform under the lock.
func (d *Daemon) client(platform model.Platform) (scm.Client, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.clients[platform]
	return c, ok
}

// WithBuildSecrets injects environment variables into every build and redacts
// their values from build logs and error messages.
//
// Separate from New because the values come from the vault, which the daemon
// has no business opening for itself, and because a daemon with no secrets
// configured should not require the caller to pass an empty map.
func (d *Daemon) WithBuildSecrets(secrets map[string]string) *Daemon {
	if len(secrets) == 0 {
		return d
	}
	d.SetBuildSecrets(secrets)
	return d
}

// SetBuildSecrets replaces the injected secrets and rebuilds the redactor.
//
// Callable at runtime, because the setup page can change a secret while the
// daemon is serving. The redactor is compiled from the values, so a rotation
// that did not rebuild it would put the new value verbatim into the next build
// log — which is the one failure this whole subsystem exists to prevent.
//
// A build already running keeps the builder it started with. Its environment
// was constructed at launch and its redactor matches it, which is the
// consistent answer; the alternative is swapping a redactor out from under a
// stream that is mid-line.
func (d *Daemon) SetBuildSecrets(secrets map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.secrets = secrets
	if len(secrets) == 0 {
		d.builder = pipeline.NewBuilder(d.cfg.Build, d.log)
		return
	}
	d.builder = pipeline.NewBuilderWithSecrets(d.cfg.Build, secrets, d.log)
}

// commitLock returns the mutex serializing the publishing phase for a preview.
//
// Cancellation alone cannot protect the publish. Two things make that true.
// The zrok SDK's CreateShare takes no context, so a publish already in flight
// cannot be stopped. And publishing a name is *destructive* to whoever holds
// it: the exposer withdraws the existing share for that name first. So a
// superseded build that reaches Publish does not merely waste work, it tears
// down the newer preview and replaces it with older content.
//
// Checking "am I still current?" and publishing therefore have to be one
// atomic step. This lock makes them so, per preview, without holding the
// daemon-wide mutex across seconds of network I/O.
func (d *Daemon) commitLock(previewID string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	mu, ok := d.commit[previewID]
	if !ok {
		mu = &sync.Mutex{}
		d.commit[previewID] = mu
	}
	return mu
}

// isCurrent reports whether b is still the registered build for previewID.
func (d *Daemon) isCurrent(previewID string, b *build) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[previewID] == b
}

// Run starts the workers and the reaper and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Reported until recovery finishes, so a build queued in that window is explained
	// rather than looking stuck. No worker exists yet: reap-then-republish must complete
	// before anything may publish, which is the ordering that stops a restart deleting
	// what it just restored.
	d.starting.Store(true)
	// Measured from here, so the report covers what the operator actually waited through
	// — the reap, every republish, and the history load — rather than the republish loop
	// alone, which is the part that happens to be instrumented.
	bootStarted := time.Now()
	// Before recovery, because recovery republishes and every name it renders has to carry
	// the prefix an operator chose — not the config file's, which may be what they changed
	// it away from.
	d.loadNamePrefix(ctx)
	if err := d.recover(ctx); err != nil {
		d.starting.Store(false)
		return err
	}

	// Before any worker can add to the feed, so the restored history sits behind
	// this run's events rather than being interleaved with them.
	d.setStartup(StageHistory, "Loading recent build history.", 0, 0)
	d.backfill(ctx, eventLogSize)

	// The report, assembled before the flag flips.
	//
	// The page shows it when it first sees `starting: false`, so it has to be there in
	// the same payload that says so — stored afterwards, the first "started" status
	// carries no report and the moment to show it has passed.
	pending, err := d.store.PendingCount(ctx)
	if err != nil {
		// A count the report can do without. Failing startup over it would trade a
		// working daemon for a statistic.
		d.log.Warn("counting pending jobs for the startup report", "error", err)
	}
	d.lastStartup.Store(&StartupSummary{
		Seconds:         int(time.Since(bootStarted).Seconds()),
		Instance:        d.instance,
		Previews:        int(d.adoptedPreviews.Load() + d.createdPreviews.Load()),
		AdoptedPreviews: int(d.adoptedPreviews.Load()),
		CreatedPreviews: int(d.createdPreviews.Load()),
		AdoptedBuilds:   int(d.adoptedBuilds.Load()),
		CreatedBuilds:   int(d.createdBuilds.Load()),
		Orphans:         d.startupOrphans,
		Pending:         pending,
		Items:           d.startupItemsSnapshot(),
	})

	d.starting.Store(false)
	// Cleared after the flag, not before: the page reads one payload, and a status
	// assembled between the two would say "started" while still carrying a stage.
	d.startup.Store(nil)

	var wg sync.WaitGroup
	for i := 0; i < d.cfg.Workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			d.worker(ctx, n)
		}(i)
	}

	// Every project should have a preview of its default branch, so start the ones that do
	// not.
	//
	// After the workers exist, because this enqueues builds and nothing would claim them
	// otherwise; and in a goroutine, because it reaches the platform once per project and a
	// slow API must not hold up the daemon that is now otherwise running.
	//
	// This is the backfill for projects that predate branch previews. New projects get theirs
	// when they are created, so on a settled installation this finds nothing and costs one
	// database read.
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.backfillBranchPreviews(ctx)
		// After the branch previews, in the same goroutine rather than a second one: both
		// reach the platform once per project, and running them concurrently would double the
		// API calls a restart makes for no gain in wall-clock that anybody is waiting on.
		d.backfillOpenPullRequests(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.reaper(ctx)
	}()

	<-ctx.Done()
	wg.Wait()

	// Flush held-back reports before tearing anything down. A shutdown inside the
	// debounce window would otherwise lose the last report of every in-flight
	// build — usually the terminal one, leaving a comment reading "Building" for a
	// build that finished. Before the exposer closes, because a report can name a
	// URL and the exposer is what serves it.
	d.publisher.Close()

	if err := d.exposer.Close(); err != nil {
		d.log.Error("closing exposer", "error", err)
	}
	return nil
}

// backfillOpenPullRequests builds the open pull requests that have no preview.
//
// It exists because a delivery can be missed, and when it is, nothing notices. The daemon was
// stopped for a rebuild while somebody opened a pull request; the webhook was retried into a
// closed port; the tunnel was down for the minute that mattered. The pull request then sits with
// no comment and no preview, and the first sign of it is a person asking why their link never
// appeared — which is exactly how `customer-connect-docs#21` was found.
//
// Only pull requests with **no preview at all**. A preview that exists and failed is left alone,
// for the same reason a failed branch preview is: rebuilding it on every restart would hide a
// build that is broken behind one that keeps being retried, and the dashboard already shows it.
//
// Unlinked pull requests are skipped without this having to know: `handleBuild` is where that
// check lives, and this goes through it like everything else.
//
// One listing per project per startup, and it costs nothing on a settled installation — every
// pull request already has a preview, so the answer is "nothing to do" after one API call.
func (d *Daemon) backfillOpenPullRequests(ctx context.Context) {
	projects, err := d.store.ListProjects(ctx)
	if err != nil {
		d.log.Warn("listing projects to scan for unbuilt pull requests", "error", err)
		return
	}
	previews, err := d.store.ListPreviews(ctx)
	if err != nil {
		d.log.Warn("listing previews to scan for unbuilt pull requests", "error", err)
		return
	}

	// One pass over the previews rather than a query per pull request. Keyed by preview id
	// because that is what a pull request hashes to, and what a row is keyed on.
	have := map[string]bool{}
	for _, p := range previews {
		have[p.PreviewID] = true
	}

	for _, p := range projects {
		if !p.Enabled || ctx.Err() != nil {
			continue
		}
		repo := model.Repo{Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo}

		client, ok := d.client(repo.Platform)
		if !ok {
			continue
		}
		lister, ok := client.(scm.PullRequestLister)
		if !ok {
			continue
		}
		prs, err := lister.OpenPullRequests(ctx, repo)
		if err != nil {
			// Not fatal, and not even worth an error: a platform unreachable at startup is the
			// ordinary case for a daemon that boots before its network.
			d.log.Warn("could not list open pull requests while scanning",
				"project", repo.String(), "error", err)
			continue
		}

		for _, pr := range prs {
			if have[pr.PreviewID()] {
				continue
			}
			if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
				d.log.Warn("could not queue a pull request found with no preview",
					"pr", pr.String(), "error", err)
				continue
			}
			d.log.Info("queued a pull request that had no preview", "pr", pr.String(),
				"note", "its webhook delivery was missed, or it was opened while the daemon was down")
		}
	}
}

// backfillBranchPreviews gives every project a preview of its default branch.
//
// The promise a branch preview makes is that the URL always answers, and a promise that only
// applies to projects added after the feature shipped is not one. So this runs at every
// startup and builds what is missing.
//
// It is deliberately not a repair of anything else. A project whose branch preview exists but
// failed is left alone: it has a row, somebody can see the failure on its card, and rebuilding
// it on every restart would hide a build that is broken behind one that keeps being retried.
// Only the absence of a preview is treated as something to fix.
//
// Errors are logged per project and never fatal. A platform that cannot be reached at startup
// is the ordinary case for a daemon that boots before its network, and the next restart — or
// the button on the card — tries again.
func (d *Daemon) backfillBranchPreviews(ctx context.Context) {
	projects, err := d.store.ListProjects(ctx)
	if err != nil {
		d.log.Warn("listing projects to backfill branch previews", "error", err)
		return
	}
	previews, err := d.store.ListPreviews(ctx)
	if err != nil {
		d.log.Warn("listing previews to backfill branch previews", "error", err)
		return
	}

	// One pass over the previews rather than a lookup per project: this runs at every
	// startup, and a query per project on an installation with fifty of them is fifty
	// queries to answer one question.
	has := map[model.Repo]bool{}
	for _, p := range previews {
		if p.PR.IsBranch() {
			has[p.PR.Repo] = true
		}
	}

	for _, p := range projects {
		if !p.Enabled {
			continue
		}
		repo := model.Repo{Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo}
		if has[repo] {
			continue
		}
		// Cancellation means the daemon is shutting down, not that this project is fine.
		if ctx.Err() != nil {
			return
		}
		branch, err := d.BuildBranch(ctx, repo, "")
		if err != nil {
			d.log.Warn("could not start a branch preview for a project that has none",
				"project", repo.String(), "error", err)
			continue
		}
		d.log.Info("backfilled a branch preview", "project", repo.String(), "branch", branch)
	}
}

// recover reconciles the world with the database at startup.
//
// Nothing is serving any preview yet — the process just started — so every
// remote share the exposer owns is an orphan, and every artifact directory on
// disk is either about to be republished or dead. The cheap and correct answer
// is to reap everything remote, then rebuild each recorded preview from its
// artifacts on disk. That restores working preview URLs within a second or two
// of startup without re-cloning or re-running a single npm install.
func (d *Daemon) recover(ctx context.Context) error {
	previews, err := d.store.ListPreviews(ctx)
	if err != nil {
		return err
	}

	// Republished concurrently, bounded, and only after the reap below has returned.
	//
	// Each publish is a share creation plus an overlay listener — ten to fifteen seconds
	// against the hosted zrok controller — and no worker starts until every one of them is
	// done, so serially this was minutes of a daemon that looked started and would not build
	// anything. A queued build in that window is indistinguishable from a stuck queue, which
	// is how it was reported twice.
	//
	// Safe to parallelise, for reasons specific to this loop rather than in general:
	//
	//   - Each preview publishes under its own name, and two previews sharing a name is
	//     already refused by the exposer rather than resolved.
	//   - Publishing is destructive to whoever holds the name. What holds these names now
	//     is either nothing — the reap deleted it — or the share about to be adopted under
	//     that same key, which is not a collision. That is why the reap still runs first,
	//     rather than being merged into this loop.
	//   - The stores of live publications are behind d.mu in every exposer, and the daemon's
	//     own maps are written under it here.
	//
	// Bounded by workers, with a floor: a daemon configured for one worker still has no
	// reason to restore one preview at a time, and unbounded would open a listener per
	// preview at once against a controller that rate-limits.
	limit := max(d.cfg.Workers, 4)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		restored int
		builds   int
		sem      = make(chan struct{}, limit)
	)
	// Eligibility is decided before anything is launched, so the progress banner has a
	// denominator. Counting `previews` instead would show "1 of 9" on a database where
	// eight rows are failed builds with no artifacts to restore, and a total that never
	// arrives reads as a stall.
	eligible := make([]store.Preview, 0, len(previews))
	for _, p := range previews {
		if p.State != scm.StateReady || p.ArtifactDir == "" {
			continue
		}
		if _, err := os.Stat(p.ArtifactDir); err != nil {
			d.log.Info("dropping preview with missing artifacts", "preview", p.PreviewID, "dir", p.ArtifactDir)
			if err := d.store.DeletePreview(ctx, p.PreviewID); err != nil {
				d.log.Error("forgetting preview", "preview", p.PreviewID, "error", err)
			}
			continue
		}
		eligible = append(eligible, p)
	}

	// The denominator is round trips, not previews. Each preview is one share plus one
	// per retained build, and the build shares are the bulk of it — restoring two pull
	// requests is eleven calls. Counting previews gave a bar that read "1 of 2" and did
	// not move for minutes.
	//
	// Costs one extra sqlite query per preview, which is local and immediate against the
	// ten-to-fifteen seconds each of the calls it is counting takes.
	d.startupDone.Store(0)
	d.startupTotal = len(eligible)
	keep := make(map[string]bool, len(eligible))
	for _, p := range eligible {
		keep[p.PreviewID] = true
		for _, b := range d.buildSharesToRestore(ctx, p) {
			keep[buildKey(p.PreviewID, b.BuildID)] = true
			d.startupTotal++
		}
	}

	// What the exposer already has, before anything is deleted.
	//
	// This is the whole of the adoption strategy: a share survives the process that
	// created it, only its overlay listener does not. Listing first means the reap can be
	// told what to keep, and each restore can bind to a share that is already there
	// instead of paying to delete it and paying again to create an identical one — which
	// was measured at 85 seconds of deleting followed by 183 seconds of creating, for two
	// pull requests.
	d.setStartup(StageReaping, "Checking what the exposer already has.", 0, 0)
	d.adoptable = nil
	if ad, ok := d.exposer.(expose.Adopter); ok {
		found, err := ad.Adoptable(ctx)
		if err != nil {
			// Not fatal, and deliberately not retried here: without a listing every
			// publication is created from scratch, which is exactly the old behaviour.
			d.log.Warn("listing shares to adopt; falling back to recreating them", "error", err)
		} else {
			d.adoptable = found
			d.log.Info("shares available to adopt", "count", len(found))
		}
	}

	// Reap, now with a keep-set rather than nil.
	//
	// nil meant "delete everything you own", on the reasoning that a share cannot outlive
	// its process. The share does outlive it; the listener does not. So what must be
	// deleted is only what the database no longer claims — a preview since torn down, a
	// build since pruned, a share left by a create that timed out after succeeding.
	var orphans int
	for key := range d.adoptable {
		if !keep[key] {
			orphans++
		}
	}
	// Recorded for the report. Counted from our own listing rather than from Reap, which
	// returns an error and not a tally — so this is "what we could see that nothing
	// claims", which is the same set Reap is about to delete.
	d.startupOrphans = orphans
	d.setStartup(StageReaping, "Clearing shares the database no longer claims.", 0, 0)
	if orphans > 0 {
		d.addStartupItem(fmt.Sprintf("clearing %d share(s) nothing claims", orphans))
	}
	if err := d.exposer.Reap(ctx, keep); err != nil {
		d.log.Warn("reaping orphaned shares at startup", "error", err)
	}

	d.setStartup(StageRestoring, restoringNote, 0, d.startupTotal)

	for _, p := range eligible {
		wg.Add(1)
		go func(p store.Preview) {
			defer wg.Done()
			// The preview's own share is one counted round trip, and it is counted
			// whether it worked. A failure consumed the wait just the same, and the
			// alternative — counting successes — leaves the bar short of its total
			// exactly when something went wrong, which is when it is being read.
			defer d.bumpStartup()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := d.republish(ctx, p); err != nil {
				// Artifacts that cannot be served are the same situation as artifacts
				// that are not there: drop the row so the next push rebuilds. Keeping
				// it means this error on every startup forever, and a preview whose
				// comment advertises a URL that serves a broken page.
				if errors.Is(err, errArtifactsUnusable) {
					d.log.Warn("dropping preview whose artifacts cannot be served",
						"preview", p.PreviewID, "error", err)
					if err := d.store.DeletePreview(ctx, p.PreviewID); err != nil {
						d.log.Error("forgetting preview", "preview", p.PreviewID, "error", err)
					}
					return
				}
				// The row still says `ready` and still carries the URL the last
				// successful build published, and nothing is bound to that address
				// now — so the dashboard offers a link that 502s and the pull
				// request comment advertises the same one. Both read this row.
				d.log.Error("restoring preview", "preview", p.PreviewID, "error", err)
				d.markUnpublished(ctx, p, err)
				return
			}

			n := d.restoreBuildShares(ctx, p)
			mu.Lock()
			restored++
			builds += n
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	pending, err := d.store.PendingCount(ctx)
	if err != nil {
		return err
	}
	d.log.Info("recovered",
		"previews_restored", restored, "build_shares_restored", builds, "jobs_pending", pending)

	if pending > 0 {
		d.nudge()
	}
	return nil
}

// buildSharesToRestore is the build shares one preview expects to have published: a
// recorded URL, and artifacts still on disk to serve.
//
// Used for two things before any of them is published — the progress denominator, and
// the reap's keep-set. Both have to agree with what restoreBuildShares will actually
// attempt: a total that includes work nobody does leaves the bar short of the end every
// time, and a keep-set that misses one deletes a share that is about to be adopted.
//
// Side-effect free, which is why restoreBuildShares still runs its own loop rather than
// consuming this: that loop also *clears* the URL of a build whose artifacts have been
// pruned, and doing that from a function called to count things would mean the keep-set
// pass silently rewrote rows.
//
// A failed query is an empty list rather than an error. Wrong by a share makes the bar
// finish early; failing recovery over it restores nothing at all.
func (d *Daemon) buildSharesToRestore(ctx context.Context, p store.Preview) []store.Build {
	rows, err := d.store.BuildsFor(ctx, p.PreviewID)
	if err != nil {
		d.log.Warn("listing builds to count their shares", "preview", p.PreviewID, "error", err)
		return nil
	}
	out := make([]store.Build, 0, len(rows))
	for _, b := range rows {
		if b.URL == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(d.cfg.ArtifactsDir(), p.PreviewID, b.BuildID)); err != nil {
			continue
		}
		out = append(out, b)
	}
	return out
}

// publishOrAdopt serves h for spec, taking over an existing publication when the
// exposer has one under the same key.
//
// Reports whether it adopted, because that is the difference between a restart costing
// one overlay bind per preview and costing a delete plus a create against a controller
// that answers in ten to fifteen seconds.
//
// A failed adoption falls through to publishing. The share it could not bind to is left
// alone deliberately — Publish replaces whatever holds the name, so the fallback is also
// the cleanup.
func (d *Daemon) publishOrAdopt(ctx context.Context, spec expose.Spec, h http.Handler) (*expose.Publication, bool, error) {
	ad, ok := d.exposer.(expose.Adopter)
	if a, found := d.adoptable[spec.Key()]; ok && found {
		pub, err := ad.Adopt(ctx, spec, a, h)
		if err == nil {
			return pub, true, nil
		}
		d.log.Warn("adopting an existing share; creating a new one instead",
			"publication", spec.Key(), "name", spec.Name, "error", err)
	}
	pub, err := d.exposer.Publish(ctx, spec, h)
	return pub, false, err
}

// addStartupItem records one thing recovery did, for the banner and the report.
//
// Bounded, oldest dropped. It is a running commentary on a wait, not a log — the log
// already has every line of it, and an unbounded slice on a daemon restoring a hundred
// previews would be shipped in full to every /status.
func (d *Daemon) addStartupItem(text string) {
	const maxItems = 40

	d.itemsMu.Lock()
	d.startupItems = append(d.startupItems, text)
	if len(d.startupItems) > maxItems {
		d.startupItems = d.startupItems[len(d.startupItems)-maxItems:]
	}
	d.itemsMu.Unlock()
}

// startupItemsSnapshot copies the activity list for a payload.
//
// A copy, because the caller marshals it after the lock is released and recovery is
// still appending — handing out the slice itself is a data race that shows up as a
// truncated JSON body under load.
func (d *Daemon) startupItemsSnapshot() []string {
	d.itemsMu.Lock()
	defer d.itemsMu.Unlock()
	if len(d.startupItems) == 0 {
		return nil
	}
	return slices.Clone(d.startupItems)
}

// restoreBuildShares republishes one preview's per-build shares, and forgets the
// ones it cannot.
//
// Without this a build share lasted exactly as long as the process that made it.
// Startup reaps with an empty keep-set — nothing we own can have survived — and then
// republished previews only, so every build URL 404'd after a restart while
// `builds.url` went on advertising it. Reported as "that should still be there", and
// it should have been.
//
// A row whose artifacts are gone has its URL cleared rather than being republished,
// because `keep_builds` prunes artifacts and the row outlives them. Leaving the URL
// would keep the dashboard offering a link to something no longer on disk, which is
// the same failure in slower motion.
//
// Best effort per build, like the original publish: the branch share is already back
// up by the time this runs, and one build URL missing is worth a warning rather than
// a startup that gives up part-way through.
func (d *Daemon) restoreBuildShares(ctx context.Context, p store.Preview) int {
	rows, err := d.store.BuildsFor(ctx, p.PreviewID)
	if err != nil {
		d.log.Warn("listing builds to restore their shares", "preview", p.PreviewID, "error", err)
		return 0
	}

	// Concurrent, bounded, for the same reason recover's own loop is: this is where the
	// count multiplies. One preview can hold keep_builds shares — ten by default — and at
	// ten to fifteen seconds per publish that is minutes for a single pull request, during
	// which no build can start.
	//
	// Each build publishes under its own name (the branch name plus its short sha), so
	// nothing here contends with anything else. Both maps written below are behind d.mu.
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		n   int
		sem = make(chan struct{}, max(d.cfg.Workers, 4))
	)
	for _, b := range rows {
		if b.URL == "" {
			continue
		}

		dir := filepath.Join(d.cfg.ArtifactsDir(), p.PreviewID, b.BuildID)
		if _, err := os.Stat(dir); err != nil {
			// Pruned by keep_builds, or never written. Forget the URL so nothing
			// offers it; the row itself stays, because the history is still true.
			if err := d.store.ClearBuildShare(ctx, p.PreviewID, b.BuildID); err != nil {
				d.log.Warn("forgetting a pruned build's URL",
					"preview", p.PreviewID, "build", b.BuildID, "error", err)
			}
			continue
		}

		wg.Add(1)
		go func(b store.Build) {
			defer wg.Done()
			// Counted on the way out, whatever happened. A share that fails to publish
			// still consumed the round trip the operator is waiting on, and a progress
			// bar that only counts successes stops short of its own total and reads as
			// a stall at the very end.
			defer d.bumpStartup()
			sem <- struct{}{}
			defer func() { <-sem }()

			site, err := preview.New(dir, p.BaseURL)
			if err != nil {
				d.log.Warn("serving a build's artifacts", "build", b.BuildID, "error", err)
				return
			}

			pub, adopted, err := d.publishOrAdopt(ctx, expose.Spec{
				PreviewID: p.PreviewID,
				BuildID:   b.BuildID,
				Name:      b.Name,
				BaseURL:   p.BaseURL,
				PR:        p.PR,
			}, site)
			if err != nil {
				d.log.Warn("restoring a build share", "build", b.BuildID, "name", b.Name, "error", err)
				d.addStartupItem("failed " + b.Name)
				return
			}
			if adopted {
				d.adoptedBuilds.Add(1)
				d.addStartupItem("adopted " + b.Name)
			} else {
				d.createdBuilds.Add(1)
				d.addStartupItem("created " + b.Name)
			}

			d.mu.Lock()
			d.liveBuilds[buildKey(p.PreviewID, b.BuildID)] = pub
			d.mu.Unlock()

			mu.Lock()
			n++
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	return n
}

// applyProject overlays the operator's project settings onto the repository's own.
//
// Field by field, and only where the project states one. An empty project field
// means "no opinion", which keeps a row that merely names a repository useful —
// the common case, where the repository already configures itself correctly and
// the row exists to say the repository is allowed at all.
//
// A missing project row changes nothing. Requiring one to build would break every
// repository the App is already installed on, which is a decision for whoever
// turns that requirement on rather than a side effect of adding this.
//
// Failure is a warning, not an error. The alternative is refusing to build because
// a lookup failed, which trades a build for nothing.
func (d *Daemon) applyProject(ctx context.Context, pr model.PullRequest, cfg config.RepoConfig) config.RepoConfig {
	p, err := d.store.ProjectFor(ctx, string(pr.Repo.Platform), pr.Repo.Owner, pr.Repo.Name)
	if err != nil {
		if !errors.Is(err, store.ErrNoProject) {
			d.log.Warn("reading the project settings", "repo", pr.Repo.String(), "error", err)
		}
		return cfg
	}

	// The framework preset, before the project's own fields overlay it.
	//
	// Precedence, top down: the project's explicit field, then its preset, then the
	// repository's .docpreview.yml. The middle one beating the last is the surprising half
	// and is deliberate — the repository's file says what it says, and an operator who
	// chose "Docusaurus" in the dashboard has said something more recent and more specific.
	// Deferring to the repository is what the blank preset is for, and it is the default,
	// so a project nobody has touched behaves exactly as before.
	//
	// An unknown id is ignored rather than refused: it means this binary does not have a
	// preset the row names — a downgrade, or an entry since removed — and falling back to
	// the repository's own configuration is better than failing the build.
	if f, ok := config.FrameworkByID(p.Framework); ok {
		if f.Dir != "" {
			cfg.Build.Dir = f.Dir
		}
		if f.BuildCommand != "" {
			cfg.Build.Command = f.BuildCommand
		}
		if f.Output != "" {
			cfg.Build.Output = f.Output
		}
	}

	if p.BuildDir != "" {
		cfg.Build.Dir = p.BuildDir
	}
	if p.BuildCommand != "" {
		cfg.Build.Command = p.BuildCommand
	}
	if p.BuildOutput != "" {
		cfg.Build.Output = p.BuildOutput
	}
	if p.BaseURL != "" {
		cfg.Build.BaseURL = p.BaseURL
	}
	if p.DetectScript != "" {
		cfg.Detect.Script = p.DetectScript
	}
	// Parsed here rather than stored parsed, and a bad value is dropped rather than
	// failing the build. The projects page refuses an unparseable duration on save, so
	// reaching this with one means a row edited by hand in sqlite — and the server
	// default is a better answer to that than no build at all.
	if p.Timeout != "" {
		if dur, err := time.ParseDuration(p.Timeout); err == nil && dur > 0 {
			cfg.Build.Timeout = dur
		} else {
			d.log.Warn("ignoring an unusable project build timeout",
				"repo", pr.Repo.String(), "timeout", p.Timeout,
				"fix", "set it to a duration like 45m on the projects page")
		}
	}
	return cfg
}

// driverAllowed refuses the local driver unless the operator enabled it in writing.
//
// The refusal is here, at the last moment before a build, rather than at startup or in
// config validation, because a project row can name the driver too. Validating only the
// server config would let a project opt itself into running branch code on the host,
// which is precisely the decision this is trying to keep in one place.
//
// A failed build with this message is a better outcome than a successful one that ran
// the branch's postinstall scripts as the daemon's user. The message names the key,
// because the operator's next question is what to do about it.
func (d *Daemon) driverAllowed(driver string) error {
	if driver != config.DriverLocal || d.cfg.Build.AllowLocalDriver {
		return nil
	}
	return fmt.Errorf("the local build driver is not enabled: it runs this pull request's "+
		"own build scripts on this host as the daemon's user, so it is off unless asked for. "+
		"Install docker and leave build.driver at %q, or accept that and set "+
		"build.allow_local_driver: true in the server config",
		config.DriverDocker)
}

// confineToDriver drops the parts of a repository's own config that would execute
// branch-authored code on this host.
//
// # What this does and does not buy
//
// Under `docker` it changes nothing: the container is the boundary, and honouring the
// repository's build command there is the intended design.
//
// Under `local` it clears two fields that arrived in the pull request —
// `build.command` and `detect.script`. `www/docs/reference/repo-config.md` promised
// exactly this for the command and **nothing implemented it**: `buildLocal` ran the
// value through `cmd /c` on the host, so anyone who could push a branch to a watched
// repository could run a command on the machine holding the GitHub App private key and
// every project's tokens. The detect script was worse, because it ran on the host under
// *either* driver, before any of this was consulted.
//
// **It does not make the local driver safe, and nothing can.** That driver runs
// `npm install` and `npm run build` in the branch's own tree: every dependency's
// postinstall script and whatever `scripts.build` says are branch-authored code
// executing as the daemon's user, by design, before this function is reachable. The
// only real isolation is the container, which is why the default driver is `docker`
// and why the fallback to `local` is logged as loudly as it is. What this closes is the
// narrower channel — a repository with no dependencies at all, running an arbitrary
// command line — and it makes the documented promise true.
//
// A project-supplied command still runs. That value is the operator's, cannot be
// edited by a contributor, and is the whole reason a project row outranks the branch.
func (d *Daemon) confineToDriver(
	ctx context.Context, pr model.PullRequest, cfg config.RepoConfig, driver string,
) config.RepoConfig {
	if driver == config.DriverDocker {
		return cfg
	}

	// What the operator stated, which survives. A lookup failure is treated as "no
	// project", which is the conservative direction: it drops more, not less.
	var project store.Project
	if p, err := d.store.ProjectFor(ctx,
		string(pr.Repo.Platform), pr.Repo.Owner, pr.Repo.Name); err == nil {
		project = p
	}

	if project.BuildCommand == "" && cfg.Build.Command != config.DefaultBuildCommand {
		d.log.Warn("ignoring the build command from this branch because the local driver "+
			"would run it on this host; set it on the project instead",
			"pr", pr.String(), "driver", driver)
		cfg.Build.Command = config.DefaultBuildCommand
	}
	if project.DetectScript == "" && cfg.Detect.Script != "" {
		d.log.Warn("ignoring the detect script from this branch because the local driver "+
			"would run it on this host; path matching decides instead",
			"pr", pr.String(), "script", cfg.Detect.Script)
		cfg.Detect.Script = ""
	}
	return cfg
}

// projectDriver returns the build driver and image for a pull request.
//
// Per-project, falling back to the server default. A project that runs a command
// somebody outside your trust boundary can influence wants docker; the repository
// whose contributors you all know wants local and its two-second builds. Making it
// one server-wide setting forces the stricter answer on everything or the looser
// answer on everything.
func (d *Daemon) projectDriver(ctx context.Context, pr model.PullRequest) (driver, image string) {
	driver, image = d.cfg.Build.Driver, d.cfg.Build.Image

	p, err := d.store.ProjectFor(ctx, string(pr.Repo.Platform), pr.Repo.Owner, pr.Repo.Name)
	if err != nil {
		return driver, image
	}
	if p.Driver != "" {
		driver = p.Driver
	}
	if p.Image != "" {
		image = p.Image
	}
	return driver, image
}

// SetProjectSecrets installs the resolver for a project's own environment
// variables.
//
// A function rather than a map, and consulted at the start of each build rather than
// cached, for the same reason expose.TokenFunc is a function: the values live in a
// vault that may be locked when the daemon starts, and the page that unlocks it is
// served by this daemon. It also means a token added from that page applies to the
// next build without a rearm callback or a restart — which matters because the
// operator adding it is usually looking at the build that just failed without it.
//
// A daemon with no resolver installed behaves exactly as before: global build
// secrets only.
func (d *Daemon) SetProjectSecrets(fn func(platform, owner, repo string) map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.projectSecretsFn = fn
}

// projectSecrets resolves one project's environment variables, empty for a project
// that has none or a vault that is locked.
//
// Read once, at the start of a build, and handed to Builder.WithSecrets — which
// rebuilds the redactor from the values. Those two steps must stay together: a
// secret injected without being added to the redactor is a credential one `set -x`
// away from a public pull request comment.
func (d *Daemon) projectSecrets(pr model.PullRequest) map[string]string {
	d.mu.Lock()
	fn := d.projectSecretsFn
	d.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(string(pr.Repo.Platform), pr.Repo.Owner, pr.Repo.Name)
}

// saveBuild records a build attempt, logging rather than propagating a failure.
//
// Best-effort on purpose. The history and the build picker are worth having, and
// neither is worth failing a build that otherwise succeeded — the same reasoning
// that makes reporting best-effort.
//
// Its own context, because this is called from paths whose context may already be
// cancelled by a supersede, and a superseded build's outcome is exactly the thing
// the history should record.
func (d *Daemon) saveBuild(b store.Build) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.store.SaveBuild(ctx, b); err != nil {
		d.log.Warn("recording the build", "preview", b.PreviewID, "build", b.BuildID, "error", err)
	}
}

// errArtifactsUnusable marks a republish failure that rebuilding would fix, so
// recovery drops the row instead of retrying it on every startup.
var errArtifactsUnusable = errors.New("stored artifacts cannot be served")

// reportRank orders the states a single commit moves through.
//
// A build goes queued → building → one of ready, failed or skipped, and never
// backwards. That is what makes a stale report recognisable: any state ranked
// below the furthest one already reported for the same commit is late, whatever
// its timestamp says.
//
// A timestamp cannot do this job. It records when a report was *made*, and the
// inversion being defended against is in the making — two goroutines producing
// reports for one preview, where the earlier lifecycle state is sometimes the
// later message. Sorting by time faithfully reproduces the wrong order.
func reportRank(s scm.State) int {
	switch s {
	case scm.StateQueued:
		return 1
	case scm.StateBuilding:
		return 2
	case scm.StateReady, scm.StateFailed, scm.StateSkipped:
		return 3
	default:
		// Anything unrecognised — a teardown, a state added later — is terminal
		// rather than stale. Silently dropping a state this function has not been
		// taught about would be the worse failure.
		return 4
	}
}

// reportMark is the furthest state reported for one commit.
type reportMark struct {
	commit string
	rank   int
}

// staleReport reports whether r describes a state already passed for its commit,
// and records it when it does not.
//
// Keyed by commit, because the lifecycle restarts with every push: ready → queued
// is backwards within one commit and correct across two. A commit that differs
// from the mark resets it rather than comparing against it.
//
// Equal ranks pass. Two ready reports for one commit are legitimate — a republish
// after a restart can move the URL, and the comment has to be rewritten — so the
// test is strictly-below, not below-or-equal.
func (d *Daemon) staleReport(r scm.Report) bool {
	rank := reportRank(r.State)

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.reported == nil {
		d.reported = map[string]reportMark{}
	}
	mark, seen := d.reported[r.PreviewID]

	if seen && mark.commit == r.Commit && rank < mark.rank {
		return true
	}
	d.reported[r.PreviewID] = reportMark{commit: r.Commit, rank: rank}
	return false
}

// republish serves a preview's existing artifacts at its existing name.
//
// The stored base URL is the value the artifacts were baked with, which is also
// the path they must be served at — one value, used twice, and that is the
// invariant. It can still be wrong by the time this runs: a path-mounting exposer
// folds its mount prefix into the base URL and a host-per-preview one does not, so
// changing `exposer.kind` leaves every stored row describing a layout the current
// exposer will not produce.
//
// Serving anyway yields a site whose HTML loads and whose every asset 404s, with
// nothing in any log to explain it. So the artifacts are checked against the value
// before they are published, and a mismatch is an error — which drops the row, the
// same as a missing artifact directory, and lets the next push rebuild.
func (d *Daemon) republish(ctx context.Context, p store.Preview) error {
	if err := pipeline.VerifyBaseURL(p.ArtifactDir, p.BaseURL); err != nil {
		return fmt.Errorf("%w: stored artifacts do not match the base URL they would be served at "+
			"(most likely exposer.kind changed since this preview was built): %w",
			errArtifactsUnusable, err)
	}

	site, err := preview.New(p.ArtifactDir, p.BaseURL)
	if err != nil {
		return err
	}

	pub, adopted, err := d.publishOrAdopt(ctx, expose.Spec{
		PreviewID: p.PreviewID,
		Name:      p.Name,
		BaseURL:   p.BaseURL,
		PR:        p.PR,
	}, site)
	if err != nil {
		return err
	}
	if adopted {
		d.adoptedPreviews.Add(1)
		d.addStartupItem(fmt.Sprintf("adopted %s (#%d %s)", p.Name, p.PR.Number, p.PR.Branch))
	} else {
		d.createdPreviews.Add(1)
		d.addStartupItem(fmt.Sprintf("created %s (#%d %s)", p.Name, p.PR.Number, p.PR.Branch))
	}

	d.mu.Lock()
	if old := d.live[p.PreviewID]; old != nil {
		old.Close()
	}
	d.live[p.PreviewID] = pub
	d.mu.Unlock()

	// The URL can move if the exposer had to disambiguate the name, and a
	// comment pointing at a dead URL is worse than no comment.
	if pub.URL != p.URL {
		p.URL = pub.URL
		p.UpdatedAt = time.Now()
		if err := d.store.SavePreview(ctx, p); err != nil {
			return err
		}
		d.report(ctx, scm.Report{
			PR: p.PR, PreviewID: p.PreviewID, State: scm.StateReady,
			URL: pub.URL, Name: pub.Name, Commit: p.Commit, UpdatedAt: p.UpdatedAt,
		})
	}
	return nil
}

// markUnpublished records that a preview recovery could not restore is not being served.
//
// Seen live on 2026-07-30: a preview that had built perfectly failed to restore when
// `CreateShare` timed out, and `/status` went on reporting `state: ready` with a URL that
// answered 502 for the rest of the day. `restoreBuildShares` already forgets a build's URL
// when it cannot republish it, for exactly this reason; the preview path did not.
//
// Three things, and each closes one of the three surfaces that were lying:
//
//   - The row loses its URL and gains `failed` plus a reason. The dashboard's Open button
//     is decided by the presence of a URL rather than by the state — a rebuild does not
//     withdraw the live share, so a state test would grey the button on every rebuild —
//     which means clearing the URL is the only thing that greys it here.
//   - A failed report, so the comment stops offering the link. Nothing else would send
//     one: the row is not rebuilt until somebody pushes or presses Rebuild, and until then
//     the comment is the copy of this state that a reviewer is actually looking at.
//   - A banner line, matching the "failed <name>" a build share leaves, so the operator
//     watching startup sees which preview did not come back.
//
// Not a deletion, which is what the artifact failures a few lines above get. Those cannot
// be served at all; this one can — the artifacts are on disk and match their base URL, and
// the failure is one controller round trip that the next attempt may well win. Deleting
// the row would take the preview off the page that holds the Rebuild button while its
// comment still pointed at the dead URL.
//
// Best effort, and deliberately not fatal to recovery: the preview it describes is already
// not being served, and a startup that gives up here restores nothing after it.
func (d *Daemon) markUnpublished(ctx context.Context, p store.Preview, cause error) {
	// Scrubbed and one line, as every other reason written to a row is: this is rendered
	// on a dashboard that can be shared read-only, and a publish failure's error text
	// carries hostnames and API detail.
	reason := d.scrub(firstLine(cause.Error())) + " — nothing is served at the previous " +
		"URL; press Rebuild to build this preview again"

	if err := d.store.FailPreview(ctx, p.PreviewID, reason); err != nil {
		d.log.Error("recording that a preview could not be republished; its row still "+
			"advertises a URL that is not served", "preview", p.PreviewID, "error", err)
	}
	d.addStartupItem("failed " + p.Name)

	// The commit is the one on the row, so this report is ranked against the `ready` the
	// build published — a fresh process has no mark for it, so nothing drops it, and a
	// terminal state ties rather than losing. See staleReport.
	d.report(ctx, scm.Report{
		PR: p.PR, PreviewID: p.PreviewID, State: scm.StateFailed,
		Reason: reason, Commit: p.Commit, UpdatedAt: time.Now(),
	})
}

// Handle processes a normalized webhook event.
func (d *Daemon) Handle(ctx context.Context, ev scm.Event) error {
	switch ev.Kind {
	case scm.EventBuild:
		return d.handleBuild(ctx, ev)
	case scm.EventTeardown:
		return d.handleTeardown(ctx, ev)
	default:
		return fmt.Errorf("unknown event kind %q", ev.Kind)
	}
}

// CancelBuild abandons the build running for a preview, and reports whether there was one.
//
// The machinery already existed for supersede — a push abandons the build the previous
// push started — and nothing exposed it. So a build wedged on a slow registry, or one
// somebody started against the wrong image, held a worker until the fifteen-minute timeout
// with the only remedy being to restart the daemon.
//
// The entry is cleared, not merely cancelled, exactly as supersede does it: clearing is
// what makes the abandoned build fail its own isCurrent check and decline to publish. A
// build cancelled while still registered would go on to take the name.
//
// Queued but not started is a different thing and is not this: there is no context to
// cancel, and the job stays in the queue. The caller is told so rather than being told
// nothing happened.
func (d *Daemon) CancelBuild(ctx context.Context, previewID string) bool {
	d.mu.Lock()
	b := d.running[previewID]
	if b != nil {
		delete(d.running, previewID)
	}
	d.mu.Unlock()

	if b == nil {
		// Nothing running. A queued build has no context to cancel, so stopping it means
		// taking it out of the queue before a worker claims it.
		//
		// Checked second, and that order is the race: a worker can claim the job between
		// the page being drawn and the click arriving. Looking at `running` first means a
		// job that has just started is cancelled properly rather than being reported as
		// "already gone" because the queue row had disappeared.
		// Read before deleting: the pull request lives in the queue row, and a comment
		// left reading "Queued" for a build that will never run is the abandoned-comment
		// failure in another form.
		var queuedPR model.PullRequest
		if jobs, err := d.store.PendingJobs(ctx); err == nil {
			for _, j := range jobs {
				if j.PR.PreviewID() == previewID {
					queuedPR = j.PR
					break
				}
			}
		}

		removed, err := d.store.Dequeue(ctx, previewID)
		if err != nil {
			d.log.Warn("dequeueing a build", "preview", previewID, "error", err)
			return false
		}
		if !removed {
			return false
		}
		d.log.Info("removed a queued build on request", "preview", previewID)

		if queuedPR.Repo.Name != "" {
			d.report(ctx, scm.Report{
				PR: queuedPR, PreviewID: previewID, State: scm.StateFailed,
				Commit: queuedPR.HeadSHA, Reason: "cancelled from the dashboard before it started",
				UpdatedAt: time.Now(),
			})
			d.recordf(queuedPR, "failed", "queued build cancelled from the dashboard")
		}
		return true
	}
	d.log.Info("cancelling a build on request", "pr", b.pr.String(), "sha", b.pr.HeadSHA)
	b.cancel()

	// Reported as failed rather than left as building. The pull request comment says
	// "Building" until something says otherwise, and a cancelled build that never reports
	// leaves it saying that forever.
	d.report(ctx, scm.Report{
		PR: b.pr, PreviewID: previewID, State: scm.StateFailed,
		Commit: b.pr.HeadSHA, Reason: "cancelled from the dashboard", UpdatedAt: time.Now(),
	})
	d.recordf(b.pr, "failed", "build cancelled from the dashboard")
	return true
}

// Expecting reports whether a build is running or queued for a preview.
//
// It exists for the log stream, which has to decide between two right answers. A preview
// with nothing coming should replay its last log and close, so a tab opening yesterday's
// failure is not holding a connection open for a build that will never happen. A preview
// with a build queued should keep the connection and announce that build when it starts,
// because the alternative is what Rebuild used to do: connect, learn "nothing is running",
// and have no way to find out that changed.
//
// Queued as well as running, and that is the case it was written for: Rebuild enqueues, and
// a worker claims the job a second or two later.
func (d *Daemon) Expecting(ctx context.Context, previewID string) bool {
	d.mu.Lock()
	_, running := d.running[previewID]
	d.mu.Unlock()
	if running {
		return true
	}

	jobs, err := d.store.PendingJobs(ctx)
	if err != nil {
		// Unknown, so assume not: holding a connection open on a failed lookup is the
		// worse of the two mistakes.
		return false
	}
	for _, j := range jobs {
		if j.PR.PreviewID() == previewID {
			return true
		}
	}
	return false
}

// RebuildPreview queues a build of a preview's recorded commit again.
//
// The commit that is already on the row, not whatever the branch has moved to since. "Build
// this again" and "build what is on the branch now" are different requests, and the second
// is what a push already does — so this one is for the case a push cannot fix: a build that
// failed on a bad cache entry, a timeout, an image that has since been corrected, or a
// project setting that changed under a preview nobody is pushing to.
//
// It goes through handleBuild, so it supersedes anything already running for the preview
// and reports the same states in the same order as a webhook would. A rebuild is not a
// second kind of build.
//
// Reports false when the preview is unknown, which includes one torn down between the page
// being drawn and the button being pressed.
func (d *Daemon) RebuildPreview(ctx context.Context, previewID string) (bool, error) {
	previews, err := d.store.ListPreviews(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range previews {
		if p.PreviewID != previewID {
			continue
		}
		pr := p.PR

		// A branch preview rebuilds at the branch's *current* tip, not the commit on the
		// row — the opposite of the rule below, and for the same reason it exists.
		//
		// "Build this again" means the recorded commit for a pull request, because that is
		// the thing under review. A branch preview is not under review: it is a claim about
		// what `main` looks like now, so rebuilding it at a commit that `main` has since
		// moved past would republish a stale site and call it current. That is the one
		// promise this preview makes.
		if pr.IsBranch() {
			if _, err := d.BuildBranch(ctx, pr.Repo, pr.Branch); err != nil {
				return false, err
			}
			return true, nil
		}

		// ListPreviews already copies the stored commit into HeadSHA; stated here because
		// a build with an empty HeadSHA silently builds the branch tip instead, which is
		// the thing this function exists not to do.
		if pr.HeadSHA == "" {
			pr.HeadSHA = p.Commit
		}
		d.log.Info("rebuilding on request", "pr", pr.String(), "sha", pr.HeadSHA)
		if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// ScanRepo queues a build for every open pull request on a repository, and reports how
// many it queued.
//
// This is what makes adding a project from the dashboard do something. Every other way
// into a build begins with a webhook delivery, so a repository added by hand had nothing
// to build until somebody pushed — and from the operator's side that is
// indistinguishable from a broken installation.
//
// Each pull request goes through handleBuild, exactly as a delivery would: the same
// queued comment, the same supersede behaviour, the same commit lock. Nothing here is a
// second build path, which is the point — a parallel one would drift.
//
// Errors are collected rather than fatal. One pull request whose enqueue fails must not
// stop the rest, and the count tells the caller what actually happened.
// CheckRepoCredential asks the platform's client whether its credential reaches one
// repository. Behind the projects page's Test button.
//
// Here rather than in the projects admin because the client map lives here, and behind an
// optional interface because only one platform needs it — a GitHub App's installation is
// the check, while a Bitbucket access token is pasted in and unverified until something
// uses it.
func (d *Daemon) CheckRepoCredential(ctx context.Context, repo model.Repo) (string, error) {
	client, ok := d.client(repo.Platform)
	if !ok {
		return "", fmt.Errorf("no %s client is configured on this daemon "+
			"(store its credentials on /secrets)", repo.Platform)
	}
	checker, ok := client.(scm.RepoChecker)
	if !ok {
		return "", fmt.Errorf("the %s client has nothing to test: its credential is "+
			"scoped by the platform rather than pasted in here", repo.Platform)
	}
	return checker.CheckRepo(ctx, repo)
}

func (d *Daemon) ScanRepo(ctx context.Context, repo model.Repo) (int, error) {
	client, ok := d.client(repo.Platform)
	if !ok {
		return 0, fmt.Errorf("no %s client is configured on this daemon", repo.Platform)
	}
	lister, ok := client.(scm.PullRequestLister)
	if !ok {
		return 0, fmt.Errorf("the %s client cannot list open pull requests", repo.Platform)
	}

	prs, err := lister.OpenPullRequests(ctx, repo)
	if err != nil {
		return 0, err
	}

	queued := 0
	var errs []error
	for _, pr := range prs {
		if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
			errs = append(errs, fmt.Errorf("pull request %d: %w", pr.Number, err))
			continue
		}
		queued++
	}
	return queued, errors.Join(errs...)
}

// UnlinkPreview removes a preview and stops this pull request being built again.
//
// Two halves, and neither is useful alone. Tearing the preview down without recording
// the decision means the next delivery or the next scan rebuilds it, which reads as
// the removal having failed. Recording the decision without tearing down leaves a live
// share and a pull request comment advertising a preview nothing maintains.
//
// The ignore is written first. A teardown reaches the exposer, the platform and the
// filesystem, so it is the half that can fail partway — and a recorded ignore with a
// half-removed preview is recoverable by unlinking again, while the reverse rebuilds
// itself the moment somebody pushes.
//
// Reports whether the preview existed, so the caller can answer 404 rather than
// pretending to have removed something.
func (d *Daemon) UnlinkPreview(ctx context.Context, previewID string) (bool, error) {
	// Found by listing, as RebuildPreview does: the store has no lookup by id, and one
	// query per operator button press is not worth a second access path to keep in step.
	previews, err := d.store.ListPreviews(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range previews {
		if p.PreviewID != previewID {
			continue
		}
		if err := d.store.IgnorePR(ctx, p.PR); err != nil {
			return true, err
		}
		d.log.Info("unlinking a pull request", "pr", p.PR.String(), "preview", previewID)
		if err := d.teardown(ctx, p.PR, previewID); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, nil
}

// BuildBranch queues a build of a branch, with no pull request behind it.
//
// This is what makes a project's default branch permanently previewable. An empty branch
// means "whatever this repository calls its default", which is read from the platform
// rather than assumed: a repository on `master` would otherwise get a preview of a branch
// that does not exist, failing at the clone with git's own message about a missing ref —
// accurate, and about the wrong thing.
//
// Reports the branch it built, so the caller can say which one without asking again.
//
// The commit is resolved here rather than left empty. A clone with no commit takes whatever
// the branch happens to point at when git runs, which is a different fact from the one the
// preview will claim to be showing — and the build id, the log filename and the per-build
// share are all named after it.
func (d *Daemon) BuildBranch(ctx context.Context, repo model.Repo, branch string) (string, error) {
	client, ok := d.client(repo.Platform)
	if !ok {
		return "", fmt.Errorf("no %s client is configured on this daemon "+
			"(store its credentials on /secrets)", repo.Platform)
	}
	resolver, ok := client.(scm.BranchResolver)
	if !ok {
		return "", fmt.Errorf("the %s client cannot name a branch's head commit", repo.Platform)
	}

	var commit string
	var err error
	if branch == "" {
		branch, commit, err = resolver.DefaultBranch(ctx, repo)
	} else {
		commit, err = resolver.BranchTip(ctx, repo, branch)
	}
	if err != nil {
		return "", err
	}

	// Number 0 is what makes this a branch preview — see model.PullRequest.IsBranch. Every
	// consequence follows from it rather than from a flag threaded through the build path.
	pr := model.PullRequest{
		Repo:    repo,
		Branch:  branch,
		HeadSHA: commit,
		// The base branch is itself. Nothing reads it for a branch build — the changed-file
		// gate is skipped — but leaving it empty would put a blank where a log line expects
		// a ref.
		BaseBranch: branch,
	}
	d.log.Info("building a branch preview", "pr", pr.String(), "sha", commit)
	if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
		return "", err
	}
	return branch, nil
}

// LinkPR builds one pull request by number, whether or not it was unlinked.
//
// The counterpart to UnlinkPreview, and the answer to a pull request that no webhook
// ever announced — a project added by hand before its webhook existed, or one unlinked
// by mistake.
//
// The pull request's branch and head commit come from the platform rather than from the
// caller: a build needs a commit, and a number typed into a form does not carry one.
// That is also the check that the number exists and is open, which is why nothing is
// written until the listing has answered.
func (d *Daemon) LinkPR(ctx context.Context, repo model.Repo, number int) error {
	client, ok := d.client(repo.Platform)
	if !ok {
		return fmt.Errorf("no %s client is configured on this daemon "+
			"(store its credentials on /secrets)", repo.Platform)
	}
	lister, ok := client.(scm.PullRequestLister)
	if !ok {
		return fmt.Errorf("the %s client cannot look up a pull request by number", repo.Platform)
	}

	prs, err := lister.OpenPullRequests(ctx, repo)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if pr.Number != number {
			continue
		}
		// Un-ignored before the build rather than after: handleBuild is where the
		// ignore is enforced, so a link that queued first would be dropped by its own
		// check.
		if _, err := d.store.UnignorePR(ctx, repo, number); err != nil {
			return err
		}
		d.log.Info("linking a pull request", "pr", pr.String())
		return d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr})
	}
	return fmt.Errorf("%s has no open pull request #%d", repo.String(), number)
}

func (d *Daemon) handleBuild(ctx context.Context, ev scm.Event) error {
	pr := ev.PR
	id := pr.PreviewID()

	// An unlinked pull request stops here, and this is the only place that check
	// lives.
	//
	// Every route to a build passes through this function — a webhook delivery, the
	// scan that runs when a project is added, an operator's Build now — so one check
	// covers all of them and none of them can drift. Checked before the queued report,
	// because reporting "queued" and then not building is worse than either.
	//
	// Linking a pull request deletes the row rather than passing a flag past this
	// check: "build this" and "stop ignoring this" are the same request, and a bypass
	// would make the ignore something an operator has to remember to undo.
	if ignored, err := d.store.PRIgnored(ctx, pr.Repo, pr.Number); err != nil {
		// Not fatal. A read failure here must not silently stop building — the
		// unlinked set is a preference, and losing it costs an unwanted preview
		// somebody can unlink again.
		d.log.Warn("could not check whether this pull request is unlinked; building it",
			"pr", pr.String(), "error", err)
	} else if ignored {
		d.log.Info("skipping an unlinked pull request", "pr", pr.String())
		return nil
	}

	// Report queued before enqueueing, not after.
	//
	// Enqueue wakes a worker, and a worker that claims the job reports "building"
	// — so reporting "queued" afterwards is a race this lost most of the time,
	// leaving the comment showing queued for a build already running. Emitting it
	// first means the job does not exist yet, so nothing can be building.
	//
	// The report is best-effort where the enqueue is not: a queue write that fails
	// means no build, and the caller has to hear about it. A comment that fails to
	// say "queued" is a cosmetic loss on the way to saying "building".
	d.report(ctx, scm.Report{
		PR: pr, PreviewID: id, State: scm.StateQueued,
		Commit: pr.HeadSHA, UpdatedAt: time.Now(),
	})

	if err := d.store.Enqueue(ctx, pr); err != nil {
		return err
	}

	// Abandon any build already running for this pull request. Its output is
	// for a commit nobody is going to look at.
	//
	// The entry is cleared, not just cancelled. Clearing it is what makes the
	// superseded build fail its isCurrent check even if the replacement has not
	// been claimed by a worker yet — otherwise a build cancelled while nothing
	// else is registered would still see itself as current and publish.
	d.mu.Lock()
	if b := d.running[id]; b != nil {
		d.log.Info("superseding in-flight build", "pr", pr.String(), "sha", pr.HeadSHA)
		delete(d.running, id)
		b.cancel()
	}
	d.mu.Unlock()

	d.nudge()
	return nil
}

func (d *Daemon) handleTeardown(ctx context.Context, ev scm.Event) error {
	if !d.cfg.Preview.TeardownOnClose {
		return nil
	}
	return d.teardown(ctx, ev.PR, ev.PR.PreviewID())
}

// teardown withdraws a preview, deletes its artifacts, and removes its comment.
func (d *Daemon) teardown(ctx context.Context, pr model.PullRequest, previewID string) error {
	// Hold the commit lock for the whole teardown. Without it, a build already
	// inside its publishing phase would finish and reinstall the preview we
	// just removed, leaving a live share and a deleted database row.
	commit := d.commitLock(previewID)
	commit.Lock()
	defer commit.Unlock()

	d.mu.Lock()
	if b := d.running[previewID]; b != nil {
		delete(d.running, previewID)
		b.cancel()
	}
	pub := d.live[previewID]
	delete(d.live, previewID)
	// Every build share of this preview goes with it. Collected under the lock and
	// closed below, outside it, because closing reaches the exposer's API.
	var buildPubs []*expose.Publication
	for key, bp := range d.liveBuilds {
		if strings.HasPrefix(key, previewID+"/") {
			buildPubs = append(buildPubs, bp)
			delete(d.liveBuilds, key)
		}
	}
	delete(d.commit, previewID)
	// The lifecycle is over, so the next report for this preview starts a new one.
	// Keeping the mark would make a reopened pull request's "queued" look stale
	// against the torn-down build's terminal state and be dropped.
	delete(d.reported, previewID)
	d.mu.Unlock()

	var errs []error
	// The queued build goes with the running one, and for the same reason.
	//
	// Claim used to be the only statement that removed a `jobs` row, so a push landing
	// moments before a close left a job behind — and a worker claimed it minutes later,
	// built it, wrote the rows back and published a share for a preview that had been
	// deliberately removed. Unlinking a pull request is a button an operator presses to
	// make the work stop, so a teardown that leaves the work queued has not done what it
	// said. Cancelling the in-flight build above and dropping the queued one here are the
	// two halves of one act.
	//
	// Collected rather than logged: a surviving job resurrects everything the rest of this
	// function is removing, which makes it the one step whose failure the caller has to
	// hear about.
	//
	// One race is left and is narrow: a worker that has claimed the job but not yet
	// registered it in `d.running` is visible in neither place. The commit lock is what
	// bounds it — that build cannot publish until this teardown returns — and closing it
	// properly means the commit phase re-reading the pull request's state, which is the
	// half of this that needs an API call it does not make today.
	if _, err := d.store.Dequeue(ctx, previewID); err != nil {
		errs = append(errs, fmt.Errorf("removing the queued build: %w", err))
	}
	// Before the shares go, not after: on an exposer whose names are objects with a
	// quota, releasing first is what makes a crash mid-teardown recoverable. See
	// releaseNames.
	d.releaseNames(ctx, previewID, pub, buildPubs)
	if pub != nil {
		if err := pub.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, bp := range buildPubs {
		if err := bp.Close(); err != nil {
			errs = append(errs, fmt.Errorf("withdrawing a build share: %w", err))
		}
	}
	if err := os.RemoveAll(filepath.Join(d.cfg.ArtifactsDir(), previewID)); err != nil {
		errs = append(errs, fmt.Errorf("removing artifacts: %w", err))
	}
	if err := os.RemoveAll(filepath.Join(d.cfg.WorkspacesDir(), previewID)); err != nil {
		errs = append(errs, fmt.Errorf("removing workspace: %w", err))
	}
	// The package cache is keyed on the preview for exactly this: it has the same
	// lifetime as the workspace and artifacts above, so it goes the same way. A
	// cache with a longer life than the branch that filled it has no moment at which
	// anything knows it is safe to delete, and grows until somebody notices the disk.
	// The docker driver's caches are volumes, not directories — see
	// pipeline.CacheVolume for why. Removed here for the same reason the directory is:
	// the cache has the lifetime of the pull request that filled it, and a volume nothing
	// records is a volume nobody ever deletes.
	if d.removeCacheVolumes != nil {
		if err := d.removeCacheVolumes(ctx, previewID); err != nil {
			errs = append(errs, fmt.Errorf("removing the build cache volumes: %w", err))
		}
	}
	// The local driver's cache is still a directory on the host.
	if dir := d.cfg.PreviewCacheDir(previewID); dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("removing the build cache: %w", err))
		}
	}
	if err := d.logs.Remove(previewID); err != nil {
		errs = append(errs, fmt.Errorf("removing build logs: %w", err))
	}
	// No comment to retract for a branch preview, and asking for one is worse than
	// pointless: Retract finds its comment by marker on pull request `Number`, so on a
	// branch preview it would go looking on pull request 0 — a 404 logged as a teardown
	// failure, for a comment that never existed.
	if client, ok := d.client(pr.Repo.Platform); ok && !pr.IsBranch() {
		if err := client.Retract(ctx, pr); err != nil {
			errs = append(errs, fmt.Errorf("retracting comment: %w", err))
		}
	}
	if err := d.store.DeletePreview(ctx, previewID); err != nil {
		errs = append(errs, err)
	}

	d.log.Info("tore down preview", "pr", pr.String(), "preview", previewID)
	d.recordf(pr, "torn-down", "preview withdrawn and artifacts removed")
	return errors.Join(errs...)
}

// releaseNames gives up every exposer-side name this preview ever took.
//
// Only zrok has anything to release — a reserved name is an object with its own
// lifetime, counted against the account's quota, and it survives the share bound to
// it on purpose. Nothing released them, so docpreview leaked one name per branch and,
// once builds got their own shares, one per commit. An exposer that does not
// implement NameReleaser has no such object and this is a no-op.
//
// # Where the names come from
//
// The live publications are not the whole set. A preview restored after a restart
// whose republish failed has a recorded name and no publication, and so does a build
// whose artifacts were pruned; those are exactly the names most likely to be leaked
// already. So the database is read as well — and read here, because teardown deletes
// the row a few lines later.
//
// # Why this cannot fail a teardown
//
// The pull request is gone. What an error leaves behind is one name against a quota,
// which is worth a warning naming the name so it can be deleted by hand — and not
// worth aborting the teardown for, which would strand the artifacts, the workspace,
// the cache and the comment as well.
func (d *Daemon) releaseNames(ctx context.Context, previewID string,
	pub *expose.Publication, buildPubs []*expose.Publication,
) {
	releaser, ok := d.exposer.(expose.NameReleaser)
	if !ok {
		return
	}

	names := map[string]bool{}
	if pub != nil {
		names[pub.Name] = true
	}
	for _, bp := range buildPubs {
		names[bp.Name] = true
	}
	if previews, err := d.store.ListPreviews(ctx); err != nil {
		d.log.Warn("could not read previews while releasing names; "+
			"a name with no live share may be left against the account's quota",
			"preview", previewID, "error", err)
	} else {
		for _, p := range previews {
			if p.PreviewID == previewID {
				names[p.Name] = true
			}
		}
	}
	if builds, err := d.store.BuildsFor(ctx, previewID); err != nil {
		d.log.Warn("could not read builds while releasing names; "+
			"a build's name may be left against the account's quota",
			"preview", previewID, "error", err)
	} else {
		for _, b := range builds {
			names[b.Name] = true
		}
	}
	delete(names, "")

	for name := range names {
		if err := releaser.ReleaseName(ctx, name); err != nil {
			d.log.Warn("could not release an exposer name; it stays counted against "+
				"the account's limit until it is deleted by hand",
				"preview", previewID, "name", name, "error", err)
		}
	}
}

func (d *Daemon) nudge() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// worker claims and runs builds until ctx is cancelled.
func (d *Daemon) worker(ctx context.Context, n int) {
	log := d.log.With("worker", n)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		pr, err := d.store.Claim(ctx)
		switch {
		case errors.Is(err, store.ErrNoJobs):
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			case <-ticker.C:
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			log.Error("claiming job", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}

		d.build(ctx, pr)

		// There may be more work; do not wait for the tick.
		d.nudge()
	}
}

// build runs one pull request through the pipeline.
func (d *Daemon) build(parent context.Context, pr model.PullRequest) {
	id := pr.PreviewID()
	log := d.log.With("pr", pr.String(), "preview", id, "sha", pr.HeadSHA)

	ctx, cancel := context.WithCancel(parent)
	me := &build{cancel: cancel, pr: pr, started: time.Now()}

	d.mu.Lock()
	d.running[id] = me
	d.mu.Unlock()

	defer func() {
		cancel()
		d.mu.Lock()
		// Clear the entry only if it is still ours. Comparing pointer identity
		// is the whole point: an older, superseded build finishing late must
		// not remove the cancel handle belonging to the build that replaced it,
		// or the next push would have nothing to cancel.
		if d.running[id] == me {
			delete(d.running, id)
		}
		d.mu.Unlock()
	}()

	client, ok := d.client(pr.Repo.Platform)
	if !ok {
		log.Error("no client for platform", "platform", pr.Repo.Platform)
		return
	}

	started := time.Now()
	d.report(ctx, scm.Report{
		PR: pr, PreviewID: id, State: scm.StateBuilding,
		Commit: pr.HeadSHA, UpdatedAt: started,
	})

	result, decision, err := d.runPipeline(ctx, me, client, pr, log)

	// A superseded build must not write a report: a newer build is already
	// updating the same comment, and a late failure message would overwrite a
	// perfectly good "ready".
	//
	// This only suppresses the *comment*. Everything with a lasting effect —
	// the artifact directory, the published share, the database row — is
	// guarded inside runPipeline's commit phase, because by the time control
	// returns here those writes have already happened.
	if errors.Is(err, errSuperseded) || (ctx.Err() != nil && parent.Err() == nil) {
		log.Info("build superseded, discarding result")
		return
	}

	switch {
	case err != nil:
		log.Error("build failed", "error", err)
		d.recordFailure(ctx, pr, err)
		d.report(ctx, scm.Report{
			PR: pr, PreviewID: id, State: scm.StateFailed,
			// Scrubbed a second time on the way out. The builder already
			// redacts what it returns, but a failure can also come from the
			// clone or the detect script, and this is the single point every
			// one of them passes through before reaching a comment. Redacting
			// twice costs a string scan; missing a path costs a credential.
			Commit: pr.HeadSHA,
			Reason: d.scrub(firstLine(err.Error())), LogExcerpt: d.scrub(err.Error()),
			Duration: time.Since(started), UpdatedAt: time.Now(),
		})

	case !decision.Build:
		log.Info("skipped", "reason", decision.Reason)
		d.recordSkip(ctx, pr, decision.Reason)
		d.report(ctx, scm.Report{
			PR: pr, PreviewID: id, State: scm.StateSkipped,
			Commit: pr.HeadSHA, Reason: decision.Reason, UpdatedAt: time.Now(),
		})

	default:
		log.Info("preview ready", "url", result.URL, "name", result.Name, "took", result.Duration)
		d.report(ctx, scm.Report{
			PR: pr, PreviewID: id, State: scm.StateReady,
			URL: result.URL, Name: result.Name, Commit: pr.HeadSHA,
			Duration: result.Duration, UpdatedAt: time.Now(),
		})
	}
}

// buildOutcome is the successful result of runPipeline.
type buildOutcome struct {
	URL  string
	Name string
	// BuildURL is this build's own share, empty when it has none. Recorded on the
	// build row so the dashboard can offer it after a restart; the pull request
	// comment keeps linking to the branch URL, which is the one that stays current.
	BuildURL string
	Duration time.Duration
}

// errSuperseded means a newer push replaced this build before it could
// publish. It is not a failure and produces no report.
var errSuperseded = errors.New("build superseded by a newer push")

// runPipeline clones, detects, builds, and publishes.
func (d *Daemon) runPipeline(
	ctx context.Context,
	me *build,
	client scm.Client,
	pr model.PullRequest,
	log *slog.Logger,
) (buildOutcome, pipeline.Decision, error) {
	var out buildOutcome

	cloneURL, err := client.CloneURL(ctx, pr)
	if err != nil {
		return out, pipeline.Decision{}, err
	}

	ws, err := d.cloner.Clone(ctx, pr, cloneURL)
	if err != nil {
		return out, pipeline.Decision{}, err
	}
	// The working tree is only needed until the build output is copied out.
	defer func() {
		if err := ws.Remove(); err != nil {
			log.Warn("removing workspace", "error", err)
		}
	}()

	repoCfg, err := config.LoadRepoConfig(ws.Dir)
	if err != nil {
		return out, pipeline.Decision{}, err
	}

	// The project's settings win over the branch's.
	//
	// .docpreview.yml arrived in the pull request, so on any repository where
	// opening one is not a privilege, its author chose what runs — and the local
	// driver runs it. An operator-authored project row cannot be edited by a
	// contributor, so where the two disagree the row is the trustworthy one.
	repoCfg = d.applyProject(ctx, pr, repoCfg)

	// Resolved here rather than beside the builder below, because the driver decides
	// what the *detector* is allowed to run and the detector runs first.
	driver, image := d.projectDriver(ctx, pr)
	if err := d.driverAllowed(driver); err != nil {
		return out, pipeline.Decision{}, err
	}
	repoCfg = d.confineToDriver(ctx, pr, repoCfg, driver)

	// A branch preview is built unconditionally, and the detector is not consulted.
	//
	// The gate exists to answer "did this pull request touch the documentation", and it
	// answers it from the diff against the merge base. A branch has no such diff: there is
	// no pull request to ask the platform about, and `ChangedFiles` on number 0 would ask a
	// question with no meaning. Worse, an empty file list is exactly what the detector reads
	// as "nothing to build" — so the permanent preview of `main` would skip every build and
	// never publish anything.
	//
	// Building every time is also the behaviour this preview is for. It is the current state
	// of the branch; a commit that changed no documentation still moved the branch, and a
	// preview that silently lags behind it is worse than one rebuilt for nothing.
	decision := pipeline.Decision{
		Build:  true,
		Reason: "Branch preview — every commit on this branch is built.",
	}
	if !pr.IsBranch() {
		changed, err := client.ChangedFiles(ctx, pr)
		if err != nil {
			return out, pipeline.Decision{}, err
		}

		decision, err = d.detector.Detect(ctx, ws, repoCfg, changed)
		if err != nil {
			return out, decision, err
		}
		if !decision.Build {
			return out, decision, nil
		}
	}

	// The name has to be settled before the build, not at publish. Under a
	// path-mounting exposer it decides the path the site is served under, and
	// Docusaurus bakes baseUrl in at build time — so a build that does not know
	// its own prefix produces a site that 404s every asset it references.
	name, err := d.previewName(pr)
	if err != nil {
		return out, decision, err
	}
	repoCfg.Build.BaseURL = d.previewBaseURL(name, repoCfg.Build.BaseURL)

	// Open a log for this build and stream into it. The build ID is the commit
	// plus a timestamp: the commit alone would collide when the same commit is
	// rebuilt, and a timestamp alone would sort correctly but tell you nothing.
	// One builder for the whole build, read once. A rotation mid-build must not
	// leave the log scrubbed by one redactor and the environment set by another.
	// One builder for the whole build, with this project's driver applied. Taken
	// once and reused below, so a rotation mid-build cannot leave the log scrubbed
	// by one redactor and the environment set by another.
	builder := d.currentBuilder().WithDriver(driver, image).WithSecrets(d.projectSecrets(pr))

	buildID := logBuildID(pr.HeadSHA)
	logw, logErr := d.logs.Begin(pr.PreviewID(), buildID, builder.Redactor())
	if logErr != nil {
		// A build with no log is worse than no build, but only slightly — and
		// refusing to build because a log file could not be opened would be a
		// disproportionate response to a full disk.
		log.Warn("could not open a build log; the build will run untailed", "error", logErr)
	}

	// Record the attempt before it runs, so a build in flight appears in the
	// history rather than materialising only once it ends — which is when somebody
	// looking at the dashboard most wants to see it.
	startedAt := time.Now()
	d.saveBuild(store.Build{
		PreviewID: pr.PreviewID(), BuildID: buildID, PR: pr, Commit: pr.HeadSHA,
		State: string(scm.StateBuilding), StartedAt: startedAt,
	})

	var sink io.Writer
	if logw != nil {
		sink = logw
	}
	// No event is recorded for opening the log. It used to emit one, which put
	// a "log — build log opened" row in the activity feed between every queued
	// and building pair: an entry for a thing that always happens, carrying no
	// information, at twice the rate of the states anyone is watching for.

	result, err := builder.Build(ctx, ws, repoCfg, sink)

	if logw != nil {
		if err != nil {
			// The summary line for a *build* failure, the whole error for one of ours.
			//
			// The builder wraps the whole build output into its error so a pull request
			// comment can quote it, so writing err.Error() for those appends a second copy
			// of the log to the log — which is what the first version did.
			//
			// But errArtifactsUnusable is docpreview's own, carries no build output, and is
			// four lines of diagnosis: the base URL the preview will serve at, the one the
			// site was built for, the asset that proves it, and the two ways to fix it.
			// Truncating that to its first line left "the site was built for a different
			// base URL than the preview will serve" with none of the numbers — an error
			// stating a mismatch and hiding both sides of it.
			text := firstLine(err.Error())
			var baseURL *pipeline.BaseURLError
			if errors.As(err, &baseURL) {
				// Its diagnosis, not its Error(): the latter has the whole build log
				// appended, which is already in this file.
				text = baseURL.Diagnosis
			}
			fmt.Fprintf(logw, "\n%s\n", d.scrub(text))
		}
		if ferr := d.logs.Finish(pr.PreviewID(), logw); ferr != nil {
			log.Warn("closing the build log", "error", ferr)
		}
	}

	if err != nil {
		d.saveBuild(store.Build{
			PreviewID: pr.PreviewID(), BuildID: buildID, PR: pr, Commit: pr.HeadSHA,
			State: string(scm.StateFailed), Reason: d.scrub(firstLine(err.Error())),
			StartedAt: startedAt, FinishedAt: time.Now(),
		})
		return out, decision, err
	}

	d.saveBuild(store.Build{
		PreviewID: pr.PreviewID(), BuildID: buildID, PR: pr, Commit: pr.HeadSHA,
		State: string(scm.StateReady), StartedAt: startedAt, FinishedAt: time.Now(),
	})

	// --- Commit phase ------------------------------------------------------
	//
	// Everything above this line is reversible: a clone in a scratch directory
	// and a build tree nobody can see. Everything below it is not — it replaces
	// the served artifacts, takes the public name away from whoever holds it,
	// and rewrites the database row.
	//
	// Two pushes in quick succession must end with the second one published,
	// never the first. Cancellation alone does not achieve that: the zrok SDK's
	// CreateShare takes no context and so cannot be interrupted, and publishing
	// a name withdraws the share currently using it. A superseded build that
	// reaches Publish therefore destroys the newer preview rather than merely
	// wasting its own effort.
	//
	// So the check and the writes happen together, under a per-preview lock. A
	// build that is no longer current stops here, having changed nothing.
	commit := d.commitLock(pr.PreviewID())
	commit.Lock()
	defer commit.Unlock()

	if ctx.Err() != nil || !d.isCurrent(pr.PreviewID(), me) {
		return out, decision, errSuperseded
	}

	// Move the output out of the workspace before the workspace is deleted.
	//
	// One directory per build, not per preview. A single directory per preview was
	// overwritten by every build, so an older commit had nothing left to serve —
	// which is why the log pane could offer a build the Open button beside it could
	// not reach. Retention is what bounds this; see TODO.md.
	//
	// buildID is safe as a path component: buildlog.Begin refuses one containing a
	// separator, and this is the same value.
	artifactDir := filepath.Join(d.cfg.ArtifactsDir(), pr.PreviewID(), buildID)
	if err := replaceDir(result.OutputDir, artifactDir); err != nil {
		return out, decision, err
	}

	site, err := preview.New(artifactDir, repoCfg.Build.BaseURL)
	if err != nil {
		return out, decision, err
	}

	pub, err := d.exposer.Publish(ctx, expose.Spec{
		PreviewID: pr.PreviewID(),
		Name:      name,
		BaseURL:   repoCfg.Build.BaseURL,
		PR:        pr,
	}, site)
	if err != nil {
		return out, decision, err
	}

	d.mu.Lock()
	if old := d.live[pr.PreviewID()]; old != nil {
		old.Close()
	}
	d.live[pr.PreviewID()] = pub
	d.mu.Unlock()

	// A detached context, because the write must complete even though this
	// build's own context may have been cancelled moments ago. The share is
	// already live; a database row that does not match it would survive a
	// restart as an unreapable orphan.
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelSave()

	if err := d.store.SavePreview(saveCtx, store.Preview{
		PreviewID:   pr.PreviewID(),
		PR:          pr,
		Name:        pub.Name,
		URL:         pub.URL,
		BaseURL:     repoCfg.Build.BaseURL,
		ArtifactDir: artifactDir,
		Commit:      pr.HeadSHA,
		State:       scm.StateReady,
		UpdatedAt:   time.Now(),
	}); err != nil {
		// The share is live but nothing durable records it. Leaving it there
		// would mean a working URL, a comment saying the build failed, an empty
		// /status, and a restart that reaps the share as an unknown orphan —
		// publication and persistent state pulled apart, which is precisely
		// what this phase exists to prevent.
		//
		// So undo the publish. Nothing live, nothing recorded, and a failure
		// report that is true. The new artifacts stay on disk unreferenced,
		// which costs a directory and is overwritten by the next build.
		d.unpublish(pr.PreviewID())
		return out, decision, fmt.Errorf("recording the preview failed, so it was withdrawn: %w", err)
	}

	// A second share, pinned to this commit, beside the branch share that follows
	// whatever is newest. Best effort: see publishBuildShare.
	buildURL := d.publishBuildShare(ctx, pr, buildID, name, repoCfg, site)
	if buildURL != "" {
		// The row was already written as ready above, before this URL existed. The
		// upsert keeps a non-empty name and url and refreshes the rest, so writing
		// it again is how the share reaches the row it belongs to — and it has to
		// reach it, or the next reap treats the share as an orphan.
		d.saveBuild(store.Build{
			PreviewID: pr.PreviewID(), BuildID: buildID, PR: pr, Commit: pr.HeadSHA,
			State: string(scm.StateReady), StartedAt: startedAt, FinishedAt: time.Now(),
			Name: name + "-" + model.ShortSHA(pr.HeadSHA), URL: buildURL,
		})
	}

	// Artifacts are per build now, so this is where the growth is bounded. After
	// the row is saved, never before: a prune that ran first could delete the
	// directory this build is about to publish from.
	d.pruneArtifacts(pr.PreviewID(), buildID)

	return buildOutcome{
		URL: pub.URL, Name: pub.Name, BuildURL: buildURL, Duration: result.Duration,
	}, decision, nil
}

// publishBuildShare gives one build a URL of its own and returns it, empty on any
// failure.
//
// The branch share follows the newest successful build, which is what a reviewer
// wants and what the pull request comment links to. It is also why an older build
// could be selected in the dashboard's picker while the only Open button on screen
// went somewhere else: one share cannot represent two builds.
//
// # Best effort, deliberately
//
// The branch share is the contract and it is already live by the time this runs.
// This one is extra, and every way it can fail is a reason to keep going rather
// than to fail the build: a reserved-name quota on the exposer account, a name
// collision, an exposer that cannot mint a second share. Returning an error here
// would turn "you get one fewer URL than you hoped" into a failed build and a
// comment saying the docs did not build, which is a much worse trade.
//
// So it logs at warn and returns empty. A caller with no build URL renders the same
// as a daemon that has never had one.
func (d *Daemon) publishBuildShare(
	ctx context.Context, pr model.PullRequest, buildID, branchName string,
	repoCfg config.RepoConfig, site http.Handler,
) string {
	// Derived from the branch name rather than rendered from the template again,
	// so a name_template that separates repositories keeps doing so here, and the
	// two names sort next to each other in any list of shares.
	name := branchName + "-" + model.ShortSHA(pr.HeadSHA)

	pub, err := d.exposer.Publish(ctx, expose.Spec{
		PreviewID: pr.PreviewID(),
		BuildID:   buildID,
		Name:      name,
		BaseURL:   repoCfg.Build.BaseURL,
		PR:        pr,
	}, site)
	if err != nil {
		d.log.Warn("this build has no URL of its own; the branch URL is unaffected",
			"pr", pr.String(), "build", buildID, "name", name, "error", err)
		return ""
	}

	d.mu.Lock()
	if old := d.liveBuilds[buildKey(pr.PreviewID(), buildID)]; old != nil {
		// The same build published twice is a rebuild of one commit. Close the
		// older publication *after* the new one exists, which is the ordering
		// every other publish here uses.
		old.Close()
	}
	d.liveBuilds[buildKey(pr.PreviewID(), buildID)] = pub
	d.mu.Unlock()

	d.log.Info("published build", "pr", pr.String(), "build", buildID,
		"name", pub.Name, "url", pub.URL)
	return pub.URL
}

// buildKey names one build's publication in d.liveBuilds. Same shape as
// expose.Spec.Key, and deliberately so: the two maps are indexed alike.
func buildKey(previewID, buildID string) string { return previewID + "/" + buildID }

// markOpenable sets Openable on each event that still has something to show.
//
// Retention prunes old builds, so "this entry names a preview that exists" — which
// is all the page could check for itself — stopped being the right question. An
// entry whose build has been pruned must go inert rather than offer a click that
// lands on an empty pane.
//
// A build log is enough on its own. The log outlives the artifacts under a shorter
// keep_builds than keep_logs, and reading why a build failed is worth a click even
// when the site it produced is gone.
//
// Matched on the build id's suffix, because an event carries a short sha and a build
// id is `<date>-<time>-<sha>`. An event with no commit — a teardown, anything
// recorded against the preview rather than a build — is openable when the preview
// still exists, which is what the page used to assume for everything.
//
// One listing per preview, cached across the batch: sixty events over three previews
// is three directory reads, not sixty.
func (d *Daemon) markOpenable(events []Event) []Event {
	logs := map[string][]string{}
	buildsFor := func(previewID string) []string {
		if got, ok := logs[previewID]; ok {
			return got
		}
		var ids []string
		// A listing error means no logs to offer, which is the same answer as an
		// empty listing for this purpose.
		metas, _ := d.logs.List(previewID)
		for _, m := range metas {
			ids = append(ids, m.BuildID)
		}
		// Artifacts too: a build whose log has aged out past keep_logs may still be
		// serving, and that is even more worth opening than the log was.
		if entries, err := os.ReadDir(filepath.Join(d.cfg.ArtifactsDir(), previewID)); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					ids = append(ids, e.Name())
				}
			}
		}
		logs[previewID] = ids
		return ids
	}

	out := make([]Event, 0, len(events))
	for _, e := range events {
		if e.PreviewID == "" {
			out = append(out, e)
			continue
		}
		if e.Commit == "" {
			// Nothing build-specific to look for. The preview's own existence is the
			// only thing that could make this openable, and the page already knows it.
			e.Openable = true
			out = append(out, e)
			continue
		}
		for _, id := range buildsFor(e.PreviewID) {
			if strings.HasSuffix(id, "-"+e.Commit) {
				e.Openable = true
				break
			}
		}
		out = append(out, e)
	}
	return out
}

// pruneArtifacts keeps the newest preview.keep_builds build directories.
//
// Build ids are `<date>-<time>-<sha>`, so sorting the directory names in reverse
// puts the newest first and no timestamps need parsing. That is a real dependency
// on the id's shape rather than a convenience, which is why it is stated here: a
// build id that stopped starting with a sortable timestamp would silently prune the
// wrong builds.
//
// keep is the build that just published and is never removed, whatever the sort
// says. A clock that went backwards — an NTP correction, or a daemon restarted in a
// different timezone — would otherwise make the newest build sort last and delete
// the artifacts it is serving from.
//
// Best effort throughout. A directory that will not delete is usually one a request
// is reading, and failing the build over it would turn a disk-space concern into a
// failed pull request comment.
func (d *Daemon) pruneArtifacts(previewID, keep string) {
	limit := d.cfg.Preview.KeepBuilds
	if limit < 1 {
		return
	}

	dir := filepath.Join(d.cfg.ArtifactsDir(), previewID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && e.Name() != keep {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	// limit-1, because the build being kept counts against the limit.
	for i, name := range names {
		if i < limit-1 {
			continue
		}
		victim := filepath.Join(dir, name)
		if err := os.RemoveAll(victim); err != nil {
			d.log.Debug("leaving old artifacts in place", "dir", victim, "error", err)
			continue
		}
		d.retireBuildShare(previewID, name)
		d.log.Info("pruned old build artifacts",
			"preview", previewID, "build", name, "keep_builds", limit)
	}
}

// retireBuildShare withdraws a pruned build's own share, forgets its URL, and gives
// its name back.
//
// Without this, pruning deleted directories and nothing else. What was left was a live
// share serving a build whose files were gone — a URL the dashboard still offered and
// which answered 404 for whatever the exposer does with a missing root — plus a
// reserved name against the account's quota. Neither was recoverable by the hourly
// sweep, because `Reap`'s keep-set is built from builds with a recorded `url` and the
// row still had one: the leak protected itself.
//
// Three steps, in this order, and the order is the same reasoning as teardown's. The
// name is released first, so a crash between steps leaves a de-reserved name whose
// share the next startup collects. The share is withdrawn second. The row is cleared
// last, because a row with no URL is what stops `Reap` protecting the share, and doing
// that before the withdraw would let the sweep race the withdraw for it.
//
// Every failure is a warning. A build's artifacts are already gone by the time this
// runs, and the build that triggered the prune succeeded — turning a tidy-up failure
// into a failed build would be the wrong trade.
func (d *Daemon) retireBuildShare(previewID, buildID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d.mu.Lock()
	pub := d.liveBuilds[buildKey(previewID, buildID)]
	delete(d.liveBuilds, buildKey(previewID, buildID))
	d.mu.Unlock()

	// The name comes from the publication when the share is live, and from the row when
	// it is not — a build restored after a restart, or one whose publish failed, still
	// holds a name.
	name := ""
	if pub != nil {
		name = pub.Name
	}
	if name == "" {
		if builds, err := d.store.BuildsFor(ctx, previewID); err == nil {
			for _, b := range builds {
				if b.BuildID == buildID {
					name = b.Name
					break
				}
			}
		}
	}
	if releaser, ok := d.exposer.(expose.NameReleaser); ok && name != "" {
		if err := releaser.ReleaseName(ctx, name); err != nil {
			d.log.Warn("could not release a pruned build's name; it stays against the "+
				"account's limit", "preview", previewID, "build", buildID,
				"name", name, "error", err)
		}
	}
	if pub != nil {
		if err := pub.Close(); err != nil {
			d.log.Warn("withdrawing a pruned build's share",
				"preview", previewID, "build", buildID, "error", err)
		}
	}
	if err := d.store.ClearBuildShare(ctx, previewID, buildID); err != nil {
		d.log.Warn("clearing a pruned build's recorded share",
			"preview", previewID, "build", buildID, "error", err)
	}
}

// unpublish withdraws the live publication for a preview and forgets it.
//
// Callers must already hold the preview's commit lock. It deliberately does not
// try to restore whatever was published before: Publish withdrew that share to
// take the name, so there is nothing to restore to. "Nothing is live" is the
// only consistent state reachable from here, and it is an accurate one.
func (d *Daemon) unpublish(previewID string) {
	d.mu.Lock()
	pub := d.live[previewID]
	delete(d.live, previewID)
	d.mu.Unlock()

	if pub != nil {
		if err := pub.Close(); err != nil {
			d.log.Error("withdrawing a preview that could not be recorded",
				"preview", previewID, "error", err)
		}
	}

	// Any row from an earlier build now describes a share that no longer exists
	// and artifacts that have been replaced.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.store.DeletePreview(ctx, previewID); err != nil {
		d.log.Error("clearing the preview record after a failed publish",
			"preview", previewID, "error", err)
	}
}

// NamePrefix is what every published name starts with.
//
// The stored override if an operator set one, the config file's value otherwise. Loaded at
// startup and updated by SetNamePrefix rather than read per build: this is on the build path
// and it changes about once in the life of an installation.
func (d *Daemon) NamePrefix() string {
	if p := d.namePrefix.Load(); p != nil {
		return *p
	}
	return model.NamePrefix(d.cfg.Exposer.Prefix)
}

// SetNamePrefix changes the prefix and records it, so it survives a restart.
//
// It does **not** rename anything already published. Every live preview keeps the name it
// was created with until its next build, so an installation that has been running under one
// prefix and is given another serves a mix — which is the honest outcome and the reason the
// dashboard says so beside the field. Renaming in place would mean withdrawing and
// recreating every share, which is minutes of 404s and a comment on every open pull request
// pointing at a URL that has moved.
func (d *Daemon) SetNamePrefix(ctx context.Context, prefix string) error {
	prefix = model.NamePrefix(prefix)
	if why := model.ValidPrefix(prefix); why != "" {
		return errors.New(why)
	}
	if err := d.store.SetSetting(ctx, store.SettingNamePrefix, prefix); err != nil {
		return err
	}
	d.namePrefix.Store(&prefix)
	d.log.Info("name prefix changed", "prefix", prefix,
		"note", "existing previews keep their names until rebuilt")
	return nil
}

// loadNamePrefix reads the stored override at startup.
//
// A missing row means "never set", which falls back to the config file. A row holding the
// empty string means an operator deliberately cleared it, which is not the same thing and
// must not silently re-inherit the file's value.
func (d *Daemon) loadNamePrefix(ctx context.Context) {
	v, ok, err := d.store.Setting(ctx, store.SettingNamePrefix)
	if err != nil {
		// Not fatal: the config file's value is a working answer, and refusing to start
		// over a cosmetic setting would be the wrong trade.
		d.log.Warn("reading the stored name prefix; using the config file's",
			"error", err, "prefix", model.NamePrefix(d.cfg.Exposer.Prefix))
		return
	}
	if !ok {
		return
	}
	v = model.NamePrefix(v)
	d.namePrefix.Store(&v)
}

// previewName renders the configured name template for the active exposer.
func (d *Daemon) previewName(pr model.PullRequest) (string, error) {
	// The prefix belongs to the exposer, not to the template: it applies whichever template
	// is in play, and an operator who has to remember to write it into three templates will
	// one day write it into two.
	p := d.NamePrefix()
	switch d.cfg.Exposer.Kind {
	case "frontdoor":
		return expose.RenderName(d.cfg.Exposer.Frontdoor.NameTemplate, pr, p)
	case "ziti":
		return expose.RenderName(d.cfg.Exposer.Ziti.NameTemplate, pr, p)
	default:
		return expose.RenderName(d.cfg.Exposer.Zrok.NameTemplate, pr, p)
	}
}

// report publishes a status to the pull request, logging rather than
// propagating failures.
//
// Reporting is best-effort on purpose. If GitHub is down, the build still ran
// and the preview is still live; failing the build because the comment could
// not be written would throw away work that succeeded.
func (d *Daemon) report(ctx context.Context, r scm.Report) {
	// A state this commit has already moved past is dropped everywhere, not just
	// on the way to the platform. The dashboard reads the same rows, so letting a
	// stale report through here would move it backwards too — and the three
	// surfaces disagreeing is worse than any of them being briefly behind.
	if d.staleReport(r) {
		d.log.Debug("dropping a report for a state already passed",
			"pr", r.PR.String(), "preview", r.PreviewID, "state", r.State, "commit", r.Commit)
		return
	}

	// Record before publishing, and regardless of whether a client exists.
	// Every state change funnels through here, which makes it the one place
	// that sees the whole lifecycle — and the dashboard should show a build
	// happening even when the platform that asked for it is unreachable.
	d.record(r, reportMessage(r))

	// A branch preview stops here: recorded for the dashboard, never sent to the platform.
	//
	// There is nothing to send it to. A branch has no pull request, so an upsert would have
	// to invent one — and the failure mode if it tried is worse than nothing: `findComment`
	// looks for the marker on pull request `Number`, which is 0, and what a platform does
	// with a comment on pull request 0 is not a thing worth discovering in production.
	//
	// The check is here rather than at the call sites because this is the funnel every
	// report passes through. There are five callers and a sixth is a matter of time.
	if r.PR.IsBranch() {
		return
	}

	client, ok := d.client(r.PR.Repo.Platform)
	if !ok {
		return
	}

	// Where to read the detail the comment declines to quote. Set here rather
	// than at each call site because this is the one funnel every report passes
	// through, and a report that reached a platform without it would tell
	// somebody a build failed and not where to look.
	//
	// The dashboard, with the preview in the fragment — not `/logs/<preview>`, which
	// this used to be and which is a JSON array. The only link in a failure comment
	// handed a reviewer a raw payload, and a fragment is the right half of the URL to
	// put it in: the daemon never sees it, so it cannot leak into a log, and the page
	// opens that preview's log pane on arrival.
	if r.State == scm.StateFailed && r.DetailURL == "" {
		if base := strings.TrimRight(d.cfg.DashboardURL, "/"); base != "" {
			r.DetailURL = fmt.Sprintf("%s/#preview=%s", base, r.PreviewID)
		}
	}

	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}

	d.publisher.send(r, client)
}

// publishReport writes one report to a platform.
func (d *Daemon) publishReport(r scm.Report, client scm.Client) {
	// A detached context with its own deadline: a cancelled build still has to be
	// able to say why it stopped, and this runs from a timer rather than from the
	// caller's goroutine, so there is no request context to inherit.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Publish(ctx, r); err != nil {
		d.log.Error("publishing report", "pr", r.PR.String(), "state", r.State, "error", err)
		return
	}
	// Logged on success too, at info.
	//
	// Without this a published report left no trace: the failure path logged, the github
	// client logged its comment *edit* at debug, and a comment stuck on "Building" was
	// therefore indistinguishable in the log from one updated correctly. Which is exactly
	// the state a real timeout left — the build row said failed, the comment did not, and
	// nothing recorded whether the write was attempted, refused, or never reached.
	//
	// One line per state change per preview, four-ish per build. Cheap against being able
	// to answer "did the pull request hear about this".
	d.log.Info("reported to the platform",
		"pr", r.PR.String(), "state", r.State, "commit", r.Commit)
}

// reportMessage is the one-line description of a state change.
func reportMessage(r scm.Report) string {
	switch r.State {
	case scm.StateQueued:
		return "queued"
	case scm.StateBuilding:
		return "building"
	case scm.StateReady:
		if r.Duration > 0 {
			return "ready in " + r.Duration.Round(time.Second).String()
		}
		return "ready"
	case scm.StateSkipped:
		if r.Reason != "" {
			return r.Reason
		}
		return "skipped"
	case scm.StateFailed:
		if r.Reason != "" {
			return r.Reason
		}
		return "failed"
	default:
		return string(r.State)
	}
}

// reaper removes previews that have gone stale.
func (d *Daemon) reaper(ctx context.Context) {
	// Hourly: preview TTLs are measured in days, so anything finer is wasted
	// wakeups, and anything coarser leaves a dead preview linked from a
	// comment for most of a working day.
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		expired, err := d.store.ExpiredPreviews(ctx, d.cfg.Preview.TTL)
		if err != nil {
			d.log.Error("finding expired previews", "error", err)
			continue
		}
		for _, p := range expired {
			// A branch preview does not expire, and this is the only place that could have
			// expired it.
			//
			// The TTL exists because a pull request's preview outlives its usefulness — the
			// pull request is merged or abandoned and nobody closes it properly. A branch
			// preview has no such end: `main` is still `main` after a quiet fortnight, and
			// the whole promise of this preview is that its URL always answers. A repository
			// with no commits for longer than the TTL would otherwise have its permanent
			// preview quietly deleted, which is the one thing it must not do.
			if p.PR.IsBranch() {
				continue
			}
			d.log.Info("reaping expired preview", "preview", p.PreviewID, "age", time.Since(p.UpdatedAt))
			if err := d.teardown(ctx, p.PR, p.PreviewID); err != nil {
				d.log.Error("reaping preview", "preview", p.PreviewID, "error", err)
			}
		}

		// Build logs outlive their previews only until the retention window
		// closes. They can contain anything a build printed, so an unbounded
		// pile of them is a liability rather than an asset.
		if n, err := d.logs.Sweep(d.cfg.Build.KeepLogs); err != nil {
			d.log.Warn("sweeping build logs", "error", err)
		} else if n > 0 {
			d.log.Info("swept old build logs", "previews", n)
		}

		// The build history is bounded on the same window, so a row and its log
		// disappear together and the picker never lists an outcome whose log has
		// gone. A row per push otherwise accumulates forever.
		if err := d.store.PruneBuilds(ctx, time.Now().Add(-d.cfg.Build.KeepLogs)); err != nil {
			d.log.Warn("pruning the build history", "error", err)
		}

		// Anything the exposer holds that is not a recorded publication is a leak.
		//
		// Both kinds have to be listed. The keep-set is expressed in publication
		// keys, and a set holding only preview ids would mark every build share as
		// an orphan — so this sweep would delete each one minutes after it was
		// published, and the dashboard would offer URLs that had already gone.
		keep := map[string]bool{}
		if all, err := d.store.ListPreviews(ctx); err == nil {
			for _, p := range all {
				keep[p.PreviewID] = true

				// Only builds that recorded a share. A build row with no URL never
				// had one, and adding its key would keep nothing while making the
				// set larger.
				builds, err := d.store.BuildsFor(ctx, p.PreviewID)
				if err != nil {
					// Reaping with an incomplete keep-set deletes live shares, so
					// skip the sweep entirely rather than sweep on partial truth.
					d.log.Warn("listing builds for the reap keep-set; skipping this sweep",
						"preview", p.PreviewID, "error", err)
					keep = nil
					break
				}
				for _, b := range builds {
					if b.URL != "" {
						keep[buildKey(p.PreviewID, b.BuildID)] = true
					}
				}
			}
			if keep != nil {
				if err := d.exposer.Reap(ctx, keep); err != nil {
					d.log.Warn("reaping shares", "error", err)
				}
			}
		}
	}
}

// Status is the payload of the status endpoint.
type Status struct {
	Exposer string `json:"exposer"`

	// Role is how this caller is signed in: "admin", "viewer", or empty when no login was
	// needed or none was presented.
	//
	// Filled in by the ingress rather than by the daemon, because it is a property of the
	// request and everything else here is a property of the world. It is on this payload
	// because the page cannot see the session cookie — it is HttpOnly, deliberately — so
	// without being told, the dashboard has no way to know whether to offer a sign-out.
	Role string `json:"role,omitempty"`

	// Instance changes when the daemon restarts, which is how an open tab learns
	// its JavaScript is older than the daemon answering it.
	//
	// The dashboard is one embedded file with no cache busting, so a tab left open
	// across a rebuild keeps running the code it loaded. That produced three false
	// bug reports in one afternoon — a fixed layout that had not moved, a filter
	// that had not applied, a stale row — each costing a diagnosis before somebody
	// thought to reload.
	//
	// The process start time, not a build id: a version stamped at compile time
	// cannot tell a restart of the same binary from no restart at all, and restart
	// is the event that matters. The page compares rather than trusts, so any
	// change prompts a reload.
	Instance string `json:"instance"`

	Pending  int             `json:"pending"`
	Running  int             `json:"running"`
	Previews []StatusPreview `json:"previews"`
	Events   []Event         `json:"events"`

	// Starting is true while recovery is still running.
	//
	// Workers do not exist until it finishes — reap-then-republish has to complete before
	// anything may publish — so a build queued during that window sits there, and the page
	// showed "Queued" with no way to tell it apart from a stuck queue. Which is what it was
	// reported as, twice.
	Starting bool `json:"starting"`

	// Startup is which part of recovery is running, and how far through it is.
	//
	// Starting alone answered "not yet" and nothing else, for the two minutes it takes
	// eleven zrok round trips to complete. A wait with no progress is indistinguishable
	// from a hang, so the first thing anybody did with that banner was restart the
	// daemon — losing the recovery it was reporting. Absent once recovery finishes,
	// because there is then nothing to say.
	Startup *StartupProgress `json:"startup,omitempty"`

	// LastStartup is what the most recent recovery did. Present from the moment recovery
	// finishes until the process ends, because the page shows it once — on the first
	// status that says the daemon has started — and a report that vanished with the
	// banner could only be read by somebody already watching.
	LastStartup *StartupSummary `json:"last_startup,omitempty"`

	// Projects is every configured project, as a label and a badge.
	//
	// Here rather than fetched from /api/projects, for two reasons. The dashboard-only
	// share allowlists a handful of read paths, and the project switcher has to work
	// through it — a picker that renders unlabelled rows for a remote viewer is a
	// different page for no reason. And it is one payload the page already polls
	// instead of a second request whose failure mode is a list that fills in late.
	//
	// Labels and badges only. Nothing here says what a project builds, and nothing
	// here is a secret.
	Projects []StatusProject `json:"projects"`
}

// StatusProgress stages, in the order recovery runs them. Exported because the
// dashboard names them and a test asserts the sequence.
const (
	// StageReaping has no count. The exposer deletes what it finds behind one Reap
	// call and reports a total only when it returns, so a fabricated denominator here
	// would be a number the operator could watch and not believe.
	StageReaping   = "reaping"
	StageRestoring = "restoring"
	StageHistory   = "history"
)

// StartupSummary is what recovery did, kept after it finishes.
//
// Shown once, when the dashboard notices the daemon has started. Startup is the one
// event nobody can watch the middle of — it takes minutes, and anybody who reloads
// during it sees a banner and no detail — so the interesting moment for a report is
// immediately after.
type StartupSummary struct {
	// Seconds the whole of recovery took, reap and republish and backfill.
	Seconds int `json:"seconds"`

	// Instance ties this report to one process, so a dashboard left open across two
	// restarts does not re-announce the older one.
	Instance string `json:"instance"`

	Previews        int `json:"previews"`
	AdoptedPreviews int `json:"adopted_previews"`
	CreatedPreviews int `json:"created_previews"`
	AdoptedBuilds   int `json:"adopted_builds"`
	CreatedBuilds   int `json:"created_builds"`

	// Orphans is what the database no longer claimed, and so was deleted.
	Orphans int `json:"orphans"`

	// Pending is how many builds were waiting when the workers finally started. Not
	// zero after a restart that interrupted something, and the first thing to know.
	Pending int `json:"pending"`

	// Items is the same running commentary the banner showed, kept so the report can be
	// read by somebody who was not watching.
	Items []string `json:"items,omitempty"`
}

// StartupProgress is one stage of recovery, as the dashboard shows it.
//
// Note is a whole sentence written for somebody watching a URL fail to load, not a
// stage label — the question being answered is "why is nothing working yet", and
// "reaping" does not answer it.
type StartupProgress struct {
	Stage string `json:"stage"`
	Note  string `json:"note"`

	// Done and Total are omitted together when there is nothing countable. Total 0
	// means unknown, which the page renders as an indeterminate bar rather than as
	// "3 of 0".
	Done  int `json:"done"`
	Total int `json:"total"`

	// Items is what has been done so far, most recent last — "adopted docs-main-3fc1a0d",
	// "created docpreview-round-4". A stage and a count say how far along a four-minute
	// wait is and not what it is spending the time on, which is the question that gets
	// asked out loud.
	Items []string `json:"items,omitempty"`
}

// setStartup publishes the current recovery stage.
//
// Safe to call from the concurrent republish loop: each call swaps a fresh value in,
// so a racing pair produces one of the two, never a mix.
func (d *Daemon) setStartup(stage, note string, done, total int) {
	d.startup.Store(&StartupProgress{
		Stage: stage, Note: note, Done: done, Total: total,
		Items: d.startupItemsSnapshot(),
	})
}

// bumpStartup records one completed exposer round trip.
//
// Counted in round trips rather than in previews, and that is the whole point of it.
// Restoring two pull requests is eleven calls — one share per preview plus one per
// retained build — at ten to fifteen seconds each, so a bar reading "1 of 2" sat
// unchanged for minutes while eleven things happened behind it. Reported as the UI
// being broken, which is a fair reading of a progress indicator that does not move.
func (d *Daemon) bumpStartup() {
	done := int(d.startupDone.Add(1))
	d.setStartup(StageRestoring, restoringNote, done, d.startupTotal)
}

// restoringNote is the sentence the banner shows for the long stage. A package-level
// const because both the stage's opening call and every bump have to send the same
// text — a note that changes as the count rises looks like a different stage.
const restoringNote = "Republishing preview URLs from artifacts on disk."

// StatusProject is what the project switcher needs to draw one row.
//
// Key is `<platform>:<owner>/<repo>`, matching StatusPreview.Repo, so the page can join
// the two without parsing either.
type StatusProject struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Avatar string `json:"avatar,omitempty"`
}

// StatusPreview is one preview in the status payload.
type StatusPreview struct {
	PreviewID string    `json:"preview_id"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Branch    string    `json:"branch"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`

	// PRURL is where a human reads the pull request this preview is for.
	//
	// Composed here rather than stored, because it is a function of the platform, the
	// repository and the number — all three already in the row — and storing it would be
	// a fourth copy to keep in step. Empty for the local platform, which has no web
	// interface at all; the page renders a link only when it is set.
	PRURL string `json:"pr_url,omitempty"`
}

// Status summarizes the daemon's state.
func (d *Daemon) Status(ctx context.Context) (Status, error) {
	pending, err := d.store.PendingCount(ctx)
	if err != nil {
		return Status{}, err
	}
	previews, err := d.store.ListPreviews(ctx)
	if err != nil {
		return Status{}, err
	}

	queued, err := d.store.PendingJobs(ctx)
	if err != nil {
		return Status{}, err
	}

	d.mu.Lock()
	running := make(map[string]*build, len(d.running))
	for id, b := range d.running {
		running[id] = b
	}
	d.mu.Unlock()

	out := Status{
		Exposer:     d.exposer.Kind(),
		Instance:    d.instance,
		Starting:    d.starting.Load(),
		Startup:     d.startup.Load(),
		LastStartup: d.lastStartup.Load(),
		Pending:     pending,
		Running:     len(running),
		Events:      d.markOpenable(d.events.recent(60)),
	}

	// Best effort. The switcher degrades to repository names, which is what it showed
	// before projects had labels at all — and a status endpoint that fails because a
	// cosmetic lookup failed would take the whole dashboard down with it.
	if projects, err := d.store.ListProjects(ctx); err == nil {
		for _, p := range projects {
			label := p.DisplayName
			if label == "" {
				label = p.Repo
			}
			out.Projects = append(out.Projects, StatusProject{
				Key: p.Key(), Label: label, Avatar: p.Avatar,
			})
		}
	} else {
		d.log.Warn("listing projects for the status payload", "error", err)
	}

	// The stored rows are the committed history: a preview is written when a
	// build succeeds and left alone while the next one runs. Queued and
	// building are therefore not states the store ever holds, and reading it
	// alone reports every in-flight build as whatever it was last time —
	// or omits it entirely, on a branch that has never finished a build.
	//
	// Both facts live in memory, so the live view is composed here rather than
	// by writing transient states to disk. Persisting them instead would mean
	// a row saying "building" survives a crash forever, and would overwrite
	// the URL and reason of a preview that is still serving perfectly well.
	pendingByID := queuedByID(queued)

	seen := make(map[string]bool, len(previews))
	for _, p := range previews {
		seen[p.PreviewID] = true

		sp := StatusPreview{
			PreviewID: p.PreviewID,
			Repo:      p.PR.Repo.String(),
			Number:    p.PR.Number,
			Branch:    p.PR.Branch,
			Name:      p.Name,
			URL:       p.URL,
			State:     string(p.State),
			Reason:    p.Reason,
			UpdatedAt: p.UpdatedAt,
			PRURL:     p.PR.WebURL(),
		}

		// Building wins over queued: a push that supersedes a running build
		// cancels it and enqueues the replacement, so the same preview is
		// briefly both, and the interesting half is the one doing work.
		if b, ok := running[p.PreviewID]; ok {
			sp.State = string(scm.StateBuilding)
			sp.Branch = b.pr.Branch
			sp.Reason = ""
			sp.UpdatedAt = b.started
		} else if j, ok := pendingByID[p.PreviewID]; ok {
			sp.State = string(scm.StateQueued)
			sp.Branch = j.PR.Branch
			sp.Reason = ""
			// The enqueue time, not the stored row's. A rebuilt preview keeps the
			// timestamp of its last finished build, so without this a job queued
			// seconds ago rendered as hours old — the sibling branches above and
			// below both set this and only here was it missed.
			sp.UpdatedAt = j.EnqueuedAt
		}

		out.Previews = append(out.Previews, sp)
	}

	// Anything in flight with no stored row at all — the first build of a
	// branch, which is exactly the moment somebody is watching.
	for id, b := range running {
		if seen[id] {
			continue
		}
		seen[id] = true
		out.Previews = append(out.Previews, StatusPreview{
			PreviewID: id,
			Repo:      b.pr.Repo.String(),
			Number:    b.pr.Number,
			Branch:    b.pr.Branch,
			State:     string(scm.StateBuilding),
			UpdatedAt: b.started,
			PRURL:     b.pr.WebURL(),
		})
	}
	for _, j := range queued {
		id := j.PR.PreviewID()
		if seen[id] {
			continue
		}
		seen[id] = true
		out.Previews = append(out.Previews, StatusPreview{
			PreviewID: id,
			Repo:      j.PR.Repo.String(),
			Number:    j.PR.Number,
			Branch:    j.PR.Branch,
			State:     string(scm.StateQueued),
			// The enqueue time rather than time.Now(). Now() re-renders as "just
			// now" on every poll, which hides a job that has been stuck in the
			// queue — the one thing this row exists to reveal.
			UpdatedAt: j.EnqueuedAt,
			PRURL:     j.PR.WebURL(),
		})
	}

	return out, nil
}

func queuedByID(jobs []store.PendingJob) map[string]store.PendingJob {
	out := make(map[string]store.PendingJob, len(jobs))
	for _, j := range jobs {
		out[j.PR.PreviewID()] = j
	}
	return out
}

// replaceDir atomically-ish moves src over dst.
//
// os.Rename across filesystems fails, and the artifacts directory and the
// workspace can easily be on different volumes, so fall back to a copy. The
// destination is removed first either way: leaving a previous build's files
// behind would resurrect pages the pull request deleted.
func replaceDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("creating artifacts directory: %w", err)
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clearing previous artifacts: %w", err)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyDir(src, dst)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			// Symlinks in build output would let a preview serve files from
			// outside the artifacts directory.
			return nil
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := out.ReadFrom(in); err != nil {
			return err
		}
		return out.Close()
	})
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// Ensure the daemon satisfies the handler contract the HTTP layer expects.
var _ interface {
	Handle(context.Context, scm.Event) error
	Status(context.Context) (Status, error)
} = (*Daemon)(nil)

var _ http.Handler = (*preview.Site)(nil)
