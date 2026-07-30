# Source control integration

## The interface

```go
type Client interface {
    Platform() model.Platform
    VerifyWebhook(ctx, headers map[string][]string, body []byte) ([]Event, error)
    CloneURL(ctx, pr) (string, error)
    ChangedFiles(ctx, pr) ([]string, error)
    Publish(ctx, Report) error
    Retract(ctx, pr) error
}
```

`Publish` takes a `Report`, not a string. The client renders it. That is what keeps the comment identical across
platforms and keeps `daemon` from knowing Markdown.

Events are `EventBuild`, `EventTeardown`, `EventIgnore`. One delivery can produce several — a push touching
three pull requests is three events — which is why `VerifyWebhook` returns a slice.

## One sentinel for "not authentic"

`scm.ErrBadSignature` (`internal/scm/scm.go:78`) is the error every client returns when a delivery cannot be
authenticated. The ingress tests for it to choose 401 over 400 (`internal/daemon/ingress.go:200`).

It used to live in the github package, and the ingress tested for that one. So a verification failure on any other
platform fell through to the generic branch and answered **400** — which tells a caller probing for a valid secret
that its guess was structurally fine, and hands it an oracle the 401 path is written to deny. The local client
returns the shared sentinel now (`internal/scm/local/local.go:149`); `github.ErrBadSignature`
(`internal/scm/github/webhook.go:23`) is an alias so existing callers and tests keep compiling.

**Deliberate limit.** The HMAC check itself is still duplicated: `verifySignature` exists in
`internal/scm/github/webhook.go:65` and again in `internal/scm/local/local.go:209`, byte for byte. Two copies of
twelve lines is cheaper than a shared helper nobody can find; extracting it is worth doing when a third caller
appears — Bitbucket, which signs differently, will be that caller and will show what the shared shape should be.

## The single-comment protocol

The requirement was "update, don't duplicate". A twelve-commit pull request must not have twelve bot comments.

```markdown
<!-- docpreview:<previewID> -->
| | |
|---|---|
| **Preview** | https://mydocs-new-install-guide.share.zrok.io/ |
| **Status** | ✅ Ready |
| **Commit** | `4f0c2a1` |
| **Updated** | 2026-07-28 14:22 UTC · revision 6 |
```

The marker is one line in the body, and it is the whole of how the comment gets edited instead of duplicated: there
is no platform API for "the comment I made earlier", and storing comment IDs in the database means a restored backup
or a fresh install starts spamming. So the comment identifies itself. Upsert is: list the pull request's comments,
find the one whose body `scm.HasMarker` recognises, `PATCH` it if found, `POST` if not.

The marker carries the preview ID rather than a fixed string, so two docpreview instances watching the same
repository — staging and production, say — do not fight over one comment.

**Invisible is a property of the renderer, not of the syntax**, and that turned one function into three
(`internal/scm/scm.go:152-221`):

| | |
|---|---|
| `MarkerStyle` | which syntax, because the protocol is platform-neutral and the syntax is not |
| `MarkerFor(id, style)` | renders one. `MarkerHTMLComment` is `<!-- docpreview:<id> -->`; `MarkerLinkRef` is `[docpreview]: #<id>` |
| `HasMarker(body, id)` | matches **every style there has ever been**, which is what every caller must use |

`scm.Marker(previewID)` remains, as the one-argument form meaning "the style GitHub wants" — every caller writing a
comment today writes a GitHub one, and a platform needing the other reaches for `MarkerFor` rather than changing
what `Marker` means.

`MarkerLinkRef` exists because Bitbucket Cloud escapes raw HTML, so an HTML comment renders there as a visible
paragraph of literal text at the bottom of the comment. Not a guess: Vercel's own integration ships
`<!-- vercel-commit-author-required -->` and Bitbucket renders it as `<p>&lt;!-- … --&gt;</p>` on a public pull
request, while their *other* comment uses a link reference definition and vanishes from the rendered HTML entirely.
A CommonMark link reference definition is consumed as a definition and emits nothing, whether or not the renderer
allows raw HTML. Its destination is `#`-prefixed so that a renderer which *does* resolve the label produces a
same-page anchor rather than a request to somewhere. See [15-bitbucket.md](15-bitbucket.md).

**A style may be added to `HasMarker` and none may ever be removed.** This is the load-bearing half of having two,
and the reason introducing `MarkerLinkRef` was not simply "change the constant": a daemon upgraded across a style
change finds comments it wrote in the old one, and a matcher that knew only the new style would post a second
comment on every open pull request at once — the exact failure the marker exists to prevent. The comments live on
somebody else's server and outlive every release of this program.
`TestHasMarkerMatchesEveryStyleEverWritten` (`internal/scm/marker_test.go:16`) enumerates the styles for that
reason, and `TestFindCommentMatchesEitherMarkerStyle` (`internal/scm/github/findcomment_test.go:49`) pins the same
property one layer up, where the comment list actually comes from the API.

The revision counter exists so a reviewer can tell "still says building" from "stuck". Demonstrated climbing
3 → 6 on one comment.

**Rendering is deterministic and stays that way.** `RenderComment` is a pure function of `Report`. An agent
writing free text through the app's installation token would be an unaccountable actor with write access to
every repository the app is installed on; any generated content belongs in a clearly-labelled `<details>` block
inside a comment the code still renders. See `TODO.md`.

## GitHub

Auth is a GitHub App, not a personal access token: installation tokens are scoped to the repositories the app
is installed on and expire in an hour, where a PAT carries the whole user.

- A JWT signed with the app's RSA private key (RS256), 10-minute expiry
- Exchanged for an installation token per installation, cached until shortly before expiry
- The private key and webhook secret live in the vault

`VerifyWebhook` checks `X-Hub-Signature-256` with `hmac.Equal`. The body must be fully read before it can be
verified, which is why `maxWebhookBody` caps it — GitHub's own limit is 25 MB, and accepting that from an
internet-facing endpoint means an attacker with or without a valid signature can make the process allocate it.

A failed verification returns **401 with no explanation**. Saying which part of the signature was wrong is a
hint.

### The request path retries two things, and deliberately not a third

`Client.do` (`internal/scm/github/github.go:431`) wraps `doOnce` in a loop over exactly two cases.

**A 401, exactly once, with a fresh installation token.** A cached token can die without expiring: the App's
permissions change, the installation is suspended, someone reinstalls. None of those move the clock, so
`tokenRefreshMargin` never notices, and the daemon holds a dead token while every request 401s until the cached
expiry finally passes — up to the better part of an hour of a repository looking broken for no visible reason. A
401 is the only signal GitHub gives, so `authenticator.invalidate` (`internal/scm/github/auth.go:172`) is called
from the request path. It is guarded by a **flag, not a counter** (`triedFreshToken`): a genuine auth failure — a
wrong app ID, a revoked App — then costs two requests instead of looping. Retrying a POST here is safe because a
401 is refused *before* authorization, so nothing was created by the attempt that failed.

**A rate limit, up to `rateLimitAttempts` times** (three, `internal/scm/github/github.go:404`), waiting exactly as
long as GitHub asks. If the context ends inside the wait, the return is `errors.Join(ctxErr, apiErr)` — the
cancellation *and* the rate limit, because the limit is what the operator has to act on and a bare
`context.Canceled` names nothing actionable.

**Not a 5xx**, even though `APIError.Retryable` says it could be (`internal/scm/github/github.go:551`). The
asymmetry is about effects, not about likelihood of success: a rate limit is refused before GitHub acts, so
repeating it cannot repeat an effect, but a 500 may well have created the comment and then failed to say so. The
comment upsert finds-then-creates (`upsertComment`, `internal/scm/github/github.go:213`), so a retry after that
posts a **second** comment and breaks invariant 1. Making 5xx safe needs a re-find before each retry, or an
idempotency key; either belongs with the upsert, not in the transport, which is why `Retryable` stays honest about
what *could* be retried and `do` stays conservative about what *is*.

### Rate-limit classification was inert before this

`errorFromResponse` (`internal/scm/github/github.go:555`) sets `RateLimited` and `RetryAfter`. Both halves were
broken in ways that only showed up under load.

- **`X-RateLimit-Reset` is epoch seconds** and was parsed as RFC3339, which never matches, so every rate-limited
  response reported a zero wait. Anything acting on it would have retried immediately into the same limit. It is
  `strconv.ParseInt` and `time.Unix` now (`internal/scm/github/github.go:633`).
- **A secondary rate limit is a 403 with a *non-zero* remaining count.** That is the one a supersede storm hits —
  it fires on burst rather than on volume. The remaining-count test alone classified it as a permissions error, so
  a wait-and-retry became a failed build and a comment stuck on "Building". The fix adds a second case: a 403
  carrying `Retry-After`, or whose message contains GitHub's secondary-limit wording
  (`isSecondaryLimitMessage`, `internal/scm/github/github.go:605`). The string match is the fallback, never the
  primary test — `Retry-After` is what GitHub documents sending.

Every 403 is still not retryable. A permissions problem is the commoner cause of one, and treating them all as
transient spins forever on a misconfigured App.

`RetryAfter` needs a companion boolean. `retryAfterFrom` returns `(duration, ok)` and `APIError.retryAfterKnown`
carries it (`internal/scm/github/github.go:540`): **zero with the header present means "retry now", absent means
"no idea"**. They are opposite instructions that both leave the duration at zero, and collapsing them made every
explicit zero turn into a pointless `rateLimitFallback` delay.

Tests are in `internal/scm/github/api_test.go`, which stands a fake `api.github.com` in front of the client through
the existing `api_base` config field — the same field GitHub Enterprise uses, so the seam is production code rather
than a test hook. It covers the epoch parsing, both secondary-limit shapes, the genuine 403, the retry bound, the
zero-versus-absent distinction, context cancellation mid-wait, the 5xx non-retry, and both 401 paths.

### Fork pull requests are refused

Building one executes a stranger's `package.json` under an installation token. There is no flag for this and
there should not be. See [10-security.md](10-security.md).

## The local simulator

`internal/scm/local` is a real `scm.Client` against **bare git repositories on disk**. It exists so the entire
loop — push, detect, build, publish, comment, update — runs with no GitHub account, no network, and no tunnel.

`docpreview sim init <name>` creates a bare repo with a generated `post-receive` hook that `curl`s
`/webhook/local`. **Nothing monitors git** — no polling, no file watching. A push is a webhook delivery, exactly
as on GitHub; only the sender differs.

The hook is the *only* simulated part. Everything downstream of the webhook is the production path. A mock that
short-circuited the queue would not have caught the supersede races, and a fake builder would not have caught
the base-URL failures.

Three decisions in the hook:

- **Pushing the base branch does nothing.** A preview belongs to a branch under review.
- **The pull request number is `cksum` of the branch name**, so repeated pushes update one comment rather than
  opening a new one each time. Collisions across branches are possible and harmless.
- **Deleting a branch sends `closed`**, which is how the teardown path gets exercised.

`sim init` refuses when the ingress has no TCP listener. The hook is a `curl`, so an overlay-only configuration
has nowhere to post, and a hook written against an empty address fails on every push with a bare curl error
rather than anything that names the cause.

There is nowhere to put a comment, so `GET /pr` renders the same `Report` as a page — the identical renderer
that produces the GitHub comment.

`ChangedFiles` is `git diff --name-only base...head`. Three dots: changes since the branch diverged, not since
the tip of base, or every commit landing on main would show as a change to every open branch. The hosted
clients ask the platform because a depth-1 clone lacks the history; here the bare repository *is* the platform.

`repoPath` rejects a name containing `/`, `\`, `:`, or equal to `.` or `..`, and requires the directory to
exist. The name arrives from a webhook and selects a directory to run git against.

`gitOutput` captures stderr separately and runs it through `scrubGitOutput`, because git puts the userinfo
component of a URL into its own error messages.

**`/webhook/local` has no signature.** It is unauthenticated by design, for a loopback development loop against
repositories you created. It must not be enabled on a daemon whose ingress is reachable by anyone untrusted.

This is not a test fixture. It is how the demo runs, and it is how every bug in the exposer and concurrency
sections was found.

## Bitbucket

Interface only; `POST /webhook/bitbucket` returns 501. Vault keys are reserved (`bitbucket.email`,
`bitbucket.api_token`, `bitbucket.webhook_secret`). App passwords are dead as of June 2026 — Atlassian account
email plus API token only.

## Report and state

```go
type Report struct {
    PR        model.PullRequest
    PreviewID string
    State     State      // queued | building | ready | skipped | failed
    URL       string
    Name      string
    Commit    string
    Reason    string     // already scrubbed
    Duration  time.Duration
    UpdatedAt time.Time
}
```

`Reason` is scrubbed by the daemon before it gets here (`d.scrub`), because it carries build output and a
comment is the most public place a leaked credential could land.

`report` uses a **detached context with a 30-second timeout**, so a cancelled build can still say why it
stopped.

A superseded build writes no report at all: a newer build is already updating the same comment, and a late
failure would overwrite a perfectly good "ready".

## Invariants

1. One comment per pull request per docpreview instance, matched by marker.
2. Comment rendering is a pure function of `Report`.
3. An unverified webhook is rejected with 401 and no diagnostic detail, on **every** platform. A client that
   cannot authenticate a delivery returns `scm.ErrBadSignature` and nothing else.
4. The webhook body is capped before it is read.
5. Fork pull requests never reach the builder.
6. `Reason` is scrubbed before it reaches a client.
7. A superseded build publishes nothing.
8. No request whose failure might have taken effect is retried in `do`. Adding a status to the retry set requires
   showing that repeating it cannot repeat an effect, or making the caller idempotent first.
