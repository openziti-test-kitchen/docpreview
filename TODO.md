# TODO

Live list of what is outstanding. Ordered roughly by what would be picked up next.

Related: `www/docs/future/ziti-native-previews.md` holds the design research that is not yet a feature.

**Wanted next, out of order:** [the log pane not tailing a build that starts while you are
watching](#the-log-pane-still-does-not-start-tailing-a-build-that-begins-while-you-are-watching). It is under
Verification gaps because the first deliverable is a test, but it is the most-reported defect in the dashboard and
three attempts to diagnose it by reading the code were all wrong.

## In flight

**Branch previews, and a permanent one for the default branch. Shipped and exercised live**, 31 July 2026: `main`
previews for `bitbucket:netfoundry/customer-connect-docs` and `github:openziti-test-kitchen/docpreview`, both
serving 200, neither commenting on anything.

Every preview used to belong to a pull request, which is the wrong shape for the thing an operator looks at most:
the current state of `main`. A branch preview differs from a pull request's in four ways, and each is enforced
somewhere different:

- **No comment, ever** — `d.report` returns early for `IsBranch`, which is the funnel every state change passes
  through, and `teardown` skips `Retract` for the same reason.
- **Not torn down by a closed pull request**, because the ids cannot collide: a branch preview hashes the branch
  where a pull request hashes its number.
- **Not reaped for being old.** The TTL exists because a pull request's preview outlives its usefulness; `main` is
  still `main` after a quiet fortnight.
- **Rebuilt at the branch tip**, not at the commit on the row — the opposite of a pull request's rebuild, because
  this preview is a claim about what the branch looks like now.

- [x] **A preview identity that is not a pull request number.** Number 0 means "no pull request", and
      `PreviewID()` folds the branch into the hash only in that case — `TestPreviewIDIsStableForPullRequests` pins
      the numbered form with a literal, because changing it would orphan every stored row and remote share.
- [x] **Build the default branch when a project is added**, discovered rather than assumed: `scm.BranchResolver`
      with `DefaultBranch` and `BranchTip` on both platforms. A repository on `master` gets `master`.
      `backfillBranchPreviews` covers projects that predate the feature, on every startup.
- [x] **A build with no webhook behind it can authenticate.** Found by the live test: every GitHub branch build
      failed with "the webhook payload was missing installation.id" — a message about a webhook, for a build that
      never had one. `installationOf` looks the installation up when the pull request carries none, which also
      covers the scan and link paths.
- [ ] **Decide what happens on a push to the default branch.** A push delivery is not a pull request event and is
      ignored entirely, so the permanent preview goes stale until somebody presses Rebuild or the daemon restarts.
      Handling `push` for the default branch only is the smallest version, and it is what makes "always current"
      true rather than "current as of the last time somebody asked".
- [ ] **A failed branch preview is not retried.** `backfillBranchPreviews` only builds where a preview is *absent*,
      deliberately — rebuilding a broken one on every restart would hide it. But a permanent preview stuck failed
      is exactly the state that most wants attention, and nothing surfaces it beyond the project card.

**The log pane now follows a running build selected from the picker. Fixed**, 31 July 2026, and covered by
`tools/dashboardtest/logtail.mjs` — six scenarios, including the two that were broken.

Choosing the in-flight build from the dropdown used to show its log to the point it had reached and then stop
updating, and a build that started while the row was open did not appear in the picker until a reload.

**Both entries here previously described the wrong causes**, which is worth leaving on the record: the plausible
diagnosis and the real one were different, and the harness is what told them apart.

- [x] **The pane was streaming and not following.** The guess was that selecting the newest build by id fetched the
      stored file instead of streaming. It did not — index 0 of the picker already carries the empty value, so a
      stream did open and lines did arrive. What was wrong is that `setFollow(false)`, the deliberate pause a
      *stored* build sets, leaked into that path: the viewport stayed where it was while output piled up below the
      fold, which looks exactly like a stream that stopped. `liveBuild()` now decides, and it is true only for
      index 0 with a live state — not "whenever something is running", because `/logs/{preview}/stream` serves
      whichever build is current and streaming for any other id would show one build's output under another's label.
- [x] **The picker froze while it had focus.** The guess was that `loadBuilds` never ran on a status tick. It does,
      on every render of an open row. The real cause: `updateBuildPicker` correctly refuses to rewrite a `<select>`
      that has focus — doing so closes an open dropdown — and a `<select>` keeps focus after you pick from it. The
      preview picker has an `onblur` catch-up and the build picker had none, so a reload was the only way out.
- [ ] **`updateBody`'s handler closures capture a stale group.** Found while fixing the above: the build picker's
      `onchange` writes `ui.build[<the preview at expand time>]`, so switching the preview dropdown and then the
      build dropdown records the selection against the wrong preview. Avoided rather than fixed in the blur handler.

**Secrets have a scope now, and the projects page is where they live.** `build.secrets` was one map for the whole
daemon, which is the wrong shape for the case it exists for: a documentation site assembling several private
repositories needs a token per source, and those are not the same for every project. A single global map also means
every project's build can read every other project's tokens.

- [x] **Project-scoped environment variables.** `project/<platform>/<owner>/<repo>/<ENV>` in the same vault,
      `PUT /api/projects/{platform}/{owner}/{repo}/secrets/{env}`, resolved per build through
      `Daemon.SetProjectSecrets` and merged by `Builder.WithSecrets` — which rebuilds the redactor in the same call,
      because the two must never be separable. Written up in
      [docs/design/05-secrets.md](docs/design/05-secrets.md) and [www/docs/reference/projects.md](www/docs/reference/projects.md).
- [x] **The projects page is usable.** Cards with label-over-value facts, the add form behind a button, edit in
      place, and a secrets panel per project. Two real bugs fell out of it: `[hidden]` loses to `.wrap`'s
      `display: grid`, so the previews and activity sections rendered empty under both admin pages; and `run()`
      reported failures into `#setup-body`, hidden on `/projects`, so a failed save did nothing and said nothing.
      Covered by `tools/dashboardtest/projects.mjs`.
- [ ] **Non-secret project env.** A project can hold credentials but not plain values like `BB_USERNAME`. Today
      those go in the repository's `.docpreview.yml`, which is right for anything not secret — but the split will
      confuse somebody, and the same panel could hold both if the secret ones stayed write-only.
- [ ] **Report which variables a build actually used.** The build log is redacted, so a token that was set but never
      read looks identical to one that was missing. The build script's own "🔑 Using X" lines are the only signal,
      and they are the script's, not ours.
- [ ] **A project's secrets survive its deletion.** `DELETE /api/projects/...` removes the row and leaves
      `project/<platform>/<owner>/<repo>/*` in the vault. Deliberate for now — deleting credentials as a side effect
      of removing a build config is not obviously right, and re-adding the project gets them back — but nothing says
      so on the page, and a vault that accumulates unreachable entries is the audit gap widened again.

**A share that exists before the build does.** Today a share is created after a successful build, so the first
push to a branch gives a reviewer `share ... not found!` until it finishes, and a restart briefly 404s everything.

The fix is a handler that can be swapped rather than a share that gets replaced. `Publish` takes an
`http.Handler`, so publish a `switchable` holding an atomic pointer:

- **Queued or building** — it serves a status page: which commit, how long, a spinner, and a poll against
  `/_docpreview/status` on its own origin. Same share, so same origin, so no CORS and no daemon URL to know.
- **Ready** — the pointer is swapped to the built site. The page's poll sees `ready` and reloads itself into it.
- **Failed** — it serves the failure and the log excerpt, which is more use than a 404.

Three things this fixes beyond the missing page. Publishing a name is destructive, so a rebuild currently
withdraws a live share and takes the name again — with a swap there is nothing to withdraw and the supersede race
that `commitLock` exists for gets narrower. A restart republishes once per preview instead of once per preview
*and* once per build. And the reviewer gets a URL that is safe to paste into a review before the build lands.

- [ ] `switchable` handler, and `Publish` called at enqueue rather than at commit.
- [ ] `/_docpreview/status` on the placeholder, and the poll-and-reload page. Reserved prefix so it cannot
      collide with a documentation route.
- [ ] **Decide what a failed first build should hold.** Publishing at enqueue means a name is taken by a branch that
      may never build, and the name is the quota-bearing object. Teardown releases it now, so the cost is bounded by
      how long an abandoned branch stays open rather than being permanent. Serving the failure is the useful answer.
- [ ] Keep the artifact check. `errArtifactsUnusable` drops a preview whose stored `base_url` no longer matches
      the exposer, and a swap must not skip it — serving a site whose every asset 404s is worse than a placeholder.

**A share per build, not just per branch. Shipped; what is left is the accounting around it.** One preview used to
own one share, always serving the newest successful build, and the consequence showed up on the dashboard: the log
pane could be reading `build 20260729-190307-85912e2` while the Open button next to it went to whatever was
currently published. Two different builds described side by side, and no way to look at the older one at all.

Now a preview has a share per commit built **and** one for the branch. Five builds of a branch means six shares — the
branch share following the newest, plus five pinned to their commit. Written up in
[docs/design/02-exposers.md](docs/design/02-exposers.md) and [docs/design/08-storage.md](docs/design/08-storage.md).
**Not yet run for a week**: the whole feature multiplies the number of remote objects by the push rate, and the two
open items about limits below are the ones that decide whether that holds up.

- [x] **One preview can hold more than one publication.** `expose.Spec.BuildID` and `Spec.Key()`, and all four
      exposers key their live map, their remote tag and `Reap`'s keep-set on the key rather than on the preview id.
      This was the structural blocker: `Publish` withdraws whatever holds the key before taking it, so a build
      share used to tear down the branch share it was meant to sit beside. The branch share keeps the bare preview
      id as its key so an in-place upgrade does not reap every restored preview on the first sweep.
- [x] **Decide the naming.** Flat `<branch-name>-<sha7>`, derived from the branch share's name rather than rendered
      from the template a second time — so a `name_template` that separates repositories keeps doing so, and the two
      names sort next to each other in any list of shares (`publishBuildShare`, `internal/daemon/daemon.go:1442`).
      The owned-subdomain shape (`85912e2.docpreview.shares.zrok.io`) is not available: it needs a delegated
      namespace, which on hosted zrok.io is admin-only and a sales conversation — see
      [docs/design/19-zrok-namespacing.md](docs/design/19-zrok-namespacing.md), section 3. One template change away
      if that ever lands.
- [x] **Publish the build share.** `publishBuildShare` runs after the branch share, best effort: the branch share
      is the contract and is already live, so a reserved-name quota or a collision logs a warning and costs one URL
      rather than failing a build that succeeded. `d.liveBuilds` holds them, teardown closes them, and the reap
      keep-set lists every build with a recorded URL.
- [x] **Record the per-build URL.** `builds.name` and `builds.url`, with the project's first schema migration —
      `CREATE TABLE IF NOT EXISTS` does nothing to an existing table, so the columns had to be added by `ALTER`.
- [x] **Reclaim the zrok name on teardown.** `Daemon.releaseNames` releases every name the preview took — its own
      and one per build share, from the live publications *and* the recorded rows, since a preview whose republish
      failed has a name and no publication. `Zrok.ReleaseName` de-reserves via `PATCH /share/name` and then deletes,
      which is self-healing: `unshare` collects a non-reserved name on its own, so a crash mid-teardown is cleaned
      up by the next startup's `Reap`. `close` must never touch a name, because two of its three callers are
      rebuilds whose URL has to survive — `TestRebuildMustNotReleaseTheName`. Written up in
      [docs/design/16-exposer-zrok.md](docs/design/16-exposer-zrok.md), "Releasing a name". **Not yet run against a
      live account**: both calls were read from the controller source, not exercised.
- [x] **`isNameAlreadyExists` treated a quota rejection as success.** Four situations answer 409 from
      `POST /share/name` and only the empty-payload one means the name exists. Matching the type alone reported a
      registered name to an account at its name limit, and the following `CreateShare` failed for a reason that
      never mentioned quotas. Payload presence is the discriminator; the controller's own strings are the test
      fixtures. One name per commit reaches that limit far sooner than one per branch did.
- [ ] **Recover the names already leaked.** Every teardown before the item above left one behind. Wants a one-shot
      sweep that lists reserved names in the namespace, keeps the ones matching a live preview or a recorded build,
      and releases the rest — the same keep-set logic `Reap` uses for shares, applied to names. `ReleaseName` is the
      call; `ListNamesForNamespace` is the list. Filed as `docpreview names prune` in
      [docs/design/16-exposer-zrok.md](docs/design/16-exposer-zrok.md).
- [ ] **Decide what happens to `builds` rows on teardown.** `DeletePreview` removes only the `previews` row, so a
      torn-down preview's build rows survive and `backfill` puts them back in the feed after a restart. They render
      inert, because `markOpenable` finds no log and no artifacts, and `PruneBuilds` ages them out at `keep_logs`
      — so this is bounded and arguably correct: the history outlives the preview on purpose elsewhere. It is
      accidental rather than decided, and nothing says which it is.
- [ ] **Find the per-account reserved-name limit.** One name per build multiplies the count by the number of pushes
      to every open pull request, and the *name* is the quota-bearing object rather than the share
      (`controller/share.go` counts a `POST /share` against `shares` only). Nothing published says what a hosted
      zrok.io account is allowed, and `adminListAppliedLimitClasses` is admin-only, so an account cannot read its own
      ceiling — it has to be probed on a throwaway account by creating names until the 409 payload says
      `names limit reached`. Steady state is now the number of *live* previews rather than the number of builds ever
      run, since teardown releases names, which makes this tolerable rather than blocking. Section 2 of
      [docs/design/19-zrok-namespacing.md](docs/design/19-zrok-namespacing.md) has the method.
- [x] **Artifacts per build, not per preview.** `artifacts/<preview>/<build>`, so a build share has its own directory
      to serve and teardown removes the whole preview tree in one `RemoveAll`. `restoreBuildShares` republishes each
      build from its own directory at startup, and a build whose directory is gone — pruned by `keep_builds` — has its
      `builds.name` and `builds.url` cleared instead, because leaving the URL would keep the dashboard offering a link
      to something no longer on disk.
- [x] **Two Open buttons, not one.** The row's Open goes to the branch share; the log pane's top bar carries
      `Open build ↗` for whichever build is selected (`updateOpenBuild` in `dashboard.html`). That is what fixes the
      mismatch that prompted the whole idea: the log pane could read `build 20260729-190307-85912e2 — not live` while
      the only Open button on screen went somewhere else entirely, and no wording makes one button honest about two
      different things.
- [x] **Decide what an Open button does for a build with no share.** Greyed with the reason, not hidden and not a
      404 — the same rule the log pane already follows for a build whose log was not kept. A build has no share when
      it failed, when it predates per-build publishing, or when `keep_builds` pruned its artifacts and
      `ClearBuildShare` emptied the two columns. Hiding the control would read as a missing feature; a disabled one
      that says why does not.

**Limits, because the above makes unbounded growth the default.** Nothing in this project caps disk today. One
share and one artifact directory per preview kept that survivable by accident; per-build artifacts do not.

- [x] **A cap on retained builds per preview.** `preview.keep_builds`, default 10, pruned after each publish and
      never removing the build that just published — a clock stepping backwards would otherwise delete what is
      being served.
- [ ] **Byte and total caps.** A limit on one build's output size and a ceiling for the whole artifacts tree, with
      a documented eviction order. `keep_builds` bounds the *count*, which says nothing about a repository whose
      site is a gigabyte. Oldest build of the least recently updated preview is the obvious first rule.
- [ ] **Startup is serial and slow.** Reap-then-republish took 55 seconds for three previews, roughly 14 seconds
      per zrok round trip, with nothing reachable until it finishes. Thirty open pull requests is about seven
      minutes of downtime per restart. Republishing concurrently is the obvious fix; the reason it is not done yet
      is that `Reap` must complete first and publishing a name is destructive, so the ordering needs care.
- [ ] **Report usage where it can be acted on.** A dashboard that does not say how much disk the previews are
      using is a dashboard nobody can use to decide what to delete.
- [ ] **Exempt the paid exposers.** These limits exist because zrok's hosted service is free and shared. Frontdoor
      is a paid product and should not inherit a cap written for a free tier, so the limit belongs in
      configuration with a per-exposer default rather than as a constant in the pipeline.

**The GitHub App smoke test.** See [docs/design/11-github-setup-state.md](docs/design/11-github-setup-state.md)
for the full state.

- [x] **The daemon can boot with `github.app_id` set and the vault locked.** `Daemon.SetClient` and
      `Ingress.SetClient` make `scm.Client` swappable, and `rewireGitHub` installs the GitHub client from the
      `rearm` callback once the vault opens. Until then `/webhook/github` answers 501.
- [x] A tunnel to `127.0.0.1:8471/webhook/github`, then update the App's webhook URL. `docpreview webhook-only`
      over a named zrok share, per [www/docs/runbooks/webhook-tunnel.md](www/docs/runbooks/webhook-tunnel.md).
- [x] Install on the test repository and open a pull request. Deliveries arrive, builds run, the comment is
      upserted, and previews publish over `exposer.kind: zrok2`. Everything in "Fixed by running it for real"
      below came out of this.

- [ ] **Test the setup sequence against a live process.** Four bugs in the secrets UI were found by using it
      and none by the tests written alongside it, because those cover request/response and not
      open-page → set-passphrase → restart → return. `cmd/docpreview/rewire_test.go` now covers the
      unlock-then-install sequence in-process; the restart half is still untested.

**The master key.** `vault.key_source` now reads it from a file or a command, with the environment variable
demoted to a fallback and locked-boot the default. See
[docs/design/05-secrets.md](docs/design/05-secrets.md#the-master-key). What is left:

- [ ] **Show the key source on the dashboard.** The one thing an operator wants to know from the Secrets panel
      is whether a restart will need them, and right now that only appears in `doctor` and one startup log
      line. `secretsState` needs a field and the panel a sentence.
- [ ] **Audit secret access.** [OWASP][owasp] requires logging who requested a secret and when it was used,
      updated, or reused after expiry. `SecretsAdmin` logs writes; `Vault.Get` logs nothing, so a build that
      injects a credential leaves no record that it read one. Wants a logger on the vault and a decision about
      volume — every `Get` on a hot path is noise, so probably first-read-per-process plus every write.
- [ ] **Nothing expires.** The same guidance wants secrets created to expire and rotated on a schedule. Vault
      entries have no timestamp at all, so there is nothing to age out or report on. The GitHub installation
      token is already short-lived and minted per use, which is the shape the rest should follow; the App
      private key and the webhook secret are the static ones.

[owasp]: https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html

## Planned but not built

Eight design documents, 12–19 and 21, each ending with the order to build it in. See
[docs/design/README.md](docs/design/README.md). What follows is only the work those plans surfaced as urgent —
the plans themselves hold the reasoning.

### Several exposers at once, and one per project

**Planned, nothing built.** [21-multi-exposer.md](docs/design/21-multi-exposer.md) holds the design; the summary is
that a preview has exactly one publication today and that assumption is written into the two store rows, the
`Publication` type, the daemon's single `exposer` field, the pull request comment, and the reap.

Asked for after the exposer panel landed and made the singular visible: four sections with an `Enable` button each
reads as four independent switches, and enabling `local` therefore turned zrok off — which is a surprising way to
find out that a preview has one URL.

- [ ] **A publications table, keyed `(preview_id, build_id, exposer)`**, replacing the URL columns on `previews` and
      `builds`. The migration runs against a live database with 29 publications in it, and losing them loses the URL
      in every open pull request comment, so it is tested against a copy first.
- [ ] **A per-exposer reap keep-set, with a two-exposer test written before the fan-out is used.** An exposer reaping
      with a keep-set built from another exposer's publications deletes every live share it owns. Same footgun as two
      daemons on one account, from inside one daemon, and just as silent — deleting a share you believe you own is a
      normal thing to do.
- [ ] **One comment row per publication**, labelled and in a stable order. Unlabelled, a reviewer clicks an overlay
      or `127.0.0.1` URL and reports a broken preview; unordered, every status change reshuffles the rows and the
      edit history becomes noise.
- [ ] **A per-project exposer list**, on the projects page. Not settable from `.docpreview.yml`, for the reason no
      project field is: the file arrives in the pull request, so its author would otherwise choose where their branch
      gets published.
- [ ] **Per-exposer name collision checks.** `Collides` and `releaseNames` treat names as one namespace, so two
      previews rendering to one name in *different* exposers would be refused for no reason.

### Fixed by running it for real

Against real GitHub and a real zrok account. **None of these was reachable from the local git simulator** — it
creates no shares, renders no comment for a browser, and holds no files open — which is the argument for the smoke
test having been the next thing to do rather than more tests.

- [x] **No named zrok share could be created at all.** `CreateShare` with a `NameSelection` naming a name that is
      not registered in the namespace answers 409, so every publish failed and no preview ever got a URL.
      `Zrok.ensureName` registers it first and treats "already exists" as success — matched on the generated
      `*share.CreateShareNameConflict` type **and an empty payload**, because the 409 arrives with an empty body and
      three other situations answer 409 with a payload. Matching the type alone was itself a bug; see the
      quota item in the in-flight section. [16-exposer-zrok.md](docs/design/16-exposer-zrok.md).
- [x] **Preview URLs went into comments with no scheme.** `Share.FrontendEndpoints` reports a bare hostname, so
      the link resolved relative to github.com and 404'd there. `Publish` prefixes `https://` when the endpoint has
      none.
- [x] **Two builds of one pull request shared a workspace directory.** A supersede leaves the older build unwinding
      while the newer one clones, so the loser's cleanup deleted files under the winner — and on Windows a locked
      file made that cleanup fail, leaving a `.git` that made the winner's `git remote add origin` fail with
      "remote origin already exists". Now `workspaces/<preview>/<sha12>`, siblings pruned best-effort, and the named
      remote is gone entirely: the clone fetches the URL directly, so nothing persists to conflict.
      See [03-build-pipeline.md](docs/design/03-build-pipeline.md).
- [x] **The comment said "Queued" for a build already running.** `handleBuild` reported queued *after*
      `store.Enqueue`, which wakes a worker that reports building — a race it lost most of the time. Reported first
      now.
- [x] **Reports could move a comment backwards.** `Daemon.staleReport` drops any report ranked below the furthest
      state already reported for the same commit. Keyed by commit, because ready→queued is backwards within one
      commit and correct across two, and strictly-below, because two `ready` reports for one commit are legitimate.
      A timestamp cannot do this: the inverted queued report was stamped *later*, since it was created later.
      See [04-concurrency.md](docs/design/04-concurrency.md).
- [x] **A build's state changes were four API writes.** `internal/daemon/publisher.go` coalesces platform writes per
      preview over 250 ms, which is safe because a report is a snapshot and the comment is rewritten whole. The
      timer is not reset on each report — that lets a steady stream postpone the write forever — and `Close`
      flushes, because a shutdown inside the window loses the terminal report of every in-flight build.
- [x] **A republish served artifacts against a base URL they were not built with.** The base URL comes out of a
      stored row, and that row disagrees with the artifacts whenever `exposer.kind` changes — index.html loads and
      every asset 404s, with nothing in any log. `pipeline.VerifyBaseURL` is now exported and called from
      `Daemon.republish`; a mismatch drops the row so the next push rebuilds.
- [x] **The activity feed was empty after every restart** and build history existed nowhere. A `builds` table holds
      one row per attempt, written when the build starts and again with its outcome, pruned on the same
      `build.keep_logs` window as the logs. `Daemon.backfill` seeds the feed from it at startup, and the dashboard
      has a build picker that says how each stored log ended. See [08-storage.md](docs/design/08-storage.md) and
      [07-dashboard.md](docs/design/07-dashboard.md).
- [x] **The Secrets link is gone from the dashboard's top bar.** A control for credential management on an
      operations screen is unrelated to everything around it, and a form for pasting a private key invites being
      pasted into. `/secrets` is reached by URL or from a runbook; the `secrets` field on `/status` that used to
      hide the link conditionally went with it.

### Fixed while planning

- [x] **The `Retry-After` and rate-limit handling was inert.** `X-RateLimit-Reset` is epoch seconds and was
      parsed as RFC3339, so every rate-limited response reported a zero wait. Secondary rate limits — the ones a
      supersede storm hits — are a 403 with a *non-zero* remaining count and were classified as permission
      errors, turning a wait-and-retry into a failed build and a comment stuck on "Building".
- [x] **A revoked installation token was never re-minted.** Nothing invalidated the cache, and revocation does
      not move the clock, so a permissions change or a reinstall meant every request 401'd until the cached
      expiry passed. `do` now invalidates and retries once.
- [x] **The clone-URL scrubber leaked the credential it existed to hide.** It split userinfo on the *first* `@`,
      so a username containing one — a Bitbucket credential is an email address — published the token into the
      build log and from there into the pull request comment. Now splits on the last `@` inside the authority.
- [x] **`exposer.kind: frontdoor` could not boot with a locked vault.** The token was read during wiring, the
      same trap the GitHub client was lifted out of. `NewFrontdoor` takes a `TokenFunc` resolved per request,
      startup validation downgrades a locked vault to a warning, and `revalidateExposer` re-checks after unlock.
- [x] **A bad webhook signature answered 400 on every platform except GitHub.** `ErrBadSignature` lived in the
      github package and the ingress tested for that one, so the local client's bare error became a malformed-body
      response — which distinguishes a wrong secret from a wrong payload for anyone guessing at the secret. The
      sentinel is now `scm.ErrBadSignature`, with the github name kept as an alias.
- [x] **`Retryable()` had no caller.** Classification without a retry loop only labels the error on its way to a
      failed build. `do` now retries a rate limit up to `rateLimitAttempts` times, waiting as long as GitHub asks
      and cutting the wait short if the context ends. A 5xx is deliberately *not* retried: a rate limit is refused
      before GitHub acts, but a 500 may have created the comment before failing to say so, and the upsert
      finds-then-creates, so retrying there posts a second comment.

### Documentation

- [x] **Docs are current with the code**, and `CLAUDE.md` exists per directory (root, `cmd/docpreview`,
      `internal/daemon`, `internal/vault`, `internal/expose`, `internal/scm`, `demo`, `www`). New:
      [www/docs/runbooks/webhook-tunnel.md](www/docs/runbooks/webhook-tunnel.md), the procedure for exposing the
      webhook and proving it works before GitHub is configured.

### The webhook tunnel

- [x] **`docpreview webhook-only`** forwards exactly `POST /webhook/github` to the daemon and 404s everything
      else, optionally serving over a named public zrok share (`-zrok-name docpreview`) so there is no local TCP
      port for anything else to find. It exists because `zrok2 share public http://127.0.0.1:8471` publishes
      *every* route — including `/api/secrets`, whose gate only checks that the daemon's listeners are loopback.
- [x] **Credential writes now require the request to be local**, not just the listener. Loopback `RemoteAddr`
      **and** no `X-Forwarded-For`/`Forwarded`, because under a tunnel the daemon sees the connection from the
      local tunnel process and every internet request looks like loopback. The read path stays open and reports
      `can_write: false` with a reason, so the panel renders read-only rather than vanishing.
- [ ] **A password on the secrets surface.** The locality gate is a boundary, not authentication, and it assumes
      nothing proxies to the daemon while stripping forwarding headers. Managing credentials from anywhere but
      the host needs a real credential of its own.
- [ ] **A GitHub Action that calls the webhook over OpenZiti**, removing the tunnel entirely. The webhook is the
      only route that must be reachable from outside, and an overlay dial authenticates the caller in a way a
      public URL plus an HMAC does not.

**Four zrok v2 facts worth not relearning.** `zrok2 share public` takes one positional — the target — and its
`-n/--name-selection` flag sets only `NameSelection.NamespaceToken`, despite the name and despite its help text
saying "frontends". So the rc8 CLI cannot bind a reserved name to a public share at all. The SDK can:
`ShareRequest.NameSelections` carries `{Name, NamespaceToken}`, which `internal/expose/zrok.go:160` already
uses. **A name must be registered before a share can bind it** — `CreateShareName` first, or `CreateShare` answers
409 with an empty body. And `Share.FrontendEndpoints` reports where a share is reachable, so nothing needs to guess
a DNS suffix — but it is a **bare hostname**, not a URL, so anything putting it in a link has to add the scheme.

### Next, in rough order

- [ ] **A fake api.github.com.** [13-github-testing.md](docs/design/13-github-testing.md) designs it in full.
      `internal/scm/github/api_test.go` is the beginning of one — it covers the token exchange, rate-limit
      classification and the 401 retry. The comment upsert, `ChangedFiles` pagination and the deterministic
      supersede test are the valuable remainder.
- [ ] **Nothing stops every pull request on an installed repository being built.** A repository allowlist plus a
      required `.docpreview.yml`, and stop writing the queued comment before the build decision. See
      [12-github-roadmap.md](docs/design/12-github-roadmap.md).
- [ ] **Uninstalling the App leaves previews serving with no way to stop them.** The `installation` and
      `installation_repositories` deleted events are unhandled.
- [ ] **Two daemons sharing one exposer account delete each other's live shares.** Confirmed for both zrok
      (`Reap` filters on environment plus a `docpreview:` prefix, and the environment is a property of
      `$HOME/.zrok2`) and frontdoor (the tag carries the preview ID and nothing identifying the instance). An
      instance identity in `Spec` and `Reap` is worth doing while there are four exposers rather than eight, and
      it is the first of the five HA blockers in
      [14-production-deployment.md](docs/design/14-production-deployment.md).
- [ ] **A zrok name race.** The collision check drops the lock before `CreateShare`, and the daemon's commit
      lock is per preview, so two previews rendering to one name both pass and the loser's `reapName` deletes
      the winner's fresh share. See [16-exposer-zrok.md](docs/design/16-exposer-zrok.md).
- [x] **Registered zrok names were never released.** One per preview name ever published, surviving the deletion of
      the database. Teardown releases them now; the names leaked before it did are still there, and recovering them
      is the `docpreview names prune` in the in-flight section. A name still survives a database wiped by hand,
      which is the audit gap below rather than this item.
- [ ] **`ensureName` cannot tell a name this account owns from a stranger's.** The 409 has an empty body, so both
      read as success and the failure surfaces one call later from `CreateShare` with a message that does not say
      which it was. `ListNamesForNamespace` would tell them apart and is the same call `docpreview shares list`
      wants.
- [ ] **Nothing tests the zrok publish path**, including the two fixes above it that stopped it working. It needs
      the `zrokAPI` seam from [16-exposer-zrok.md](docs/design/16-exposer-zrok.md) — item 2 of that build order,
      and the thing everything else there depends on.
- [ ] **The `builds` table records only `building`, `failed` and `ready`.** A skipped build and a queued one leave
      no row, so the history shows a gap where a push was deliberately not built — which is the one case somebody
      asks the history about. The skip branch in `Daemon.build` (`internal/daemon/daemon.go:812`) is where the
      first belongs.
- [x] **A pending job survived the teardown of its preview.** `Claim` was the only statement that removed a `jobs`
      row, so a push landing just before a close left a job that a worker later claimed and built, republishing a
      preview that was deliberately removed — and unlinking a pull request is a button an operator presses to make
      that stop. `teardown` now calls `store.Dequeue`, the same statement the cancel button uses, and collects its
      error rather than logging it: a surviving job puts back everything the rest of teardown removes. The narrow
      race left is a worker that has claimed the job but not yet registered it in `d.running`, which is visible in
      neither place; closing it wants the commit phase to re-read the pull request's state, an API call it does not
      make today.
- [ ] **`/healthz` is `ok\n` and answers before recovery runs**, and the vault's locked state — which makes
      every GitHub webhook answer 501 — appears in no endpoint. Extend `/status`.
- [ ] **Log the dialing identity on the ziti exposer**, then enforce against it. This is the cheap first half of
      closing the per-preview authorization hole below, and the same hook lifts the secrets surface's blanket
      refusal of ziti listeners (`internal/daemon/secrets.go:86`) — which is the argument for doing it in the HTTP
      handler rather than with a service per preview. `edge.ServiceConn.GetDialerIdentityId` exists and is
      reachable through `http.Server.ConnContext`. See [18-exposer-ziti.md](docs/design/18-exposer-ziti.md).
- [ ] **Two ziti binders is a startup failure, not a comment.** Today it yields two terminators, disjoint routing,
      and roughly half of all previews 404ing while every log looks healthy. The Bind policy is keyed on a role
      attribute, so any identity granted that attribute silently becomes a second legal binder.

## Open questions that gate work

**Per-preview authorization on the ziti exposer.** Today one wildcard service serves every preview and requests
are separated by the HTTP `Host` header. That is not zero trust: the header is client-supplied, so anyone
holding the `docpreview-reader` attribute can reach every preview by sending any hostname. Per-preview services
with per-preview Dial policies would put authorization on the overlay where it belongs, at a cost of four
management-API objects per pull request and DNS churn on every connected tunneler. A middle option exists —
keep one service and check the dialing identity in the HTTP handler, since `edge.ServiceConn` carries it via
`GetDialerIdentityId` — which gets real authorization without the object churn but leaves the boundary in the
application rather than the network.

[18-exposer-ziti.md](docs/design/18-exposer-ziti.md) argues for the middle option and gives the reason that
settles it: a service per preview needs a per-preview identity model to key a Dial policy on, and docpreview has
none — reviewers hold one shared reader attribute. So the overlay would carry objects whose strength nothing
could yet use, while the handler-side check both closes this hole and lifts the secrets surface's blanket refusal
of ziti listeners. **Read that document before deciding; it is no longer undecided so much as unstarted.**

**Which OpenZiti network for anything real.** `configure ziti` targets a local quickstart. Self-hosted or
NetFoundry-hosted is a decision nobody has made.

## Bitbucket

**Built and exercised against a live private repository**, 30 July 2026: `netfoundry/customer-connect-docs` cloned
with a repository access token, built, published, and a comment upserted on pull requests 19 and 20. The plan in
[docs/design/15-bitbucket.md](docs/design/15-bitbucket.md) has landed and its build order is spent; what remains
below is what a week of use will decide rather than what is missing.

- [x] **The marker is portable.** `scm.MarkerStyle`, `MarkerFor` and `HasMarker`; `findComment` matches with
      `HasMarker` rather than against one rendered string. Bitbucket escapes raw HTML, so `<!-- docpreview:… -->`
      would render as a visible paragraph there — Vercel ships exactly that defect on a public pull request. The
      working form is a CommonMark link reference definition. Done first and on GitHub alone, because a matcher that
      forgets a style posts a duplicate comment on every open pull request, and only GitHub has comments in the wild.
- [x] `internal/scm/bitbucket` implementing `scm.Client`, writing `MarkerLinkRef`. Webhook verification
      (`X-Hub-Signature`, which is SHA-256 here despite the name GitHub uses for its SHA-1), comment upsert,
      diffstat for changed files, and fork refusal.
- [x] **A repository access token, not app passwords** — those were removed 28 July 2026. Per *project*, not
      workspace-wide, because an administrator can forbid the wider kind: `project/<platform>/<owner>/<repo>/scm.access_token`
      overrides a global `bitbucket.access_token`, and the projects page tests it before a build depends on it. No
      account email is involved; `x-token-auth` is the clone username and the API takes a bearer token.
- [x] **`Repo.Name` holds the slug**, and **`source.commit.hash` is normalized from 12 characters to 40** through
      `resolveCommit`, so `Report.Commit` is one width on both platforms.
- [x] **IPv4 first.** `api.bitbucket.org` over IPv6 from this host resets mid-response — `wsarecv: An existing
      connection was forcibly closed` — which surfaced as a credential that worked under `curl --ipv4` and failed
      from the daemon. `ipv4First()` dials `tcp4` and falls back.
- [ ] **A comment is authored by the token's name.** A repository access token posts as its own label, so comments
      arrive from `BB_REPO_TOKEN_CUSTOMER_CONNECT_DOC_PREVIEW`. The API exposes no author field; the only lever is
      renaming the token in Bitbucket. Worth documenting rather than fixing.
- [ ] **`pullrequest:updated` fires for a title edit as well as a push.** Today that queues a build of a commit
      already built, which supersede handles and the cache makes cheap, but it is work nobody asked for. Comparing
      the head against the stored commit before enqueueing is the fix.
- [ ] Vault keys reserved and unused: `bitbucket.email`, `bitbucket.api_token`. Kept because a workspace-wide
      credential of the account kind is still a legitimate shape; the secrets page marks them optional rather than
      missing.

## Identity management

`configure ziti` bootstraps, but there is no way to add or remove a reviewer afterwards, which means **no
revocation story**.

- [ ] `docpreview identity add <name>` — mint an identity, emit its enrollment JWT
- [ ] `docpreview identity list`
- [ ] `docpreview identity remove <name>`
- [ ] Optional: write the JWT straight to `\\.\pipe\ziti-edge-tunnel.sock` on Windows so enrollment is one
      command. Needs the pipe's DACL confirmed first — `(Get-Acl \\.\pipe\ziti-edge-tunnel.sock).Access`.

## Verification gaps

- [ ] Frontdoor's wire format has never been exercised against a live tenant. The guard added in round 1 means
      a wrong field name now fails loudly at the first publish, but the field names are still guesses.
      Confined to `shareRequest` and `shareResponse` in `internal/expose/frontdoor.go`.
- [ ] NetFoundry Cloud v2 `POST /core/v2/endpoints` — shape taken from docs and `python-netfoundry`, not from
      a run against an org. The 202-then-poll behaviour in particular.
- [ ] `docpreview serve` graceful SIGINT has not been exercised interactively on Windows; background processes
      there are killed hard. `BaseContext` is now wired so SSE handlers unblock, and
      `TestOverlayIngressStopsWhenClosed` covers the shutdown path, but a human Ctrl-C has not been watched.
- [ ] A real Ziti Desktop Edge import against a `configure ziti`-provisioned network. The SDK dial is the
      equivalent proof minus DNS and TUN, and ZDEW was confirmed manually in the earlier trial, but not
      against the provisioned objects.
- [x] **`clicks.mjs` reported four failures against live state: the log pane lost its place on every re-render.**
      Cause was the branch-preview exclusion. `inList` drops a *finished* branch preview, so clicking its activity
      entry pinned a preview the list excludes; the next render decided the pin was stale, reset it to the
      project's newest, and the pane jumped to a different preview's log. One tick of working, then silently
      undone. Fixed with the `pinned()` exception in `groups()`, and `feedrows.mjs` now asserts both halves — the
      pin survives a render, and the preview leaves the list again when deselected. Only reproduced against live
      state, where one project has a branch preview *and* several pull requests.

### The log pane still does not start tailing a build that begins while you are watching

**Open, and the most-reported defect in the dashboard.** Not the same bug as the pinning one above, which is
fixed: this is a row already open on a finished build when a *new* build starts underneath it. The pane should
switch to the running build and follow it. Reported repeatedly as "it doesn't tail properly", and a reload always
fixes it — which is the signature of state the render path does not recompute.

Three diagnoses of this have been wrong, all made by reading the code, which is why the next attempt should start
by reproducing it rather than by explaining it.

What is already known, and what each harness does *not* cover:

- `logtail.mjs` passes the scripted version of exactly this sequence, so the mechanism works when driven
  directly. It calls `showBuild` and stubs `EventSource` per stream — it never exercises the path where a
  **status event announces the new build** under an open row. That gap is the obvious suspect: `updateBody`
  refreshes the picker from `loadBuilds`, which is async, and nothing there decides "a build started, follow it".
- `clicks.mjs` drives clicks, not the event stream, so the arrival of a new build is outside it too.
- The daemon's side is exercised: a stream opened while a build is queued waits for it rather than 404ing.

- [ ] **Reproduce it in a harness first.** A row open on a finished build, then a `status` event carrying that
      preview as `building` with a new build id — the way the daemon actually announces it. Assert the picker
      gains the build, the pane switches to the live stream, and `Following` is on. That test is the deliverable
      whether or not the fix follows in the same commit.
- [ ] **Then check the two seams the scripted test bypasses**: whether `updateBody` reacts to a preview becoming
      live under an open row at all, and whether the `loadBuilds` response can arrive after the render that
      needed it — a late response repopulating the picker for the *previous* build would produce exactly the
      reported symptom.
- [ ] **Watch it happen against the live instance** with `Push-Change.ps1`, since every earlier reproduction
      attempt used a fixture and the report comes from real use.

## Namespace hygiene

Each preview gets its own share, its own name, its own listener — and, since per-build publishing, one more of each
per build, named `<branch-name>-<sha7>`. The default `name_template` is
`{{.Repo.Name}}-{{.Name}}` — project and branch — because the branch alone collides across repositories and
every exposer keys something on the name. Deliberately not the commit SHA: the pull request comment is edited
in place, so the link a reviewer already opened has to survive the next push. `{{.HeadSHA}}` is available for
anyone who wants immutable per-commit URLs.

Cleanup is wired, not aspirational:

- `Exposer.Reap(ctx, keep)` runs at startup with `keep == nil` — nothing is live yet, so every share carrying
  the `docpreview:` target prefix is a leak from a previous process — and again on every sweep tick with the set of
  publication keys the database still recognises: every preview id, plus `<preview>/<build>` for each build row that
  recorded a URL. A keep-set that cannot be assembled completely skips the sweep, because an incomplete one does not
  under-delete, it deletes live shares.
- `Preview.TTL` tears down previews nobody has touched, which removes them from `keep` and so from the remote.
  Teardown also releases the exposer's names, before withdrawing the shares, so a crash mid-teardown self-heals.
- The prefix is what stops it deleting shares an operator made by hand.

- [x] **Shares are adopted at startup rather than deleted and recreated.** `Reap` used to run with an empty
      keep-set — delete every share carrying the `docpreview:` prefix, then create them all again. Measured on
      2026-07-30: 85 seconds of deleting followed by 183 seconds of creating, to put two pull requests back in
      the state they were already in, and every preview URL 404s throughout.
      What dies with the process is the overlay listener; the share on the controller does not, and
      `sdk.NewListener(token, root)` binds to an existing token. So `expose.Adopter` lists what is already
      published (`Adoptable`), startup passes the database's claim as the keep-set so only genuine orphans are
      deleted, and each restore binds a listener to the share that is already there. A failed adoption falls
      through to `Publish`, which replaces by name and so is also the cleanup.
      **4m 28s → 3s** on the live instance, all thirteen shares adopted. Verified against the hosted account:
      a bind against a share created by a dead process carries traffic, and `ListShares` reports
      `FrontendEndpoints`, so no URL has to be reconstructed. `internal/daemon/adopt_test.go` covers
      prefer-adoption, fall-back-to-publish, the wrong-key case, and an exposer with no `Adopter`.
- [ ] **A name that cannot collide, and a defined answer when one does.** The template is configurable, so
      whether two previews can render to one name is an operator's decision they have no reason to know they are
      making — and the collision is silent: the loser's name-reclaim can delete the winner's fresh share. Make the
      default carry the whole identity, narrowest first, so it reads as a hostname and sorts by branch:
      `<branch>-<repo>-<owner>-<platform>`, with `gh` and `bb` for the platform to stay inside the DNS label
      limit. Then define the conflict path, because a real collision is nearly always our own leaked share:
      try to reclaim it (the `docpreview:` target prefix says it is ours); if that fails, suffix a counter; and if
      *that* fails, refuse the build with an error naming the fix — rename the branch — rather than fighting over
      a name forever. Needs a live zrok account to verify, which is the same gap that blocks the reap tests below.
- [x] **A failed republish left the row advertising a dead URL.** Seen live on 2026-07-30: a preview that had built
      successfully failed to restore at startup when `CreateShare` timed out, and `/status` went on reporting
      `state: ready` with a URL that answered 502 for the rest of the day. The row is now **marked, not deleted** —
      `store.FailPreview` empties `url`, sets `failed` and records a reason naming Rebuild, and
      `Daemon.markUnpublished` sends a matching failed report so the comment stops offering the link. Emptying the
      URL is the half that matters: the Open button is enabled by the presence of a URL rather than by the state,
      deliberately, so a state alone would change nothing. The artifacts are still good, which is why the row lives
      — Rebuild is offered from it. `name` stays for the same reason `releaseNames` reads it. Covered by
      `internal/daemon/recovery_test.go`.
- [ ] **A build share whose preview failed to restore still advertises its URL.** The other half of the item above,
      one level down: when the *preview's* republish fails, `restoreBuildShares` never runs, so every
      `builds.url` for it goes on offering a per-commit link with no listener behind it — and the log pane's
      `Open build ↗` is the button that offers them. `ClearBuildShare` is the wrong tool as it stands, because it
      empties `name` too and the name has to be released first or it leaks against the zrok quota. Bounded: the next
      startup's keep-set no longer claims those shares, so `Reap` deletes them.
- [x] **`docpreview shares list`.** Four states, problems first: `orphan` (the account holds it, the database does
      not claim it), `missing` (the database claims it, the account does not hold it), `ok`, and `never` (a recorded
      preview that has not published yet, counted apart so it is not a permanent false alarm). Read-only —
      deleting is `Reap`'s job, and an audit command that also deleted is one nobody would dare run. Its first run
      against the live account found a real inconsistency, below.
- [ ] **A build row can claim a publication it does not own.** Found by the command above: build
      `20260730-184200-cf9f37d` records a name and URL whose share is held under its sibling
      `20260730-181530-cf9f37d` — the same commit, so the same rendered name. The URL resolves, through the older
      share, which is why nothing noticed. That is `expose.Collides`'s "same preview, newer build wins" path not
      completing: the takeover withdrew the old publication without the new row's share replacing it.
- [ ] **The audit cannot see a leaked reserved name**, which is the object the quota actually counts. Once a
      share is deleted its name is invisible to `ListShares`, so nothing can find what earlier versions leaked.
      `Zrok.Names(ctx)` over `ListNamesForNamespace` is the missing piece — see
      [19-zrok-namespacing.md](docs/design/19-zrok-namespacing.md).
- [ ] **`Adoptable` collapses duplicates.** It returns a `map[key]Adoptable`, so two shares tagged with the same
      publication key become one — exactly the state a failed publish leaves behind, and therefore invisible to
      both the audit and to Reap. A slice, or a separate `Shares(ctx)` listing for auditing, fixes it.
- [ ] **Reap is untested against a live tenant** for zrok and Frontdoor. Covered by the same gap as their wire
      formats, below.

## Known limits, deliberate

Not bugs. Listed so nobody rediscovers them as surprises.

- **One docpreview per ziti service.** Binding creates a terminator; two instances create two and the router
  load-balances between them, so each holds a disjoint routing table. Give a second instance its own service.
- **The `local` exposer's URL is reachable by one person.** It is a path on the daemon's own loopback listener,
  `/preview/<name>/`. Stable now — it used to allocate an ephemeral port per publish, which made every recorded URL
  dead after a restart while the row still said `ready` — but still reachable only from the machine that built it.
  `local` is for trying the pipeline, not for sharing a link.
- **A preview link is only offered while the state is `ready`.** Queued, building and failed have no URL, and a
  torn-down one has a URL that no longer answers. The dashboard renders the button inert rather than linking to
  a connection-refused.
- **Redaction cannot catch a transformed secret.** Six encodings are matched. A build that hashes, encrypts, or
  prints a credential in fragments defeats it. It is a last line of defence over output docpreview does not
  control.
- **A secret straddling the 64 KiB flush boundary can survive.** Lines longer than the cap are flushed in
  chunks rather than buffered without limit, and each chunk is scrubbed — but a secret spanning exactly that
  split is missed.
- **Fork pull requests are refused.** Building one means executing a stranger's `package.json` under an
  installation token. There is no flag for this and there should not be.

## Agents (research only — see the spike below)

A spike looked at where LLM agents could fit. Three things came out of it as worth building, in this order:

- [ ] **A machine-readable preview status keyed by PR.** `Daemon.Status` already returns the right data but
      only as a global list on `GET /status`. Add `GET /api/preview/{platform}/{owner}/{repo}/{number}`
      returning the `StatusPreview` plus a log URL, and embed the same JSON as a hidden block in the PR
      comment next to `scm.Marker` so an agent that can only see the comment still gets structured data.
      An MCP server is a façade over this, not a substitute for it — do not build it first.
- [ ] **A deterministic post-build link checker**, hooked in `runPipeline` after `builder.Build` and before
      the commit phase, modelled on `pipeline.Detector` (timeout, no ability to fail the build). This is the
      half of "agent review" that needs no model, and it gives a later LLM summary a factual substrate
      instead of asking it to find problems by feel.
- [ ] **`docpreview identity add` for agents.** Already listed under Identity management. Giving a reviewing
      agent a ziti identity rather than a bearer token is the differentiated angle here, and it makes the
      per-preview authorization question above *decidable*: once some readers are bots you want the handler
      to tell them apart, which argues for checking the dialing identity in `Ziti.route` over a service per
      pull request.

Ruled out deliberately: an agent writing the PR comment (`RenderComment` is deterministic and posts under the
app's installation token), anything touching the supersede/commit machinery, and relying on `redact.Redactor`
to scrub generative output — it matches known values in six encodings, and a model that paraphrases a
credential defeats it entirely. The answer there is to keep the build log away from any model, not to redact
harder.

## Secret management from the dashboard

Mostly built. `/secrets` sets, generates and deletes global vault entries and `/projects` holds a project's own
environment variables; `docpreview vault set <key>` on the host is no longer the only way in. Two of the three wanted
scopes exist, narrowest winning:

- [x] **Global** — `build.secrets`, applied to every build on the daemon.
- [x] **Per project** — `project/<platform>/<owner>/<repo>/<ENV>`, resolved per build. See the in-flight section for
      what shipped and [docs/design/05-secrets.md](docs/design/05-secrets.md) for why the scope is a key prefix
      rather than a column.
- [ ] **Per git provider** — the GitHub App private key and webhook secret are already provider-scoped by
      convention (`vault.KeyGitHubPrivateKey`); making that a real scope is what lets one daemon serve a
      GitHub org and a Bitbucket workspace without their credentials sharing a namespace. The project prefix is the
      precedent to follow, and `bitbucket.*` already sits beside `github.*` by convention alone.

The four things that made this harder than a form, all still load-bearing and none of them finished business:

- **Write-only.** The UI can set and delete a secret and never read one back. `vault.Secret` refuses to render
  itself through Stringer, Formatter, GoStringer and json.Marshaler, and no endpoint returns a stored value —
  `generate` returns what that call minted, once, which is not a read. Keep it that way.
- **The dashboard has no authentication.** Writes are gated on two independent checks — a loopback-only daemon
  *and* a local, unforwarded request — because a tunnel makes every route reachable while every listener is still
  loopback. That is a boundary, not authentication; the real credential is the open item under "The webhook
  tunnel". A ziti listener is refused outright until the dialing identity is checked.
- **Rotation has to re-arm the redactor.** `rearm(changed)` rebuilds the redactor and the GitHub client on every
  write. A secret changed at runtime that does not rebuild the redactor is a secret that appears in the next build
  log verbatim, and `Builder.WithSecrets` merges and recompiles in one call so the two cannot drift apart.
- **Audit.** Who set what, when. Not who read it — nobody can. `SecretsAdmin` logs writes; `Vault.Get` logs
  nothing. Still open, above.

## Improvements worth making

- [ ] **One shared status poller.** `statusPollInterval` runs per SSE connection, so each open dashboard costs
      two sqlite reads every 700ms. Fanning one poller out to subscribers — the way `buildlog.Writer` already
      does for log lines — removes that. Deferred because two open dashboards is the realistic ceiling.
- [ ] **`docpreview init` does not offer `ziti`.** `checkExposerKind` rejects it; `configure ziti` is the
      intended path, but the inconsistency will confuse someone.
- [ ] **Monorepo support.** One `.docpreview.yml` builds one site. A repository with several documentation
      sites cannot preview the one a pull request actually touched.
- [ ] **Log search.** The viewer tails and downloads; finding a line in a 5000-line build means downloading it.
- [ ] **`keep_builds: 10` means ten per-build shares nobody asked for.** Adoption made a restart cheap, so this
      is no longer urgent — but eleven of the thirteen publications restored for two pull requests are per-commit
      history, and the first restart after a database change pays full price for all of them. Capping the restore
      at the newest one or two, or restoring a build's share lazily on first click, is a few lines.
- [ ] **The build output is still written through the NTFS bind mount.** `node_modules` and the package cache both
      moved to docker volumes — the cache was measured filling at 0.4 MB/s as a bind mount — but the site itself is
      an entire tree of small files written to the host, and prerendering is the phase that visibly stalls. Build
      to a volume and copy the output out in one transfer.
- [ ] **A build can run the docker VM out of memory, and the failure names the page rather than the cause.** Seen
      on 2026-07-31 building `github:netfoundry/docusaurus-shared` at `main`: six minutes, then
      `[cause]: [Error: ENOMEM: not enough memory, write]` while prerendering one route, reported as
      "Docusaurus static site generation failed for 1 paths". Nothing in that message says the container hit a
      limit, so it reads as a broken page. Two halves: the operator's `.wslconfig` bounds the whole VM, and
      docpreview sets no per-container limit and cannot see what it was given. Worth detecting — a container
      killed for memory is distinguishable from a build that failed on its own, and saying so would save the
      afternoon this cost.
- [x] **The daemon has a log file.** `log_file` in the server config, teed with an `io.MultiWriter` so the output
      still reaches the terminal somebody may be watching. Appended to, never rotated — rotation belongs to
      whatever supervises the process, and truncating on boot would delete the evidence from before the restart
      being investigated. A path that cannot be opened is a warning, not a refusal to start.
- [ ] **The container has no TTY, so build tools block-buffer.** Output arrives in 4-8 KiB lumps rather than by
      the line, which reads as a stalled build. A TTY would line-buffer, at the cost of ANSI escapes to strip.
- [ ] **Preview diffing.** Vercel shows what changed visually between deployments. Not attempted.
- [ ] **`internal/scm/local` has no tests.** The package is exercised end to end by the demo but has no unit
      tests of its own; `VerifyWebhook`, `ChangedFiles` and the path checks all deserve them.

## Done, for context

Rounds of review that produced fixes worth not regressing:

- Supersede correctness: a newer push must leave nothing of the older build behind (commit phase, generation
  ownership, `unpublish` on a failed record).
- ziti label collisions: a preview may not take a hostname another preview owns, and a stale publication may
  not withdraw its successor.
- Build log redaction, including the split-across-writes case and the reader/writer cap mismatch that made
  overlong logs unreadable.
- SSE carriage-return injection: build output could otherwise forge event frames.
- Name collisions in every exposer: `live` was keyed by the preview's name, which is the branch, so a second
  repository with the same branch name tore down the first one's publication. Now keyed by the publication —
  `expose.Spec.Key()` — with zrok and Frontdoor refusing a name another preview holds rather than taking it.
- `build.secrets` was dead config — nothing called `WithBuildSecrets`, so no secret was ever injected and the
  redactor was never armed. An unredacted log is indistinguishable from a log with no secrets in it.
- `Status` read only the preview table, which never holds `queued` or `building`, so the dashboard's counters
  read zero during a build and a branch building for the first time had no row at all.
- `init` writes to a temp file and validates before renaming, so a bad value cannot cost you a working config.
