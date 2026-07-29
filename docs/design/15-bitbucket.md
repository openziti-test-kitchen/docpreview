# Bitbucket

Bitbucket is the reason `scm.Client` exists. It is also the platform for which nothing has been written. This
document is the plan, and the first half of it is an inventory: how much of the anticipation in the tree is
actually load-bearing, and how much is a comment promising work.

Read [09-scm.md](09-scm.md) first. Everything here is a second implementation of what that document describes.

## What is already there, and what it is worth

Real:

- `model.PlatformBitbucket` exists (`internal/model/model.go:24`), and `PreviewID` mixes the platform into the
  hash (`internal/model/model.go:75`), so a GitHub `acme/docs#7` and a Bitbucket `acme/docs#7` are already
  different previews with different artifact directories and different comment markers. Nothing needs changing
  for two platforms to run in one daemon.
- `POST /webhook/bitbucket` is routed (`internal/daemon/ingress.go:100`) and answers 501 through the generic
  "no client for this platform" path (`internal/daemon/ingress.go:172`), not a special case. Install a client
  with `Ingress.SetClient` and the route starts working with no edit to the mux.
- Three vault keys are reserved: `bitbucket.email`, `bitbucket.api_token`, `bitbucket.webhook_secret`
  (`internal/vault/vault.go:58`).
- `model.PullRequest` documents its own Bitbucket semantics: `Number` is the pull request ID, `InstallationID`
  is unused (`internal/model/model.go:53`, `:67`).

Not real, despite appearances:

- There is no Bitbucket stub. `docs/design/01-architecture.md:31` and `README.md:78` say "a stub for Bitbucket
  that returns 501"; the 501 comes from the absent map entry, and `internal/scm/` contains only `github` and
  `local`. Nothing implements the interface, so nothing has forced the interface to be honest about two hosted
  platforms.
- The three vault keys are reserved in name only. `knownKeys()` (`internal/daemon/secrets.go:209`) lists the two
  GitHub keys and the Frontdoor token; the setup page has no Bitbucket rows, so the credentials cannot be
  entered anywhere except `docpreview vault set`.
- `config.Server` has no `Bitbucket` field (`internal/config/config.go:53`), so there is nothing to switch on.
- `internal/daemon/ingress.go:189` tests `errors.Is(err, github.ErrBadSignature)`. A Bitbucket client's own
  bad-signature error would be answered **400 "bad request" and logged at error level**, not 401 with no
  diagnostic. That breaks invariant 3 of 09-scm.md for the new platform, and it is already broken for the local
  client, whose verification failure is a bare `fmt.Errorf` (`internal/scm/local/local.go:145`). The sentinel is
  in the wrong package.

So the anticipation is worth about a day. The interface fits; the wiring does not generalize yet.

## Authentication

The reserved key names — an email and an API token — encode a decision that was correct when it was made and is
now the only remaining option for a user credential. Bitbucket Cloud app passwords are being switched off; an
Atlassian account email as the username plus an Atlassian **API token** as the password, over HTTP Basic, is the
replacement. Nothing in the naming is wrong. `bitbucket.email` looks odd next to `github.private_key` only
because Basic auth is odd: the username half of a credential is not usually a secret, and it is in the vault
here because it is half of an `Authorization` header and because it also has to be percent-encoded into a clone
URL, which is a place secrets go.

The security difference from GitHub is not a detail of header construction. A GitHub App private key mints a
ten-minute JWT which mints a **one-hour installation token scoped to the repositories that installation covers**
(`internal/scm/github/auth.go:31`, `:103`). Three properties fall out of that, and docpreview relies on all
three: a token that leaks into a log expires by itself; a token cannot reach a repository the app was not
installed on; and the comment is attributed to the app rather than to a person.

An Atlassian API token has none of them. It does not expire on a schedule the daemon can rely on, it carries
**everything the human it belongs to can see** — every workspace, every repository, Jira as well if the token is
not scoped — and every comment docpreview writes is written by that human. If the vault leaks, the blast radius
is a person's whole Atlassian identity rather than one app installation's repository list. That is the price of
the platform, not a defect in this design, and it belongs in the operator documentation next to the
recommendation to use a dedicated bot account rather than a maintainer's own.

There is a partial answer and the design should reach for it. Bitbucket Cloud has repository, project, and
workspace **access tokens**: bearer credentials that belong to a resource rather than to a user, with a scope
list, revocable individually. A repository access token is the closest thing Bitbucket has to an installation
token — it is scoped like one, and it is not a person — and it fails only the "expires on its own" property. So:

```go
// BitbucketConfig, internal/config/config.go
type BitbucketConfig struct {
    Enabled bool   `yaml:"enabled"`
    APIBase string `yaml:"api_base"`

    // Auth is "access_token" (a repository or workspace access token, preferred)
    // or "api_token" (an Atlassian account email plus API token).
    Auth string `yaml:"auth"`
}
```

Two auth modes, one `authorizer` type inside the client that turns a mode plus the vault into an
`Authorization` header, chosen once at construction. `access_token` reads a fourth vault key
(`bitbucket.access_token`) and sends `Bearer`; `api_token` reads the two reserved keys and sends `Basic`. Default
`api_token`, because that is what the reserved keys already imply and what an operator following today's
`www/docs/reference/cli.md:582` table will have stored — but `doctor` should say plainly which mode is active and
that the other one is narrower.

Do **not** infer the mode from which keys happen to be present. A vault mid-setup contains a subset of
everything, and a client that silently picks the wider credential because the narrower one is not stored yet is
the kind of magic that is discovered during an incident.

The token is long-lived, which makes one more thing worth doing that GitHub did not need: register it with the
redactor (`internal/redact`) the way `build.secrets` values are. An expiring installation token in a log is an
embarrassment; a non-expiring one is a credential.

## Webhook verification

Bitbucket Cloud webhooks can carry a secret, and the delivery is signed with HMAC-SHA256 over the raw body — the
same construction as GitHub's, with a different header name. The header is **not** `X-Hub-Signature-256`; see
*Not verified*, because getting this wrong means every delivery is rejected and the error says nothing. The
event name arrives in `X-Event-Key` (`pullrequest:created` and so on) rather than `X-GitHub-Event`, and the
delivery identifier is `X-Request-UUID` rather than `X-GitHub-Delivery`.

The comparison logic itself is identical, which is exactly the problem: `verifySignature` already exists twice,
at `internal/scm/github/webhook.go:62` and `internal/scm/local/local.go:205`, byte for byte. Bitbucket would be
the third copy of a constant-time HMAC check, and the third place someone could accidentally write `==`. It
moves to `scm` before this client is written, not after.

```go
// scm/webhook.go
var ErrBadSignature = errors.New("webhook signature verification failed")

// VerifyHMACSHA256 checks header, of the form "<prefix><hex>", against the
// HMAC-SHA256 of body under secret.
func VerifyHMACSHA256(secret, body []byte, header, prefix string) bool
```

`prefix` is a parameter rather than a constant because the two platforms disagree about the header name and may
disagree about the prefix; the digest and the comparison are the parts worth having one copy of.

The order of operations in `VerifyWebhook` is not negotiable and the reasons are already written down at
`internal/scm/github/webhook.go:22`: verify before parsing, because the endpoint is internet-facing by design
and the JSON parser must never see unauthenticated bytes. The body cap is upstream in the ingress
(`internal/daemon/ingress.go:31`) and platform-independent, so Bitbucket inherits it for free.

**Missing secret is a hard failure, not a downgrade.** Bitbucket lets a webhook be configured without one, and a
client that accepted unsigned deliveries when `bitbucket.webhook_secret` is absent would turn a forgotten field
in a web form into an unauthenticated build trigger. The local client is allowed to skip the signature
(`internal/scm/local/local.go:136`) because it is a loopback development loop; a hosted platform is not.

## Event mapping

The interface has two actionable kinds, `EventBuild` and `EventTeardown` (`internal/scm/scm.go:106`). The
mapping:

| `X-Event-Key` | Kind | Note |
|---|---|---|
| `pullrequest:created` | `EventBuild` | GitHub's `opened` |
| `pullrequest:updated` | `EventBuild` | see below — this is the awkward one |
| `pullrequest:fulfilled` | `EventTeardown` | merged |
| `pullrequest:rejected` | `EventTeardown` | declined |
| `repo:push` | ignore | previews belong to pull requests |
| everything else | ignore | approvals, comments, tasks |

`pullrequest:updated` is not GitHub's `synchronize`. It fires for a new commit on the source branch *and* for a
title change, a description edit, a reviewer change, and a retarget. Mapping it to `EventBuild` therefore
rebuilds a site because somebody fixed a typo in a PR description.

Accept that, deliberately. The alternative is remembering the last SHA built per pull request so the client can
suppress a no-op, and the client is the wrong place for that state: it would have to survive a restart, which
means the store, which means handing a hosted `scm.Client` a `*store.Store` the way `local.New` is handed one —
and a client that reads the database to decide what a webhook means is a client that behaves differently on a
fresh install. The machinery to absorb the churn already exists: `store.Enqueue` replaces any pending job for
the same preview, and a newer build cancels the one in flight (see [04-concurrency.md](04-concurrency.md)). The
cost is a wasted `npm install`, not a wrong answer.

Two events have no counterpart in either direction. GitHub's `converted_to_draft` is deliberately not a teardown
(`internal/scm/github/webhook.go:151`) — drafts are where documentation gets written — and Bitbucket has no
draft concept to mirror it. Bitbucket's approval events have no GitHub equivalent in this codebase because
docpreview does not care who approved anything.

**Fork pull requests are refused here too**, and this is the one piece of platform-specific logic that must not
be got wrong. GitHub compares `pull_request.head.repo.full_name` against the base repository
(`internal/scm/github/webhook.go:138`); Bitbucket's payload carries `pullrequest.source.repository.full_name`
and `pullrequest.destination.repository.full_name`, and the check is the same comparison against
`pr.Repo.Slug()`. Invariant 5 of 09-scm.md and invariant 1 of 10-security.md apply to every platform, and a new
client that omitted the check would satisfy every existing test.

## ChangedFiles

The interface comment explains why this is a platform call and not a git call (`internal/scm/scm.go:84`): the
answer needs a merge base, a merge base needs history, and the whole appeal of a preview builder is cloning at
depth 1. Both hosts already know.

On Bitbucket Cloud that is the pull request **diffstat** endpoint — a paginated list of changed paths with old
and new names, which is exactly the shape `ChangedFiles` wants and cheaper than the full `/diff`. Renames must
contribute both paths, for the reason GitHub's client already handles at
`internal/scm/github/github.go:181`: a file moved out of `docs/` is a documentation change even though its new
path matches no doc glob.

Pagination differs in a way that matters. GitHub takes `?per_page=&page=` and the client stops when a short page
comes back (`internal/scm/github/github.go:185`). Bitbucket returns an envelope with a `next` field holding an
absolute URL to follow. Following a server-supplied URL blindly is not something this codebase should do: check
that its scheme and host match the configured `api_base` before issuing the request, and keep the same hard page
bound GitHub has (`maxChangedFilePages`, `internal/scm/github/github.go:159`) with the same reasoning — past
thirty pages the answer is "yes, build it".

## CloneURL

`CloneURL` returns a string that is a credential; the interface says so (`internal/scm/scm.go:79`) and the
cloner treats it accordingly, passing it as a git argument and scrubbing it out of every error
(`internal/pipeline/clone.go:64`, `:138`). Bitbucket has no equivalent of a one-hour installation token, so
whatever this returns is as long-lived as the stored credential. There is no short-lived form to reach for; the
mitigation is a narrower credential (a repository access token) rather than a shorter-lived one.

Under `auth: api_token` the URL is `https://<email>:<api-token>@bitbucket.org/<workspace>/<repo>.git`, and
**the email must be percent-encoded**, not merely appended.

This uncovered a live leak in code that predates any Bitbucket work. `scrubLine` found `://` and then the
**first** `@`, and redacted the span between them. An unescaped email contains an `@`, so the scrubber redacted
the local part of the address, resumed after that `@`, found no further `://`, and copied the rest of the line —
including the token — verbatim into the build log and from there into a pull request comment. The function
existed to prevent exactly that.

**Fixed.** `scrubLine` now bounds the authority at the first `/`, `?`, `#` or whitespace and takes the **last**
`@` inside it, which is what RFC 3986 means by userinfo. Two cases were added to
`TestScrubRemovesCloneCredentials` (`internal/pipeline/clone_test.go:8`): a userinfo containing an unescaped `@`,
and two such URLs on one line, since the scrubber has to survive its own first replacement and keep going.

The client should still escape both halves of the userinfo, as the GitHub client escapes its token
(`internal/scm/github/github.go:136`). The scrubber fix means forgetting to is no longer a credential leak, which
is the difference between a defence and a convention.

## The comment

`scm.Marker` and `scm.RenderComment` are platform-neutral by construction and stay that way: `Marker` is a
`fmt.Sprintf` over the preview ID (`internal/scm/scm.go:133`) and `RenderComment` is a pure function of
`Report` (`internal/scm/comment.go:24`). Neither imports a platform. The upsert protocol — list comments, find
the marker, edit or create — needs only a list endpoint, a create, an update, and a delete, and Bitbucket has
all four: comments live under the pull request, the body is a `content.raw` field rather than a bare `body`
string, and an edit is a `PUT` rather than GitHub's `PATCH`.

What is *not* portable is the assumption that a rendered Markdown table and an HTML `<details>` block survive.
The marker is an HTML comment specifically because it is invisible when rendered
(`internal/scm/scm.go:126`); Bitbucket Cloud's comment renderer is not GitHub-flavored Markdown, and raw HTML
may be escaped rather than honored. If it is, two things happen: the marker becomes a visible line of text, and
the failure-log `<details>` block (`internal/scm/comment.go:61`) becomes a literal `<details>` on screen.

Matching still works either way — the marker is matched against the raw body the API returns, not against
rendered HTML — so the *protocol* is safe and invariant 1 holds. The damage is cosmetic and it is on the most
public surface this system has. The fix that preserves invariant 2 is to make rendering a pure function of one
more input rather than to fork the renderer:

```go
// scm/comment.go
type Flavor int

const (
    FlavorGitHub Flavor = iota // HTML comments honored, tables, <details>
    FlavorPlain                // marker on its own line, no raw HTML, fenced log
)

func RenderCommentFlavor(r Report, f Flavor) string
```

`RenderComment(r)` stays as `RenderCommentFlavor(r, FlavorGitHub)` so no existing caller or test moves. The
Bitbucket client passes `FlavorPlain` until somebody confirms what Bitbucket renders — and if it turns out to
render everything GitHub does, the constant is deleted and one call site changes. Guessing in the optimistic
direction costs a broken comment on a stranger's pull request; guessing pessimistically costs a slightly plainer
table.

## There are no check runs

GitHub gets a check run alongside the comment (`internal/scm/github/github.go:281`), keyed by name and head SHA,
updated in place through queued → in progress → conclusion. Bitbucket has **commit build statuses** instead:
keyed by a `key` string on a commit, with a state, a name, a description, and a URL.

Three consequences.

The state vocabulary is narrower. `checkConclusion` maps `StateSkipped` to GitHub's `neutral`
(`internal/scm/github/github.go:348`) — "this ran and deliberately did nothing", which is the honest report for
a pull request that touched no documentation. Bitbucket's states have no neutral. A skip has to be reported as
either a success, which claims a preview exists, or a stopped/failed state, which reads as a problem on the pull
request. Recommend the stopped form with a description that says why, and accept that it looks worse than it is.

A build status wants a URL, and before the preview is ready there is not one. GitHub's check run only sets
`details_url` once the state is ready (`internal/scm/github/github.go:299`); the Bitbucket equivalent needs
something at every state. The dashboard is the natural target, and under the ziti listener or a loopback-only
daemon there is no address a reviewer's browser could open (`localOrigin`, `cmd/docpreview/main.go:276`, returns
`""`). So build statuses are **optional**: skipped entirely when there is no reachable dashboard URL, and
best-effort otherwise — logged and swallowed exactly as the check run is (`internal/scm/github/github.go:205`),
because the comment is the durable artifact and the status is a convenience.

`Retract` deletes the comment and deliberately leaves the check run alone, on the grounds that erasing what
happened to a specific commit is revisionist (`internal/scm/github/github.go:378`). The same rule applies to a
build status, for the same reason.

## Config surface

```yaml
bitbucket:
  enabled: true
  api_base: "https://api.bitbucket.org/2.0"
  auth: "api_token"          # or "access_token"
```

`enabled` rather than a sentinel value, because there is no Bitbucket equivalent of `github.app_id`. GitHub is
"configured" when `cfg.GitHub.AppID != 0` (`cmd/docpreview/main.go:223`) — a number the operator must go and
fetch, which makes a good tripwire. Bitbucket has no such number, so it needs the explicit switch the local
platform already uses (`internal/config/config.go:261`). `validate` (`internal/config/config.go:511`) gains a
check that `auth` is one of the two spellings, in the same style as `exposer.kind`.

Everything else Bitbucket needs comes from the webhook payload. No workspace list, no repository list: a
credential that can reach a repository plus a signed delivery about that repository is the whole authorization
story, the same as GitHub's.

## Bitbucket Data Center is a separate platform, and for now a non-goal

Data Center is not Bitbucket Cloud with a different hostname. Different REST API, different webhook event names
and payload shape, different auth (a personal access token as a bearer, no Atlassian account involved), and no
`api.bitbucket.org`-shaped pagination envelope. The only thing the two share is a name.

So **not** a `kind: cloud | server` field inside one config block. `api_base` works for GitHub Enterprise
(`internal/scm/github/github.go:127`) because Enterprise is the same API at a different address; here it would
be one block in which every key but `api_base` is conditional on a sibling key, and one Go package holding two
unrelated payload parsers behind a switch nobody can read. If Data Center is ever built it gets
`model.PlatformBitbucketServer`, a `bitbucket_server:` config block, `POST /webhook/bitbucket-server`, and its
own package — which is the arrangement `scm.Client` exists to make cheap, and which keeps the choice from
leaking anywhere above the map in `wiring.clients`.

Until somebody asks for it, this is a **deliberate limit** and should be written as one in the user
documentation rather than implied by silence.

## What must move out of `github` first

A second hosted platform copies whatever it finds. These are the things that should not be copied, in the order
they hurt:

1. **`ErrBadSignature`** → `scm.ErrBadSignature`. `internal/scm/github/webhook.go:20` defines it,
   `internal/daemon/ingress.go:189` imports the `github` package purely to compare against it, and
   `internal/scm/local/local.go:145` returns an unrelated error that consequently produces the wrong status
   code. One sentinel in `scm`, `github.ErrBadSignature` kept as an alias for one release if anything external
   references it, and the ingress stops importing a platform package.
2. **`verifySignature`** → `scm.VerifyHMACSHA256`, as above. Two identical copies today, three tomorrow.
3. **The comment upsert loop.** `upsertComment` and `findComment` (`internal/scm/github/github.go:212`, `:251`)
   are pure protocol — render, page through comments, match the marker, create or edit — with four
   platform-specific holes in them. Extract into `scm`:

   ```go
   type CommentStore interface {
       List(ctx context.Context, pr model.PullRequest, page int) (comments []Comment, more bool, err error)
       Create(ctx context.Context, pr model.PullRequest, body string) error
       Update(ctx context.Context, pr model.PullRequest, id string, body string) error
       Delete(ctx context.Context, pr model.PullRequest, id string) error
   }

   func Upsert(ctx context.Context, s CommentStore, r Report) error
   func RetractComment(ctx context.Context, s CommentStore, pr model.PullRequest) error
   ```

   `Comment.ID` is a string, because GitHub's is an `int64` and Bitbucket's should not be assumed to be. The
   paging shape stays behind `List`, which is where the two hosts genuinely differ. This is the single largest
   piece of duplication avoided, and it is the piece where a subtle divergence — matching a prefix instead of
   the marker, taking the newest match instead of the first — would produce duplicate comments on one platform
   only.
4. **`APIError`, `Retryable`, `IsNotFound`** (`internal/scm/github/github.go:444`, `:462`, `:498`). The type and
   the retry predicate are platform-neutral; only the body parser and the rate-limit headers are not. GitHub
   signals a rate limit with 403 plus `X-RateLimit-Remaining: 0` (`internal/scm/github/github.go:485`) and puts
   its message in `{"message": ...}`; Bitbucket returns a different error envelope and different headers. Move
   the struct, keep `errorFromResponse` per platform.
5. **The authenticated-JSON-round-trip helper.** `do` (`internal/scm/github/github.go:398`) is a request
   builder, a bounded error read, and a decode. Worth sharing with an injected `authorize(*http.Request) error`
   and `parseError(*http.Response) error`, because the bounded read at `:469` and the `io.Copy(io.Discard, ...)`
   at `:434` are the sort of care that does not survive being retyped.

Stays platform-specific, and should: the payload structs and event mapping, the authenticator, the `CloneURL`
form, fork detection, and check runs versus build statuses.

Two wiring changes are needed as well, neither of which is a refactor of `scm`:

- **`rewireGitHub`** (`cmd/docpreview/main.go:476`) becomes `rewireSCM`, dispatching on the changed vault key.
  Its existing guarantees have to hold per platform: an unrelated key must not build a client, and a stray
  credential with the platform disabled must not conjure one — the two properties asserted at
  `cmd/docpreview/rewire_test.go:181` and `:198`. The Bitbucket client is built from vault contents at
  construction just as the GitHub one is, so it has the same locked-at-boot problem and the same answer.
- **`validate`** (`cmd/docpreview/main.go:501`) type-switches on `*github.Client` and `*local.Client`. Add
  `interface { Validate(ctx) error }` to `scm` and iterate `w.clients`, so a new platform is validated at
  startup without an edit here. A Bitbucket `Validate` asks the API who the credential belongs to, and its
  error message names the vault keys to fix, in the style of `internal/scm/github/github.go:95`.

And the setup page: three rows in `knownKeys()` (`internal/daemon/secrets.go:209`), `required` keyed off
`cfg.Bitbucket.Enabled` and the selected auth mode, plus a line in `doctor`'s `scm:` output
(`cmd/docpreview/main.go:563`) that reports the mode and whether the client was built.

## Testing

There is no live Bitbucket in the loop and there should not be. The existing GitHub tests show the two halves of
the approach, and one of them does not exist yet.

**Verification and mapping are pure functions and get pure tests.** `internal/scm/github/webhook_test.go:19`
constructs a `Client` literal with nothing but a logger and a secret, signs a payload in the test, and asserts
on the returned events. Every row of the event table above is a case; so is a missing signature, a wrong
signature, a body signed under a different key, a fork pull request producing zero events, and a build event
with no source commit. That file is the template, verbatim.

**The HTTP half needs a fake API server, and the GitHub client has no such test.** That is the gap to close
first, because the pattern will be copied: an `httptest.NewServer` with `api_base` pointed at it, a handler that
records the requests it received, and assertions about the *sequence* rather than the payloads. The tests worth
having are the ones that guard the invariants: publishing three times issues exactly one create and two updates
and never lists the same page twice; `Retract` deletes the comment it created and nothing else; a diffstat that
pages three deep returns every path and both halves of a rename; a `next` URL pointing at another host is
refused; a 429 produces an error whose `Retryable()` is true and a 404 one whose is not.

**Payloads are the one thing that cannot be inferred.** Everything else in this document can be reasoned about
from the code; the exact shape of a `pullrequest:updated` body cannot. Capture one real delivery from a live
workspace — a scratch repository and a webhook pointed at a request bin is enough — redact it, and commit it
under `internal/scm/bitbucket/testdata/`. A hand-written fixture tests the parser against the author's belief
about the payload, which is the failure mode `TODO.md:76` already records for Frontdoor's wire format. Until a
real payload is in `testdata/`, the client is unverified no matter how green the tests are.

## The order to build it in

1. ~~Move the sentinel into `scm`, fix the ingress to compare against the shared one, and make the local client
   return it.~~ **Done** — `scm.ErrBadSignature`, with `github.ErrBadSignature` kept as an alias, and tests
   asserting 401 on all three routes. The HMAC check itself is still duplicated between the github and local
   clients; extracting `VerifyHMACSHA256` is the remaining half and is worth doing when the third caller exists.
2. ~~Fix `scrubLine` to take the last `@` in the authority.~~ **Done**, with tests for a userinfo containing an
   unescaped `@` and for two such URLs on one line. It was a live leak, not a precaution against future code.
3. Add `BitbucketConfig`, its `validate` case, and the three `knownKeys()` rows. Nothing works yet; the setup
   page can now hold the credentials.
4. Capture and commit a real `pullrequest:created` and `pullrequest:updated` payload as testdata.
5. Write `VerifyWebhook` and the event mapping, including fork refusal, against those payloads. Wire the client
   into `setup` and `rewireSCM`. At this point `/webhook/bitbucket` returns 202 and the daemon queues builds
   that fail at clone time — which is the first end-to-end signal worth having.
6. `authorizer` plus `Validate`, then `CloneURL` with both halves of the userinfo escaped. Builds now succeed.
7. Extract `scm.Upsert` and `scm.CommentStore` from the GitHub client, with the fake-server tests for the
   GitHub side written first so the extraction is provably behaviour-preserving. Then implement
   `CommentStore` for Bitbucket, and `Publish`/`Retract` fall out.
8. `RenderCommentFlavor` and `FlavorPlain`, once somebody has looked at a real rendered comment.
9. `ChangedFiles` via diffstat, with the `next`-host check and the page bound.
10. Build statuses, last, and only when the dashboard has a reachable URL. Everything above works without them.
11. Update `docs/design/09-scm.md` (the Bitbucket section currently says "interface only"),
    `docs/design/README.md`, `README.md:187`, `www/docs/quickstart.md:31`, and the `TODO.md` checklist.

## Not verified

Everything above about this repository was read from the code and is cited. Everything below is recalled, not
confirmed, and each item is a thing that fails loudly-but-uninformatively if it is wrong.

- **The webhook signature header name.** I believe Bitbucket Cloud sends `X-Hub-Signature` with a `sha256=` hex
  prefix — the GitHub name *without* the `-256` suffix — but I am not confident, and confusing it with
  `X-Hub-Signature-256` produces a 401 on every delivery with no diagnostic by design. Confirm against
  Atlassian's webhook documentation, or against one captured delivery's headers, before writing the constant.
  Confirm the digest is over the raw body and the prefix spelling at the same time.
- **`X-Event-Key`, `X-Request-UUID`, `X-Hook-UUID`.** Reasonably confident about the first two; the third is
  mentioned nowhere above because I am not sure of it.
- **Event key spellings.** `pullrequest:created`, `pullrequest:updated`, `pullrequest:fulfilled`,
  `pullrequest:rejected`. Confident about the first two, less so that "fulfilled" and "rejected" are the merged
  and declined spellings.
- **That `pullrequest:updated` fires on description edits.** Asserted from how the event is described rather
  than from observation. If it turns out to fire only on source-branch movement, the tradeoff discussed above
  evaporates and the mapping gets simpler.
- **Endpoint paths.** I have deliberately named none. The diffstat, comment, and build-status endpoints are
  described by what they do, not by a path I would be guessing at. Fill them in from the API reference when the
  client is written; every one of them is a 404 away from being caught, unlike the header name.
- **Payload field paths.** `pullrequest.id`, `pullrequest.source.branch.name`, `pullrequest.source.commit.hash`,
  `pullrequest.source.repository.full_name`, `pullrequest.destination.branch.name`, and the diffstat entries'
  `old.path` / `new.path` are recalled and plausible, not confirmed. This is what step 4 above is for.
- **Comment bodies use `content.raw` and edits use `PUT`.** Reasonably confident; `TODO.md:60` says `PUT`
  independently, which is weak corroboration at best since it may share an author with this document.
- **Build status states** `INPROGRESS`, `SUCCESSFUL`, `FAILED`, `STOPPED`, and that there is no neutral. The
  absence of a neutral state is the load-bearing claim; if one exists, `StateSkipped` maps to it instead.
- **Markdown flavor.** Whether Bitbucket Cloud comments render pipe tables, and whether raw HTML — the marker
  and `<details>` — is honored or escaped. I do not know, which is why `FlavorPlain` is the recommended default
  rather than an assumption either way. One test comment on a scratch pull request settles it.
- **App password end-of-life.** That app passwords are being removed in favour of Atlassian API tokens is
  something I am confident about in direction; `TODO.md:61` and `docs/design/09-scm.md:117` both name June 2026,
  which is the repository's own claim and not independent confirmation.
- **Access tokens.** That Bitbucket Cloud offers repository, project, and workspace access tokens as scoped,
  non-user bearer credentials, and that a repository-scoped one is usable for both the API and an HTTPS clone
  (with a fixed literal username in the userinfo, `x-token-auth` or similar). The whole `auth: access_token`
  recommendation rests on this; verify it before writing the second auth mode, and if it does not hold, the
  security section's conclusion — that Bitbucket's credential is a person's — becomes unconditional.
- **Rate-limit signalling.** That Bitbucket Cloud returns 429 and may include `Retry-After`. The
  `APIError.Retryable` contract does not depend on the details, but the parser does.
- **Data Center specifics.** The `/rest/api/1.0` base, `pr:opened` / `pr:merged` / `pr:declined` /
  `pr:from_ref_updated` event names, and bearer personal access tokens. Recalled. Since Data Center is
  recommended as a non-goal, nothing is built on these; they are here only to support the claim that it is a
  different API rather than a different address.
