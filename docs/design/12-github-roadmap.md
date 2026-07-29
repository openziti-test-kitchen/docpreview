# Building out GitHub

docpreview is a GitHub App that has never spoken to GitHub. Everything proven so far went through the local
simulator ([09-scm.md](09-scm.md)), which is an honest `scm.Client` but which cannot produce a 403, a paginated
comment list, a revoked installation token, or a second organization. This document is about the parts of the
GitHub integration that only a real installation will exercise, and the order to fix them in.

[11-github-setup-state.md](11-github-setup-state.md) is the working note for the first live run. This is what
comes after it.

## What is already there

The client implements the whole `scm.Client` surface, not a subset. `VerifyWebhook` authenticates before parsing
(`internal/scm/github/webhook.go:39`), `CloneURL` mints an installation-scoped HTTPS credential and URL-escapes
the token (`internal/scm/github/github.go:120`), `ChangedFiles` walks the pull-request files endpoint,
`Publish` upserts a comment by hidden marker, and `Retract` deletes it. The REST API version is pinned in a
header (`internal/scm/github/github.go:25`), which is the one thing that makes "it worked in July" mean something
in December.

The interesting gaps are not missing methods. They are the failure modes each method has never been made to
survive.

## The check run exists, and it is the wrong shape in one place

The Checks permission is used. `Publish` writes a check run after the comment, keyed by `(name, head_sha)`, and
treats a failure to write it as cosmetic (`internal/scm/github/github.go:201-209`). That division is right: the
comment is what a reviewer reads, and a status line that silently goes missing costs nothing.

What the check run adds over the comment is not decoration. A comment is prose in a thread; a check run is a row
in the merge box, it can be made a required status, it is attached to a commit rather than to the conversation,
and it survives a squash-merge as commit history. That is why `Retract` deliberately leaves it alone
(`internal/scm/github/github.go:378-380`). The comment is the human artifact; the check run is the one a branch
protection rule can act on.

Two things about the current implementation want changing. `findCheckRun` filters by `check_name` and takes the
first result (`internal/scm/github/github.go:319-335`) — the reference endpoint returns runs from every app that
wrote one, so a second app whose check happens to be called `docpreview` would be picked up and then PATCHed,
which fails with a permission error rather than creating our own run. Filtering by our own app id, if that filter
is available on the endpoint, or comparing the returned run's `app` field, closes it. And `output.summary` is set
to `RenderComment(r)` (`internal/scm/github/github.go:292`), so the marker HTML comment and the whole build-log
excerpt are duplicated into the check run. The summary should be its own short rendering, with the log in
`output.text` where GitHub gives it more room.

The larger missing piece is the *inbound* half of Checks. GitHub sends `check_run` deliveries with a
`rerequested` action when someone clicks "Re-run", and `VerifyWebhook` handles only `pull_request` and `ping`
(`internal/scm/github/webhook.go:46-58`). A "rebuild this preview" button that a reviewer already knows how to
find is the cheapest feature on this list, and it needs a `check_run` event subscription plus a mapping from the
run's `pull_request` back to a `model.PullRequest`.

## Installation tokens survive nothing yet

The authenticator caches one token per installation and discards it five minutes before expiry
(`internal/scm/github/auth.go:103-160`). That is correct and it is where the correctness stops.

A 401 does not invalidate the cache. `do` returns the error (`internal/scm/github/github.go:430`), the cached
entry stays exactly where it was, and every subsequent call reuses the dead token until the refresh margin
expires it — up to fifty-five minutes of a build reporting nothing. Installation tokens are revoked in practice:
the installation is suspended, its permissions are edited, the App's private key is rotated. The fix is small and
it belongs in `do`: on a 401, drop the cached token for that installation and retry once. Once, not in a loop,
because a genuinely wrong private key must fail rather than hammer.

The clone is safer than it looks. `CloneURL` is called once at the top of `runPipeline`
(`internal/daemon/daemon.go:699`) and git uses the credential only for the fetch that follows, so a build that
runs for forty minutes is not holding a token it will need later. A build longer than the token's remaining life
would fail at clone, before anything expensive.

`rewireGitHub` installs a client without validating it (`cmd/docpreview/main.go:476`). `validate` calls
`gh.Validate` only on the startup path (`cmd/docpreview/main.go:505`), so a private key pasted into the setup page
is never checked against `GET /app`; the first evidence of a wrong key is a build that fails at clone with an
authentication error. `rewireGitHub` should validate and surface the result on the setup page, which is where the
person who just pasted the key is still looking.

## Rate limits are classified but never acted on

`errorFromResponse` builds an `APIError` that knows whether it was rate limited and carries a `RetryAfter`
(`internal/scm/github/github.go:444-495`), and `Retryable` says whether repeating the call could work
(`internal/scm/github/github.go:462`). Nothing calls `Retryable`. There is no retry anywhere in the client, and
`report` logs a publish failure and moves on (`internal/daemon/daemon.go:938`). So the observable behaviour of one
rate-limited request is a pull request comment that says "Building" until the next push.

Three defects underneath that:

- `X-RateLimit-Reset` is parsed with `time.Parse(time.RFC3339, ...)` (`internal/scm/github/github.go:489`).
  GitHub sends that header as Unix epoch seconds, so the parse always fails and `RetryAfter` is always zero.
- The `Retry-After` header is not read at all. That is the header GitHub sends on the responses where it actually
  tells you how long to wait.
- A secondary rate limit is a 403 whose body says so, and it does *not* come with `X-RateLimit-Remaining: 0`. The
  classification requires that header to be zero (`internal/scm/github/github.go:486`), so every secondary rate
  limit is currently indistinguishable from a permissions error and is not retried. The conservative-403 reasoning
  in that comment is right — most 403s are permissions — but the body message is the discriminator, and it is
  already parsed into `Message`.

Retry belongs in `do`, not in each caller: bounded attempts, honouring `Retry-After` then `X-RateLimit-Reset` then
a small exponential backoff, capped so it cannot outlive the report's 30-second detached context
(`internal/daemon/daemon.go:935`). And the final report of a build — the one carrying the URL — deserves more than
best effort: if it cannot be written, the comment is wrong until a human pushes again. A small persistent
"reports owed" table, drained by the same worker loop, is the honest fix. It is also the one that changes the most
code, which is why it is not first.

## Pagination is handled in one place and unbounded in another

`ChangedFiles` pages at 100 with a hard stop at 30 pages and logs when it truncates
(`internal/scm/github/github.go:159-192`). GitHub caps that endpoint at 3000 files anyway and the comment on the
constant makes the right argument: at that size the answer is "build it". This one is fine.

`findComment` is an unbounded `for page := 1; ;` loop (`internal/scm/github/github.go:251-273`). It terminates on
a short page, which it will always eventually get, but nothing bounds the cost, and it runs on every single
report — four times per build in the common queued/building/ready path, plus once more for `Retract`. On a pull
request with 400 comments that is 16 list requests per build for one comment id. Two changes: cap the walk the way
`ChangedFiles` is capped, and cache the comment id in memory keyed by preview id, verifying the marker on the
PATCH response and falling back to the walk on a 404. The reasoning at
`internal/scm/github/github.go:243-250` — that persisting comment ids means duplicate comments after a restore —
stays intact, because an in-memory cache that is empty after a restart just does the walk.

## The fork refusal is real, and it has one hole

It exists, it is where 10-security.md says it is, and a test guards it
(`internal/scm/github/webhook.go:138`, `internal/scm/github/webhook_test.go:119`). The comparison is
case-insensitive against `Repo.Slug()`, which is right for GitHub's case-preserving-but-insensitive logins.

The hole is `head != nil`. When the head repository has been deleted — a fork whose owner removed it after opening
the pull request — GitHub sends `head.repo` as null, the guard is skipped, and the event becomes a build. Nothing
downstream re-checks, because `model.PullRequest` carries no fork bit at all
(`internal/model/model.go:50-69`), so the daemon has no way to assert the invariant it claims. A null head repo
should be refused, not permitted, and the payload should be normalized into an explicit field on
`model.PullRequest` so `handleBuild` can refuse a second time. An invariant enforced in exactly one `if` inside
one platform client is an invariant the Bitbucket client will forget.

The refusal is also silent: a log line and nothing on the pull request
(`internal/scm/github/webhook.go:139`). A contributor from a fork sees no preview and no explanation. That wants a
comment, and a comment means an API call from the refusal path, which means the refusal has to stop being a
`return nil, nil` from inside `VerifyWebhook`. An `EventIgnore` kind carrying a reason — which 09-scm.md already
describes and `scm.EventKind` does not define (`internal/scm/scm.go:106-111`) — is the shape that fits.

Supporting forks eventually is a build-isolation problem, not a webhook problem. The safe version is: the docker
driver only, no operator secrets injected, a clone URL with no installation token in it since a fork's head is
public by definition, and an explicit maintainer action per pull request — a label applied by someone with write
access, re-checked on every `synchronize` so that a push after labelling does not inherit the approval. Every one
of those is load-bearing and none of them is worth building before the single-org case works.

## Two organizations break the URL, not the auth

The token cache is keyed by installation id (`internal/scm/github/auth.go:58`) and `PreviewID` includes the owner
(`internal/model/model.go:76`), so two installed organizations do not collide in the vault, the database, the
artifact directory, or the token map. Auth is fine.

The public name is not. The default template is `{{.Repo.Name}}-{{.Name}}`
(`internal/expose/expose.go:143`) and omits the owner, so `acme/docs` and `beta/docs` with the same branch name
render the same hostname, and every exposer keys a public address on that name. The second one to publish either
collides or steals the first's URL. Including the owner in the default template is a one-line change with a
migration cost — every live preview's URL moves — which argues for making it before anyone depends on the current
URLs rather than after.

The queue is also global and FIFO (`internal/daemon/daemon.go:557`). One organization opening twenty pull requests
delays another organization's single one. That is acceptable for a self-hosted tool serving one team and it is
worth writing down before somebody discovers it as a bug.

Nothing handles the App being uninstalled. The App subscribes only to pull-request events
(11-github-setup-state.md) and `VerifyWebhook` handles nothing else, so removing docpreview from a repository —
or from an organization entirely — leaves every preview published, every share alive, and every comment in place.
Teardown happens only on `pull_request.closed`, and only when `preview.teardown_on_close` is set
(`internal/daemon/daemon.go:495`). Subscribing to `installation` and `installation_repositories` and mapping
`deleted`/`removed` onto the existing teardown path is the fix, and it is the one gap on this list that leaves
resources running with no way for the affected user to stop them.

## Installing the App opts every pull request in

There is no allowlist. `config.GitHubConfig` is an app id and an api base (`internal/config/config.go:357`), and a
missing `.docpreview.yml` yields defaults rather than a refusal (`internal/config/config.go:704`). So installing
on a repository means every pull request is cloned, `.docpreview.yml` loaded, `ChangedFiles` fetched, and detection
run — and, worse, a "queued" comment and check run are written *before* any of that
(`internal/daemon/daemon.go:485`), so a pull request that turns out to touch no documentation still gets a comment
that says "Queued" and is then edited to say "Skipped".

Three mechanisms are possible and they answer different questions:

- **An allowlist in the server config** answers "which repositories may this daemon spend CPU on". It is the
  operator's decision, it needs no cooperation from the repository, and it is the only one of the three that can
  refuse before cloning.
- **Presence of `.docpreview.yml`** answers "does this repository want previews". It is self-service, it lives with
  the code, and it cannot be evaluated until after the clone — which is most of the cost.
- **A label** answers "does this *pull request* want a preview". It is per-PR and it needs the `labeled` action,
  which is currently ignored (`internal/scm/github/webhook.go:156`).

Both of the first two, and not the third. The allowlist because an operator running a build host must be able to
say what runs on it, and it is the only gate that works before the clone. The `.docpreview.yml` requirement
because a repository with no config file has expressed no interest, and defaulting a repository into being built
is how a documentation tool becomes noise on every backend pull request. A label is the wrong default — it makes
the common case require a manual step — but it is the right mechanism for the fork case above, where a human
decision is exactly what is wanted.

Independently of the gate: the "queued" report should not be written until detection says there is something to
build. Skipping straight from nothing to a comment only when `decision.Build` holds removes the entire
queued-then-skipped noise class, at the cost of losing the "we saw your push" signal on slow builds. Reporting
`queued` only when the queue is actually backed up keeps both.

## The comment, and the revision counter that is not in it

`RenderComment` emits the marker, a table, an optional reason, and a collapsed build-log excerpt
(`internal/scm/comment.go:24-67`). It does not emit a revision counter. The counter lives in the store's
`comments` table (`internal/store/store.go:288-320`) and is rendered only by the local simulator's `/pr` page
(`internal/daemon/ingress.go:265`), because only the local client calls `UpsertComment`
(`internal/scm/local/local.go:279`). The example in 09-scm.md shows "revision 6" in a GitHub comment; no GitHub
comment has ever contained one.

That is a real gap rather than a documentation typo. The counter's stated purpose — telling "still building" from
"stuck" — is exactly the thing a reviewer on GitHub cannot currently determine, since the timestamp alone does not
say how many times the comment has been rewritten. Either the GitHub client records into the same table and the
renderer emits the count, or 09-scm.md stops claiming it. The first is better and it is nearly free: the store
call already exists.

## Actions handled, and one that matters

`opened`, `synchronize`, `reopened` and `ready_for_review` build; `closed` tears down; `converted_to_draft` is
deliberately a no-op because drafts are where documentation gets written; everything else is a debug line
(`internal/scm/github/webhook.go:131-159`). The draft policy is the right one, and note that `Draft` is decoded
from the payload and never read (`internal/scm/github/webhook.go:92`) — either use it or drop it, because a parsed
and unused field reads like a policy that exists.

The omission worth fixing is `edited`. Changing a pull request's base branch changes the merge base, which changes
what `ChangedFiles` returns, which can change the build decision — and no event triggers a rebuild, so the
preview quietly describes a diff that no longer exists. `edited` with a `changes.base` key should be treated as a
build event; `edited` for a title or body change should not.

## Interface obligations GitHub meets weakly

`Validate` is not part of `scm.Client` (`internal/scm/scm.go:69-101`), so `cmd/docpreview` type-asserts to the
concrete client to call it (`cmd/docpreview/main.go:505`). With three implementations and a startup check that
wants to run against all of them, it belongs on the interface.

`EventIgnore` is documented in 09-scm.md and does not exist in the code; both the fork refusal and every ignored
action return an empty slice, which discards the reason at the point where a person asking "why did nothing
happen when I pushed" needs it. Adding the kind, with a reason string, makes ignoring visible in the dashboard's
activity feed instead of only in the log.

Delivery ids are carried into events (`internal/scm/scm.go:118`) and used for logging only. Nothing deduplicates
a redelivery. Since the ingress answers 202 before doing the work (`internal/daemon/ingress.go:207`) GitHub has
little reason to redeliver, and a duplicate delivery costs one superseded build, so this is a deliberate limit
rather than a defect — but it is one worth stating, because the obvious fix (a seen-deliveries table) is a
tempting thing to build for no benefit.

## GitHub Enterprise

`api_base` is honoured for API calls, and `CloneURL` derives the git host from it by stripping the path, on the
correct assumption that GHE serves the API at `https://host/api/v3` and git at `https://host`
(`internal/scm/github/github.go:126-133`). That is the whole of the GHE support, and for the App auth flow it may
well be enough — the JWT, the installation-token exchange, and the webhook HMAC are the same on GHE.

What is unhandled: nothing probes the GHE version, and `X-GitHub-Api-Version` is a dotcom concept whose behaviour
on a given GHE release is not something this codebase knows. Rate-limiting can be disabled entirely on a GHE
instance, in which case the headers the error classifier reads are simply absent — which the retry logic must
treat as "no guidance" rather than "no limit reached". And a GHE instance behind a private CA needs a trust store
knob that `config.GitHubConfig` does not have. None of this should be built speculatively; it should be built when
someone has a GHE instance to test against, and until then the honest statement is that GHE is untested.

## The order to build it in

1. **Run the smoke test in 11-github-setup-state.md.** Everything below is a guess about which failure modes
   matter until one real installation has posted one real comment.
2. ~~**Invalidate the cached token on a 401 and retry once.**~~ **Done.** `do` splits into a retrying wrapper
   over `doOnce`; a 401 calls `authenticator.invalidate` and retries exactly once. A flag rather than a counter,
   so a genuine auth failure costs two requests instead of looping. A 401 is pre-authorization, so the retried
   request cannot duplicate the effect of the first — which is what makes retrying a `POST` safe here.
3. ~~**Fix the rate-limit classification.**~~ **Done.** `X-RateLimit-Reset` now parses as epoch seconds,
   `Retry-After` is read and takes precedence, and a secondary limit is recognised by that header or by the
   message rather than by a remaining count that is non-zero for exactly that case. A zero `RetryAfter` now means
   "no idea" rather than "retry now". Covered by `internal/scm/github/api_test.go`.

   The retry loop landed with it. `do` repeats a rate-limited request up to `rateLimitAttempts` times, waiting
   what GitHub asks and abandoning the wait if the context ends — reporting the rate limit alongside the
   cancellation, since the limit is the thing an operator acts on. A 5xx is **not** retried despite `Retryable`
   saying it could be: a rate limit is refused before GitHub acts, but a 500 may have created the comment before
   failing to say so, and the upsert finds-then-creates. Making 5xx safe needs a re-find before each retry, which
   belongs with the upsert rather than in the transport.
4. **Close the fork hole and put the fork bit on `model.PullRequest`.** A null head repo currently builds, and the
   invariant that 10-security.md calls first needs to be assertable somewhere other than the client that already
   checks it.
5. **Handle `installation` and `installation_repositories` deletions.** This is the only gap that leaves
   previews serving with no way for the affected repository to stop them.
6. **Add the repository allowlist and require `.docpreview.yml`, and stop reporting `queued` before the build
   decision.** These three together are what makes the App installable on an organization without becoming noise
   on every pull request.
7. **Put the owner in the default name template.** A one-line change whose cost is that every existing preview
   URL moves, so it is far cheaper before real users than after.
8. **Bound `findComment` and cache the comment id in memory.** Pure cost reduction; it matters once several
   repositories with long-lived pull requests are live, and not before.
9. **Record and render the revision counter on GitHub.** Closes the gap between what 09-scm.md promises a
   reviewer and what the comment actually shows.
10. **Handle `edited` with a base-branch change, and `check_run.rerequested`.** The first fixes a preview that
    silently describes the wrong diff; the second gives reviewers a rebuild button in a place they already look.
11. **Add `EventIgnore` with a reason, and move `Validate` onto `scm.Client`.** Interface cleanups that pay off
    across all three implementations, best done once the events above have settled.
12. **A persistent "reports owed" queue.** The largest change here, and the only real fix for "the comment says
    Building because GitHub was down for ninety seconds". Everything above reduces how often it happens, which is
    why this comes last.
13. **Forks, under docker, with maintainer approval per pull request.** Only after 4, only with the docker driver
    as the default for those builds, and only if there is a repository that needs it.

## Not verified

- Whether the "List check runs for a Git reference" endpoint accepts an `app_id` filter. The recommendation to
  scope `findCheckRun` to our own app stands either way, via the `app` field on each returned run, but the query
  parameter is asserted from memory and should be checked against the current API reference.
- The exact shape of GitHub's secondary-rate-limit response — that it is a 403 with an explanatory message and
  usually a `Retry-After` header, without `X-RateLimit-Remaining: 0`. The retry code must be written against an
  observed response, not this description.
- That `X-RateLimit-Reset` is Unix epoch seconds. The code's RFC3339 parse is wrong under either reading, since
  GitHub does not send RFC3339 there, but the replacement should be confirmed against a live header.
- That GitHub sends `head.repo` as null when a fork is deleted. The `head != nil` branch exists in the code, so
  someone believed it can be null; refusing on null is correct regardless of the cause.
- Whether GitHub sends `pull_request.edited` with a `changes.base` key, and its exact structure.
- Everything about GHE. No GHE instance was available; the claim that api_base plus host derivation is sufficient
  is inference from the dotcom flow, not a test.
- Rate-limit and pagination behaviour generally. The App has never made an authenticated request to GitHub, so
  every claim about what GitHub returns is from documentation and memory rather than from a captured response.
