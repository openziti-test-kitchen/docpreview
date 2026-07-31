package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/vault"
)

// apiVersion pins the REST API version. GitHub ships breaking changes behind
// this header, so naming a version is how the integration keeps working when
// they roll a new default.
const apiVersion = "2022-11-28"

// checkName is the name docpreview's check run appears under in the pull
// request's checks list. It doubles as the lookup key when updating a run,
// which is why it must not vary.
const checkName = "docpreview"

// Client talks to GitHub as an App installation.
type Client struct {
	cfg  config.GitHubConfig
	log  *slog.Logger
	auth *authenticator
	http *http.Client

	webhookSecret vault.Secret

	// comments maps a preview ID to the comment carrying its marker.
	//
	// A cache over findComment, and the reason is correctness rather than speed:
	// GitHub's list-comments endpoint does not return a comment created moments
	// earlier, so two reports in quick succession both concluded there was none
	// and created one each. See upsertComment.
	//
	// Deliberately in memory only. A persisted ID outlives the comment it names
	// and turns every later update into a 404; the marker search is what makes a
	// cold start correct.
	commentsMu sync.Mutex
	comments   map[string]int64
}

func (c *Client) knownComment(previewID string) (int64, bool) {
	c.commentsMu.Lock()
	defer c.commentsMu.Unlock()
	id, ok := c.comments[previewID]
	return id, ok
}

func (c *Client) rememberComment(previewID string, id int64) {
	c.commentsMu.Lock()
	defer c.commentsMu.Unlock()
	if c.comments == nil {
		c.comments = map[string]int64{}
	}
	c.comments[previewID] = id
}

func (c *Client) forgetComment(previewID string) {
	c.commentsMu.Lock()
	defer c.commentsMu.Unlock()
	delete(c.comments, previewID)
}

// New builds a GitHub client from configuration and vault contents.
func New(cfg config.GitHubConfig, v *vault.Vault, log *slog.Logger) (*Client, error) {
	pem, err := v.MustGet(vault.KeyGitHubPrivateKey)
	if err != nil {
		return nil, err
	}
	secret, err := v.MustGet(vault.KeyGitHubWebhookSec)
	if err != nil {
		return nil, err
	}

	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")

	hc := &http.Client{Timeout: 30 * time.Second}
	auth, err := newAuthenticator(cfg.AppID, cfg.APIBase, pem, hc)
	if err != nil {
		return nil, err
	}

	return &Client{
		cfg:           cfg,
		log:           log.With("scm", "github"),
		auth:          auth,
		http:          hc,
		webhookSecret: secret,
	}, nil
}

func (c *Client) Platform() model.Platform { return model.PlatformGitHub }

// Validate confirms the App credentials work, by asking GitHub who we are.
func (c *Client) Validate(ctx context.Context) error {
	appJWT, err := c.auth.appJWT()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIBase+"/app", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub rejected the App credentials "+
			"(check github.app_id and the private key in the vault): %w", errorFromResponse(resp))
	}

	var app struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return fmt.Errorf("decoding App identity: %w", err)
	}
	c.log.Info("github app validated", "app", app.Name, "slug", app.Slug, "app_id", c.cfg.AppID)
	return nil
}

// OpenPullRequests lists the open pull requests of a repository, as pull requests this
// daemon could build.
//
// It exists because a project added from the dashboard has no webhook behind it. Every
// other path into a build starts with a delivery, which carries the installation id, the
// branch and the head sha; adding a project carries none of that, so a newly added
// repository sat there doing nothing until somebody happened to push. Asking GitHub what
// is already open is the difference between "added" and "working".
//
// The installation id is looked up rather than supplied, for the same reason: there is no
// delivery to take it from. Every returned pull request carries it, so the results are
// usable by exactly the same pipeline a webhook feeds.
//
// Forks are dropped here rather than deep in the build, matching what the webhook path
// does and for the same reason: building a fork's branch runs its author's code on this
// host. A fork with a null head repository — a deleted fork — is dropped too, which is
// the conservative reading of the same gap the webhook path documents.
func (c *Client) OpenPullRequests(ctx context.Context, repo model.Repo) ([]model.PullRequest, error) {
	installationID, err := c.installationFor(ctx, repo)
	if err != nil {
		return nil, err
	}

	const perPage = 100
	var out []model.PullRequest
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=%d&page=%d",
			repo.Owner, repo.Name, perPage, page)

		var pulls []struct {
			Number int  `json:"number"`
			Draft  bool `json:"draft"`
			Head   struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		if err := c.do(ctx, installationID, http.MethodGet, path, nil, &pulls); err != nil {
			return nil, fmt.Errorf("listing open pull requests on %s/%s: %w", repo.Owner, repo.Name, err)
		}

		want := repo.Owner + "/" + repo.Name
		for _, p := range pulls {
			if p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, want) {
				c.log.Info("skipping a pull request from a fork",
					"repo", want, "pr", p.Number)
				continue
			}
			out = append(out, model.PullRequest{
				Repo:           repo,
				Number:         p.Number,
				Branch:         p.Head.Ref,
				BaseBranch:     p.Base.Ref,
				HeadSHA:        p.Head.SHA,
				InstallationID: installationID,
			})
		}
		if len(pulls) < perPage {
			return out, nil
		}
	}
}

// DefaultBranch names the repository's default branch and the commit at its tip.
//
// Two calls, because GitHub's repository object carries the branch's *name* and not what it
// points at. `GET /repos/{o}/{r}` answers the first and `BranchTip` the second.
//
// The branch is read rather than assumed. A repository whose default branch is `master`
// would otherwise get a preview of a branch that does not exist, which fails at the clone
// with git's own message about a missing ref — accurate and about the wrong thing.
func (c *Client) DefaultBranch(ctx context.Context, repo model.Repo) (string, string, error) {
	installationID, err := c.installationFor(ctx, repo)
	if err != nil {
		return "", "", err
	}

	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("/repos/%s/%s", repo.Owner, repo.Name)
	if err := c.do(ctx, installationID, http.MethodGet, path, nil, &out); err != nil {
		return "", "", fmt.Errorf("reading %s/%s: %w", repo.Owner, repo.Name, err)
	}
	if out.DefaultBranch == "" {
		// An empty repository has no default branch to report. Saying so beats returning
		// a blank name that fails later as a clone error about a missing ref.
		return "", "", fmt.Errorf("%s/%s reports no default branch; is it empty?",
			repo.Owner, repo.Name)
	}

	commit, err := c.BranchTip(ctx, repo, out.DefaultBranch)
	if err != nil {
		return "", "", err
	}
	return out.DefaultBranch, commit, nil
}

// BranchTip is the commit a branch currently points at.
//
// `GET /repos/{o}/{r}/branches/{branch}` rather than the git-refs endpoint: it takes the
// branch name unencoded in the path, and a 404 from it means "no such branch" rather than
// the empty list a refs query returns for one.
func (c *Client) BranchTip(ctx context.Context, repo model.Repo, branch string) (string, error) {
	installationID, err := c.installationFor(ctx, repo)
	if err != nil {
		return "", err
	}

	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	// Escaped, because a branch name legitimately contains slashes — `release/8.2` would
	// otherwise address a path two segments deep and 404.
	path := fmt.Sprintf("/repos/%s/%s/branches/%s",
		repo.Owner, repo.Name, url.PathEscape(branch))
	if err := c.do(ctx, installationID, http.MethodGet, path, nil, &out); err != nil {
		return "", fmt.Errorf("reading branch %s on %s/%s: %w", branch, repo.Owner, repo.Name, err)
	}
	if out.Commit.SHA == "" {
		return "", fmt.Errorf("branch %s on %s/%s reports no commit", branch, repo.Owner, repo.Name)
	}
	return out.Commit.SHA, nil
}

// installationOf is the installation to act as for this pull request, looking it up when the
// pull request does not carry one.
//
// `InstallationID` is documented as "the installation that delivered the event", and for a
// webhook that is exactly right — the id is in the payload and no lookup is needed. But not
// every build comes from a delivery: a project scan, a linked pull request and a branch
// preview are all started by an operator, and a zero id there is not an error, it is the
// absence of a webhook.
//
// This existed as a bug rather than as a decision. Branch previews built a pull request with
// no id and every GitHub one failed at the clone with "the webhook payload was missing
// installation.id" — a message about a webhook, for a build that never had one, which is
// exactly the kind of error that sends somebody to read delivery logs that do not exist.
//
// One extra App-authenticated call in that case, and none in the webhook case.
func (c *Client) installationOf(ctx context.Context, pr model.PullRequest) (int64, error) {
	if pr.InstallationID != 0 {
		return pr.InstallationID, nil
	}
	return c.installationFor(ctx, pr.Repo)
}

// installationFor finds the installation covering a repository.
//
// `GET /repos/{owner}/{repo}/installation` is authenticated as the *App*, not as an
// installation — it is how an App asks "am I installed here, and under which id". A 404
// is the useful answer rather than an error condition to swallow: it means the App is not
// installed on that repository, which is the commonest reason a project somebody just
// added will never build, and the message says so.
func (c *Client) installationFor(ctx context.Context, repo model.Repo) (int64, error) {
	appJWT, err := c.auth.appJWT()
	if err != nil {
		return 0, err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/installation", c.cfg.APIBase, repo.Owner, repo.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reaching GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("the GitHub App is not installed on %s/%s; install it there "+
			"(GitHub → Settings → GitHub Apps → your app → Configure) and try again",
			repo.Owner, repo.Name)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("looking up the installation for %s/%s: %w",
			repo.Owner, repo.Name, errorFromResponse(resp))
	}

	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decoding the installation: %w", err)
	}
	if body.ID == 0 {
		return 0, fmt.Errorf("GitHub reported no installation id for %s/%s", repo.Owner, repo.Name)
	}
	return body.ID, nil
}

// CloneURL returns an HTTPS clone URL carrying a short-lived installation
// token.
//
// GitHub's documented form is https://x-access-token:<token>@github.com/o/r.git.
// The token is URL-escaped because installation tokens are opaque and nothing
// promises they avoid characters that would terminate the userinfo section.
//
// The returned string is a credential. It goes straight into a git command's
// argument list and nowhere else — never a log line, never an error message,
// never a comment.
func (c *Client) CloneURL(ctx context.Context, pr model.PullRequest) (string, error) {
	installationID, err := c.installationOf(ctx, pr)
	if err != nil {
		return "", err
	}
	token, err := c.auth.installationToken(ctx, installationID)
	if err != nil {
		return "", err
	}

	base := "https://github.com"
	if c.cfg.APIBase != "https://api.github.com" {
		// GitHub Enterprise: the API lives at https://host/api/v3, and the git
		// endpoints at https://host.
		if u, err := url.Parse(c.cfg.APIBase); err == nil {
			base = u.Scheme + "://" + u.Host
		}
	}

	return fmt.Sprintf("%s://x-access-token:%s@%s/%s/%s.git",
		schemeOf(base), url.QueryEscape(token), hostOf(base),
		pr.Repo.Owner, pr.Repo.Name), nil
}

func schemeOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	return "https"
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return "github.com"
}

// maxChangedFilePages bounds the changed-file walk.
//
// GitHub caps the pull request files endpoint at 3000 entries anyway, and a
// documentation change that touches thirty pages of files is not one this tool
// needs to classify precisely — at that size the answer is "yes, build it".
const maxChangedFilePages = 30

// ChangedFiles lists the files the pull request touches.
func (c *Client) ChangedFiles(ctx context.Context, pr model.PullRequest) ([]string, error) {
	const perPage = 100
	var files []string

	installationID, err := c.installationOf(ctx, pr)
	if err != nil {
		return nil, err
	}

	for page := 1; page <= maxChangedFilePages; page++ {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=%d&page=%d",
			pr.Repo.Owner, pr.Repo.Name, pr.Number, perPage, page)

		var batch []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := c.do(ctx, installationID, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("listing changed files on %s: %w", pr, err)
		}
		for _, f := range batch {
			files = append(files, f.Filename)
			// A rename out of docs/ is a documentation change even though the
			// new path may not match any doc glob.
			if f.PreviousFilename != "" {
				files = append(files, f.PreviousFilename)
			}
		}
		if len(batch) < perPage {
			return files, nil
		}
	}

	c.log.Warn("changed-file list truncated", "pr", pr.String(), "pages", maxChangedFilePages)
	return files, nil
}

// Publish writes the report to the pull request: one comment, edited in place,
// plus a check run so the preview also shows in the PR's status list.
//
// The comment is the durable artifact and the check run is the convenience, so
// a failure to write the check run is logged rather than returned. Losing the
// status line is a cosmetic problem; losing the comment means the reviewer
// never learns the preview exists.
func (c *Client) Publish(ctx context.Context, r scm.Report) error {
	if err := c.upsertComment(ctx, r); err != nil {
		return err
	}
	if err := c.upsertCheckRun(ctx, r); err != nil {
		c.log.Warn("could not write check run", "pr", r.PR.String(), "error", err)
	}
	return nil
}

// upsertComment posts docpreview's comment, or edits the one already there.
//
// The comment ID is remembered per preview for the life of the process, and the
// marker search is the fallback. Both halves are needed.
//
// Searching alone is not enough: GitHub's list-comments endpoint is not
// read-your-writes consistent. A `queued` report created a comment and the
// `building` report 182 ms later listed the comments, did not see it, and created
// a second — leaving one comment stranded on "Building" forever while the other
// went on updating. Any two reports close together hit that window, so moving
// where reports are emitted would hide this rather than fix it.
//
// Remembering alone is not enough either: the map is process-local, so a restart
// or a rebuilt database must still be able to find the comment. That is what the
// marker is for, and why the ID is never persisted — a stored ID that outlives
// the comment produces a 404 on every update, where a marker search produces the
// right answer or an honest miss.
func (c *Client) upsertComment(ctx context.Context, r scm.Report) error {
	body := scm.RenderComment(r)

	existing, ok := c.knownComment(r.PreviewID)
	if !ok {
		var err error
		existing, err = c.findComment(ctx, r.PR, r.PreviewID)
		if err != nil {
			return err
		}
		if existing != 0 {
			c.rememberComment(r.PreviewID, existing)
		}
	}

	if existing == 0 {
		return c.createComment(ctx, r, body)
	}

	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", r.PR.Repo.Owner, r.PR.Repo.Name, existing)
	err := c.do(ctx, r.PR.InstallationID, http.MethodPatch, path, map[string]string{"body": body}, nil)

	// The comment is gone — somebody deleted it. Forget it and post a new one,
	// rather than 404ing on this and every later report for the rest of the
	// process. Once, because a 404 on the freshly created comment would mean
	// something is wrong that a second attempt will not fix.
	if IsNotFound(err) {
		c.log.Warn("the preview comment was deleted; posting a new one",
			"pr", r.PR.String(), "comment", existing)
		c.forgetComment(r.PreviewID)
		return c.createComment(ctx, r, body)
	}
	if err != nil {
		return fmt.Errorf("updating preview comment on %s: %w", r.PR, err)
	}
	c.log.Debug("updated preview comment", "pr", r.PR.String(), "comment", existing, "state", r.State)
	return nil
}

// createComment posts a new preview comment and remembers its ID.
func (c *Client) createComment(ctx context.Context, r scm.Report, body string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", r.PR.Repo.Owner, r.PR.Repo.Name, r.PR.Number)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, r.PR.InstallationID, http.MethodPost, path,
		map[string]string{"body": body}, &created); err != nil {
		return fmt.Errorf("creating preview comment on %s: %w", r.PR, err)
	}
	c.rememberComment(r.PreviewID, created.ID)
	c.log.Info("created preview comment", "pr", r.PR.String(), "comment", created.ID, "state", r.State)
	return nil
}

// findComment locates docpreview's comment by its hidden marker, returning 0 if
// there is none.
//
// This walks every page of comments, which on a long-running pull request is
// several requests. The alternative — remembering comment IDs in our database —
// breaks the moment the database is rebuilt or the service is reinstalled, and
// the failure mode is duplicate comments on every open PR. Paying a few reads
// to keep the comment self-identifying is the better trade.
// Matched with scm.HasMarker rather than against one rendered marker string, so a
// comment written in either marker style is found. A daemon that recognised only the
// style it currently writes would post a second comment on every open pull request
// the first time that style changed.
func (c *Client) findComment(ctx context.Context, pr model.PullRequest, previewID string) (int64, error) {
	const perPage = 100
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=%d&page=%d",
			pr.Repo.Owner, pr.Repo.Name, pr.Number, perPage, page)

		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		if err := c.do(ctx, pr.InstallationID, http.MethodGet, path, nil, &comments); err != nil {
			return 0, fmt.Errorf("listing comments on %s: %w", pr, err)
		}
		for _, cm := range comments {
			if scm.HasMarker(cm.Body, previewID) {
				return cm.ID, nil
			}
		}
		if len(comments) < perPage {
			return 0, nil
		}
	}
}

// upsertCheckRun creates or updates the check run for the head commit.
//
// Check runs are keyed by (name, head_sha), and a new commit gets a new run —
// which is correct, since the old commit's result should stay attached to the
// old commit. Within one commit, we look up the existing run by name so that
// queued → building → ready updates one row instead of stacking three.
func (c *Client) upsertCheckRun(ctx context.Context, r scm.Report) error {
	if r.Commit == "" {
		return nil
	}

	payload := map[string]any{
		"name":     checkName,
		"head_sha": r.Commit,
		"status":   checkStatus(r.State),
		"output": map[string]string{
			"title":   checkTitle(r),
			"summary": scm.RenderComment(r),
		},
	}
	if concl := checkConclusion(r.State); concl != "" {
		payload["conclusion"] = concl
		payload["completed_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	if r.State == scm.StateReady && r.URL != "" {
		payload["details_url"] = r.URL
	}

	existing, err := c.findCheckRun(ctx, r.PR, r.Commit)
	if err != nil {
		return err
	}
	if existing == 0 {
		path := fmt.Sprintf("/repos/%s/%s/check-runs", r.PR.Repo.Owner, r.PR.Repo.Name)
		return c.do(ctx, r.PR.InstallationID, http.MethodPost, path, payload, nil)
	}

	// head_sha is immutable on an existing run and GitHub rejects it as an
	// update field.
	delete(payload, "head_sha")
	path := fmt.Sprintf("/repos/%s/%s/check-runs/%d", r.PR.Repo.Owner, r.PR.Repo.Name, existing)
	return c.do(ctx, r.PR.InstallationID, http.MethodPatch, path, payload, nil)
}

func (c *Client) findCheckRun(ctx context.Context, pr model.PullRequest, sha string) (int64, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?check_name=%s&per_page=100",
		pr.Repo.Owner, pr.Repo.Name, sha, url.QueryEscape(checkName))

	var out struct {
		CheckRuns []struct {
			ID int64 `json:"id"`
		} `json:"check_runs"`
	}
	if err := c.do(ctx, pr.InstallationID, http.MethodGet, path, nil, &out); err != nil {
		return 0, fmt.Errorf("listing check runs for %s: %w", sha, err)
	}
	if len(out.CheckRuns) == 0 {
		return 0, nil
	}
	return out.CheckRuns[0].ID, nil
}

func checkStatus(s scm.State) string {
	switch s {
	case scm.StateQueued:
		return "queued"
	case scm.StateBuilding:
		return "in_progress"
	default:
		return "completed"
	}
}

func checkConclusion(s scm.State) string {
	switch s {
	case scm.StateReady:
		return "success"
	case scm.StateSkipped:
		return "neutral"
	case scm.StateFailed:
		return "failure"
	default:
		return ""
	}
}

func checkTitle(r scm.Report) string {
	switch r.State {
	case scm.StateQueued:
		return "Preview queued"
	case scm.StateBuilding:
		return "Building preview"
	case scm.StateReady:
		return "Preview ready"
	case scm.StateSkipped:
		return "No documentation changes"
	case scm.StateFailed:
		return "Preview build failed"
	default:
		return "Preview"
	}
}

// Retract deletes docpreview's comment. The check run is left alone: it records
// what happened to a specific commit, and erasing that history when a pull
// request closes would be revisionist.
func (c *Client) Retract(ctx context.Context, pr model.PullRequest) error {
	previewID := pr.PreviewID()

	// Forget it either way. A preview that is torn down and later rebuilt must not
	// PATCH the comment this is about to delete, and dropping the entry before the
	// request means a failed delete still leaves the next report searching rather
	// than reusing an ID that may be gone.
	defer c.forgetComment(previewID)

	existing, ok := c.knownComment(previewID)
	if !ok {
		var err error
		existing, err = c.findComment(ctx, pr, previewID)
		if err != nil {
			return err
		}
	}
	if existing == 0 {
		return nil
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", pr.Repo.Owner, pr.Repo.Name, existing)
	if err := c.do(ctx, pr.InstallationID, http.MethodDelete, path, nil, nil); err != nil {
		// Already gone is the outcome this wanted.
		if IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting preview comment on %s: %w", pr, err)
	}
	return nil
}

// rateLimitAttempts bounds how many times a rate-limited request is repeated.
//
// Three, because the wait comes from GitHub rather than from us: a secondary
// limit typically asks for tens of seconds, and the build timeout is the real
// ceiling. More attempts would sit in a loop the operator cannot see; fewer
// would fail a burst that was going to clear on the first wait.
const rateLimitAttempts = 3

// rateLimitFallback is the wait when GitHub sends no header saying.
const rateLimitFallback = 5 * time.Second

// do issues an authenticated REST request against GitHub, retrying the two
// failures that are worth retrying.
//
// **A 401, exactly once, with a fresh installation token.** A cached token can be
// revoked without expiring — a permissions change, a suspension, a reinstall —
// and the cache has no way to learn that except by being told no. Retrying once
// turns an hour of every request failing into one extra round trip; retrying more
// than once would loop on a genuine auth failure, which is why that one is a flag
// and not a counter.
//
// **A rate limit, up to rateLimitAttempts times, waiting as long as GitHub asks.**
// This is the half that was missing: APIError has carried RateLimited and
// RetryAfter and a Retryable method since it was written, and nothing called
// them, so a burst rejection went straight to a failed build and a comment stuck
// on "Building".
//
// A 5xx is deliberately **not** retried even though Retryable says it could be.
// A rate limit is refused before GitHub acts, so repeating it cannot repeat an
// effect; a 500 may well have created the comment before failing to say so, and
// the comment upsert finds-then-creates, so a retry there posts a second comment.
// Making 5xx safe means idempotency keys or a re-find before every retry, and
// that belongs with the upsert rather than in the transport.
func (c *Client) do(ctx context.Context, installationID int64, method, path string, in, out any) error {
	triedFreshToken := false

	for attempt := 1; ; attempt++ {
		err := c.doOnce(ctx, installationID, method, path, in, out)

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return err
		}

		switch {
		case apiErr.StatusCode == http.StatusUnauthorized && !triedFreshToken:
			triedFreshToken = true
			c.auth.invalidate(installationID)
			c.log.Warn("github rejected the installation token; minting a new one and retrying once",
				"installation", installationID, "method", method, "path", path)

		case apiErr.RateLimited && attempt < rateLimitAttempts:
			wait := apiErr.RetryAfter
			if !apiErr.retryAfterKnown {
				wait = rateLimitFallback
			}
			c.log.Warn("github rate limited this request; waiting",
				"method", method, "path", path, "wait", wait, "attempt", attempt)
			if err := sleepCtx(ctx, wait); err != nil {
				// The context died first. Report the rate limit rather than the
				// cancellation: the limit is what the operator has to act on.
				return errors.Join(err, apiErr)
			}

		default:
			return err
		}
	}
}

// sleepCtx waits for d, or returns early if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) doOnce(ctx context.Context, installationID int64, method, path string, in, out any) error {
	token, err := c.auth.installationToken(ctx, installationID)
	if err != nil {
		return err
	}

	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBase+path, body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %w", method, path, errorFromResponse(resp))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// APIError is a non-2xx response from GitHub.
type APIError struct {
	Status     string
	StatusCode int
	Message    string
	// RateLimited reports whether this was a rate-limit rejection, which the
	// queue treats as retryable where a 404 is not.
	RateLimited bool
	RetryAfter  time.Duration

	// retryAfterKnown separates "GitHub said zero" from "GitHub said nothing".
	// Both leave RetryAfter at zero and they mean opposite things: the first is
	// "retry now", the second is "you decide", and only the second wants a
	// fallback delay. Collapsing them made every explicit zero wait five
	// seconds for no reason.
	retryAfterKnown bool
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return e.Status + ": " + e.Message
}

// Retryable reports whether repeating the request could plausibly succeed.
func (e *APIError) Retryable() bool {
	return e.RateLimited || e.StatusCode >= 500
}

func errorFromResponse(resp *http.Response) error {
	// Read a bounded amount: GitHub's error bodies are small, but a proxy in
	// between might not be.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	apiErr := &APIError{Status: resp.Status, StatusCode: resp.StatusCode}

	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Message != "" {
		apiErr.Message = parsed.Message
	} else if len(raw) > 0 {
		apiErr.Message = strings.TrimSpace(string(raw))
	}

	// GitHub signals a rate limit three ways, and only the first was recognised
	// here originally.
	//
	// The primary limit is 403 or 429 with a remaining count of zero. Treating
	// every 403 as retryable would spin forever on a genuine permissions
	// problem, which is the more common cause of one.
	//
	// A secondary rate limit — the one that fires on burst rather than on
	// volume, and the one a supersede storm is most likely to hit — is a 403
	// with a *non-zero* remaining count, identified by `Retry-After` or by the
	// phrase in the message. Classifying that as a permissions error, which is
	// what the remaining-count test alone does, turns a wait-and-retry into a
	// build that fails and a comment stuck on "Building".
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		apiErr.RateLimited = true
	case resp.StatusCode == http.StatusForbidden &&
		(resp.Header.Get("Retry-After") != "" || isSecondaryLimitMessage(apiErr.Message)):
		apiErr.RateLimited = true
	}

	if apiErr.RateLimited {
		apiErr.RetryAfter, apiErr.retryAfterKnown = retryAfterFrom(resp.Header)
	}
	return apiErr
}

// isSecondaryLimitMessage recognises GitHub's secondary-limit wording.
//
// A string match on an error message is a poor signal and is the fallback, not
// the primary test: `Retry-After` is checked first and is what GitHub documents
// sending. This catches the case where it is absent, because the alternative is
// classifying a burst rejection as a permissions failure and giving up.
func isSecondaryLimitMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "secondary rate limit")
}

// retryAfterFrom works out how long to wait before trying again.
//
// Two headers, in this order. `Retry-After` is what GitHub sends with a
// secondary limit and is authoritative when present. `X-RateLimit-Reset` covers
// the primary limit and is **epoch seconds** — it was previously parsed as
// RFC3339, which never matches, so every rate-limited response reported a zero
// wait and any caller acting on it would have retried immediately into the same
// limit.
//
// The second return says whether GitHub actually told us. A zero duration with
// ok true means "retry now"; a zero duration with ok false means "no idea", and
// only the second wants a caller-chosen fallback.
func retryAfterFrom(h http.Header) (time.Duration, bool) {
	if v := h.Get("Retry-After"); v != "" {
		// Delay-seconds is the form GitHub uses. The HTTP-date form is legal and
		// is handled second because a proxy could rewrite it.
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
		if ts, err := http.ParseTime(v); err == nil {
			return max(time.Until(ts), 0), true
		}
	}

	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if epoch, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return max(time.Until(time.Unix(epoch, 0)), 0), true
		}
	}
	return 0, false
}

// IsNotFound reports whether err is a GitHub 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
