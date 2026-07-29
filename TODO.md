# TODO

Live list of what is outstanding. Ordered roughly by what would be picked up next.

Related: `www/docs/future/ziti-native-previews.md` holds the design research that is not yet a feature.

## In flight

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

**A share per build, not just per branch.** Today one preview owns one share, and it always serves the newest
successful build. The consequence shows up on the dashboard: the log pane can be reading
`build 20260729-190307-85912e2` while the Open button next to it goes to whatever is currently published. Two
different builds described side by side, and no way to look at the older one at all.

Wanted: a share per commit built **and** one for the branch. Five builds of a branch means six shares — the branch
share following the newest, plus five that stay pinned to their commit.

- [x] **One preview can hold more than one publication.** `expose.Spec.BuildID` and `Spec.Key()`, and all four
      exposers key their live map, their remote tag and `Reap`'s keep-set on the key rather than on the preview id.
      This was the structural blocker: `Publish` withdraws whatever holds the key before taking it, so a build
      share used to tear down the branch share it was meant to sit beside. The branch share keeps the bare preview
      id as its key so an in-place upgrade does not reap every restored preview on the first sweep.
- [ ] **Decide the naming.** Blocked on whether zrok v2 can hang shares off one owned subdomain
      (`85912e2.docpreview.shares.zrok.io`) rather than flat siblings — see
      [docs/design/19-zrok-namespacing.md](docs/design/19-zrok-namespacing.md). A provisional flat
      `<branch>-<sha7>` is enough to build against and is one template away from either answer.
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
- [ ] **Find the per-account reserved-share limit.** One share per build multiplies share count by the number of
      pushes to every open pull request. If zrok caps reserved shares per account, that cap is the feature's real
      ceiling and has to be known before this is built, not after.
- [ ] **Artifacts per build, not per preview.** `artifacts/<preview>` currently holds one built site. Per-build
      shares need `artifacts/<preview>/<build>`, which changes what teardown removes and what the base-URL check
      compares against.
- [ ] **Two Open buttons, not one.** The row's Open goes to the branch share, which follows the newest successful
      build. Each entry in the build dropdown gets its own Open, going to that build's share. This is what fixes
      the mismatch that prompted the whole idea: the log pane can read `build 20260729-190307-85912e2 — not live`
      while the only Open button on screen goes somewhere else entirely, and no wording makes one button honest
      about two different things.
- [ ] **Decide what an Open button does for a build with no share.** Older builds will have been evicted by the
      disk limits below, and a skipped build never had one. The dropdown will therefore contain entries that
      cannot be opened, and the button has to say so rather than 404 — the same rule the log pane already follows
      for a build whose log was not kept.

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

Seven design documents, 12–18, each ending with the order to build it in. See
[docs/design/README.md](docs/design/README.md). What follows is only the work those plans surfaced as urgent —
the plans themselves hold the reasoning.

### Fixed by running it for real

Against real GitHub and a real zrok account. **None of these was reachable from the local git simulator** — it
creates no shares, renders no comment for a browser, and holds no files open — which is the argument for the smoke
test having been the next thing to do rather than more tests.

- [x] **No named zrok share could be created at all.** `CreateShare` with a `NameSelection` naming a name that is
      not registered in the namespace answers 409, so every publish failed and no preview ever got a URL.
      `Zrok.ensureName` registers it first and treats "already exists" as success — matched on the generated
      `*share.CreateShareNameConflict` type rather than the message, because the 409 arrives with an empty body.
      See [16-exposer-zrok.md](docs/design/16-exposer-zrok.md).
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

Nothing exists beyond the interface. `POST /webhook/bitbucket` returns 501. The research is done —
[docs/design/15-bitbucket.md](docs/design/15-bitbucket.md), read against a live Vercel integration on
`bitbucket.org/netfoundry/customer-connect-docs` — and stage 1 of its build order has landed.

- [x] **The marker is portable.** `scm.MarkerStyle`, `MarkerFor` and `HasMarker`; `findComment` matches with
      `HasMarker` rather than against one rendered string. Bitbucket escapes raw HTML, so `<!-- docpreview:… -->`
      would render as a visible paragraph there — Vercel ships exactly that defect on a public pull request. The
      working form is a CommonMark link reference definition. Done first and on GitHub alone, because a matcher that
      forgets a style posts a duplicate comment on every open pull request, and only GitHub has comments in the wild.
- [ ] `internal/scm/bitbucket` implementing `scm.Client`, writing `MarkerLinkRef`
- [ ] Webhook verification, comment upsert, diffstat for changed files
- [ ] **A repository access token, not app passwords** — those were removed 28 July 2026. Not an Atlassian account
      email plus API token either: that credential is a whole-account one.
- [ ] **`Repo.Name` must hold the slug, not the display name.** Bitbucket distinguishes them, and fork detection
      comparing the wrong one refuses every pull request.
- [ ] **`source.commit.hash` is 12 characters, not 40.** The client has to normalize, or `Report.Commit` silently
      holds two widths depending on the platform.
- [ ] Vault keys already reserved: `bitbucket.email`, `bitbucket.api_token`, `bitbucket.webhook_secret`

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

## Namespace hygiene

Each preview gets its own share, its own name, its own listener. The default `name_template` is
`{{.Repo.Name}}-{{.Name}}` — project and branch — because the branch alone collides across repositories and
every exposer keys something on the name. Deliberately not the commit SHA: the pull request comment is edited
in place, so the link a reviewer already opened has to survive the next push. `{{.HeadSHA}}` is available for
anyone who wants immutable per-commit URLs.

Cleanup is wired, not aspirational:

- `Exposer.Reap(ctx, keep)` runs at startup with `keep == nil` — nothing is live yet, so every share carrying
  the `docpreview:` target prefix is a leak from a previous process — and again on every sweep tick with the
  set of preview IDs the database still recognises.
- `Preview.TTL` tears down previews nobody has touched, which removes them from `keep` and so from the remote.
- The prefix is what stops it deleting shares an operator made by hand.

- [ ] **No audit command.** `Reap` logs what it deletes but there is no `docpreview shares list` to see what a
      zrok namespace or Frontdoor tenant currently holds versus what the database thinks. The gap that matters
      is a share created by a daemon whose database was then deleted: nothing claims it, and nothing looks.
- [ ] **Reap is untested against a live tenant** for zrok and Frontdoor. Covered by the same gap as their wire
      formats, below.

## Known limits, deliberate

Not bugs. Listed so nobody rediscovers them as surprises.

- **One docpreview per ziti service.** Binding creates a terminator; two instances create two and the router
  load-balances between them, so each holds a disjoint routing table. Give a second instance its own service.
- **The `local` exposer's URL moves between builds.** It allocates an ephemeral port per publish. zrok and ziti
  keep a stable name; `local` is for trying the pipeline, not for sharing a link.
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

Today the only way in is `docpreview vault set <key>` on the daemon's host, and the only scope is global:
`build.secrets` maps one environment variable to one vault key for every build, everywhere. Three scopes are
wanted, narrowest winning:

- [ ] **Global** — what exists now. Applies to every build.
- [ ] **Per project** — an Algolia key that belongs to one documentation site and has no business being in
      another project's environment. Needs a scope column on the vault entry and a lookup in
      `buildSecrets` (`cmd/docpreview/main.go`) keyed by `model.Repo`.
- [ ] **Per git provider** — the GitHub App private key and webhook secret are already provider-scoped by
      convention (`vault.KeyGitHubPrivateKey`); making that a real scope is what lets one daemon serve a
      GitHub org and a Bitbucket workspace without their credentials sharing a namespace.

What makes this harder than a form:

- **Write-only.** The UI must be able to set and delete a secret and never read one back. `vault.Secret`
  already refuses to render itself through Stringer, Formatter, GoStringer and json.Marshaler; the endpoint
  has to be equally deliberate, returning names and scopes and never values.
- **The dashboard has no authentication.** It is loopback-or-overlay today, and that is load-bearing: adding
  a write endpoint for credentials to an unauthenticated surface is worse than having no UI. Under the ziti
  listener the dialing identity is available on `edge.Conn` and is the natural authorization hook; under a
  TCP listener there is nothing, and this should probably refuse to serve at all.
- **Rotation has to re-arm the redactor.** `buildSecrets` runs once at startup and
  `NewBuilderWithSecrets` compiles the patterns from the values. A secret changed at runtime that does not
  rebuild the redactor is a secret that appears in the next build log verbatim.
- **Audit.** Who set what, when. Not who read it — nobody can.

## Improvements worth making

- [ ] **One shared status poller.** `statusPollInterval` runs per SSE connection, so each open dashboard costs
      two sqlite reads every 700ms. Fanning one poller out to subscribers — the way `buildlog.Writer` already
      does for log lines — removes that. Deferred because two open dashboards is the realistic ceiling.
- [ ] **`docpreview init` does not offer `ziti`.** `checkExposerKind` rejects it; `configure ziti` is the
      intended path, but the inconsistency will confuse someone.
- [ ] **Monorepo support.** One `.docpreview.yml` builds one site. A repository with several documentation
      sites cannot preview the one a pull request actually touched.
- [ ] **Log search.** The viewer tails and downloads; finding a line in a 5000-line build means downloading it.
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
  repository with the same branch name tore down the first one's publication. Now keyed by preview ID, with
  zrok and Frontdoor refusing a name another preview holds rather than taking it.
- `build.secrets` was dead config — nothing called `WithBuildSecrets`, so no secret was ever injected and the
  redactor was never armed. An unredacted log is indistinguishable from a log with no secrets in it.
- `Status` read only the preview table, which never holds `queued` or `building`, so the dashboard's counters
  read zero during a build and a branch building for the first time had no row at all.
- `init` writes to a temp file and validates before renaming, so a bad value cannot cost you a working config.
