# Bitbucket

Bitbucket is the reason `scm.Client` exists. It is also the platform for which nothing has been written. This
document is the plan, and the first half of it is an inventory: how much of the anticipation in the tree is
actually load-bearing, and how much is a comment promising work.

Read [09-scm.md](09-scm.md) first. Everything here is a second implementation of what that document describes.

The question this document exists to answer is "how do I do the GitHub App thing, but on Bitbucket?" — and the
short answer is that there is no single Bitbucket construct that does what a GitHub App does, because a GitHub App
is three things fused together and Bitbucket sells them separately. The next section takes that apart. Everything
after it is the consequence.

## What a GitHub App actually is, and what Bitbucket has instead

A GitHub App bundles three things docpreview depends on:

1. **A non-human identity.** Comments and check runs are attributed to `docpreview[bot]`, not to whoever's
   credential is in the vault.
2. **A per-installation grant.** An installation token reaches exactly the repositories the App was installed on,
   with exactly the permissions the App declared, and it expires in an hour
   (`internal/scm/github/auth.go:43`).
3. **A webhook subscription attached to that grant.** One webhook URL and one secret, configured once on the App,
   covering every repository the App is later installed on.

Bitbucket Cloud has five candidate constructs and not one of them provides all three.

| Construct | Identity | Scope | Credential lifetime | Webhook story | Verdict |
|---|---|---|---|---|---|
| **Forge app** | app (bot) | per-installation grant, declared scopes | platform-managed | app-level, per installation | The structural analogue. Wrong shape for a self-hosted daemon — see below. |
| **Atlassian Connect app** | app (bot) | per-installation grant | platform-managed | app-level | **Closed.** New Connect apps cannot be registered or installed since 2 Feb 2026; end of support Dec 2026. |
| **OAuth 2.0 consumer** | the authorizing *user* | that user's whole visibility | access token 2h, refresh token 3 months | none — you still add a webhook per repository | Only construct with GitHub-like token expiry. Identity is a person. |
| **Access token** (repository / project / workspace) | a bot, not a user | exactly one repo, project, or workspace, with a scope list | long-lived; expiry is optional and set at creation | none — per-repository webhook | **Recommended.** Two of the three properties. |
| **App password** | the owning user | that user's whole visibility | never expires | none | **Dead.** Final removal 28 July 2026, after a seven-week brownout ramp. |
| **Atlassian API token** | the owning user | that user's whole Atlassian identity | optional expiry, up to 1 year | none | The app-password replacement. Fallback only. |

### Why not Forge

Forge is where Atlassian points every new app, and if docpreview were a marketplace product it would be a Forge
app. It is not the right vehicle here, for a reason that has nothing to do with Atlassian's roadmap: **a Forge
app's code runs on Atlassian's infrastructure.** docpreview's entire job is to clone a branch and run
`npm install` on hardware the operator controls, behind whatever network boundary the operator chose — which is
why the exposer abstraction exists at all. A Forge app would therefore be a thin control plane that calls out to
the operator's daemon (Forge remotes), and every credential problem would still be there on the daemon side, plus
an app manifest, a deployment pipeline, a distribution decision, and an Atlassian developer account.

That is a real option for a future in which docpreview is installed by people who did not build it. It is the
wrong first implementation, and choosing it would mean the operator cannot get a preview working without becoming
an Atlassian app developer. If this decision is reversed later, what changes is the authenticator and the runbook;
the event mapping, the payload parsing, and the comment protocol below are all still needed, because a Forge
remote receives the same webhook payloads.

### Recommendation: a repository (or workspace) access token, plus a per-repository webhook

This buys the non-human identity and the narrow scope, and gives up the short lifetime. Git operations by an
access token show up under a synthetic bot address of the form `…@bots.bitbucket.org` rather than as a person,
which is the property that keeps a comment from looking like a maintainer wrote it.

What it costs an operator, in full — this is the shape of the Bitbucket sibling to
[the GitHub App runbook](../../www/docs/runbooks/github-app.md):

| Step | Where | Produces |
|---|---|---|
| Create the token | Repository settings → Security → Access tokens → Create | one bearer token, shown once |
| Choose scopes | same dialog | `repository` (read, to clone) and `pullrequest:write` (to comment) |
| Add the webhook | Repository settings → Webhooks → Add webhook | URL + secret, triggers = pull request events |
| Store both | `docpreview vault set bitbucket.access_token`, `… bitbucket.webhook_secret` | vault entries |

No app registration, no manifest, no review, no Atlassian developer account, and nothing global to the workspace.
Compared with the GitHub runbook it is *shorter* — there is no App to create and no private key to download and
delete. What it loses is the "create once, install many" property: the click-through above is per repository, so
ten repositories is ten tokens and ten webhooks where a GitHub App is created once and installed ten times.

Both halves have a workspace-level collapse, and one of them is not discoverable from the UI:

- A **workspace access token** is one credential for every repository in the workspace, at the cost of a wider
  blast radius if the vault leaks.
- A **workspace webhook** fires for events from every repository in the workspace, and can carry a secret. It
  exists only through the API — `POST /2.0/workspaces/{workspace}/hooks` — with no page in the web UI, which is
  why most integration guides do not mention it.

That second one is worth building a command around: `docpreview bitbucket install-hook -workspace acme` doing the
one API call the UI will not do, generating the secret, storing it in the vault, and printing what it created. The
GitHub runbook cannot automate its equivalent because creating a GitHub App genuinely requires a browser; here the
browser is only a limitation of Atlassian's UI, and taking ten webhook forms down to one command is the single
biggest usability difference available on this platform.

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

## Authentication: how a token is obtained, and what refreshes

The reserved key names — an email and an API token — encode a decision that was correct when it was made and is now
the fallback rather than the recommendation. The dates matter, because one of them has already passed:

| Credential | Obtained by | Refresh | Status (July 2026) |
|---|---|---|---|
| App password | account settings | never expires | **Removed 28 July 2026**, after brownouts from 9 June. Not an option. |
| Atlassian API token | Account settings → Security → API tokens | none; expiry chosen at creation, up to 1 year | The app-password replacement. `Basic <base64(email:token)>`. |
| Repository / project / workspace access token | the resource's own settings → Access tokens | none; optional expiry at creation | **Recommended.** `Bearer <token>`. |
| OAuth 2.0 consumer | 3LO browser dance, or `client_credentials` | access token 2h, refresh token 3 months of disuse | Only expiring option. See below for why it is not the answer. |

The security difference from GitHub is not a detail of header construction. A GitHub App private key mints a
ten-minute JWT which mints a **one-hour installation token scoped to the repositories that installation covers**
(`internal/scm/github/auth.go:31`, `:103`). Three properties fall out of that, and docpreview relies on all three:
a token that leaks into a log expires by itself; a token cannot reach a repository the app was not installed on;
and the comment is attributed to the app rather than to a person.

An Atlassian API token has none of them. It carries **everything the human it belongs to can see** — every
workspace, every repository, and Jira and Confluence too unless the token was created with Bitbucket-only scopes —
and every comment docpreview writes is written by that human. If the vault leaks, the blast radius is a person's
whole Atlassian identity rather than one app installation's repository list.

A **repository access token** recovers two of the three. It is scoped to one repository with an explicit scope
list, it is revocable on its own, and it is not a person: git operations by an access token are attributed to a
synthetic `…@bots.bitbucket.org` address. What it does not recover is short lifetime, and there is no way to
recover that with a stored credential — which is the whole reason the redactor note below matters.

That "not a person" claim is worth being precise about, because it has an observed counterexample. **Vercel's own
Bitbucket integration cannot manage it.** Every comment it posted on `customer-connect-docs` comes back from the
API with `user.type: "user"`, `user.display_name: "Clint Dovholuk"`, and the account ID of the human who connected
Vercel to the workspace. So the pull request shows a maintainer cheerfully announcing his own deploy failures. If
Vercel — with a Connect app, a marketplace listing and an Atlassian partnership — writes comments as a person, then
"comments attributed to a bot" is not something docpreview is going to achieve on Bitbucket either. The best
available answer is the one the runbook should state plainly: **create a dedicated Atlassian account for the bot,
or use an access token and accept that comments are attributed to whoever's account created it.** Do not let an
operator discover this from a screenshot.

### Why not OAuth 2.0, despite being the only expiring credential

A two-hour access token with a refresh token is structurally the closest thing to GitHub's installation token, and
the reason to refuse it is architectural rather than about tokens.

Bitbucket's refresh tokens can rotate: the refresh response may contain a new refresh token, and the old one stops
working. That makes the credential *mutable state owned by the request path* — and this daemon's credential store is
an age-encrypted file that can be **locked**, that a human unlocks through the dashboard, and that the daemon is
specifically designed not to read during wiring (`cmd/docpreview/CLAUDE.md`). A client that must write a new
refresh token into the vault from inside `Publish` introduces: a vault write on a hot path, two concurrent
`Publish` calls racing to store two different refresh tokens, a restart window in which the token in the vault is
the one that was just invalidated, and an authentication failure mode whose fix is "clear the vault key and do the
browser dance again" — which requires a human, which is precisely what `vault.key_source` exists to avoid.

That is a real feature for someone who needs a genuinely short-lived Bitbucket credential, and it should be built as
`auth: oauth` when someone does. It is not the first implementation, and if this decision is reversed the thing to
design first is not the token exchange but *where the rotated refresh token is written and what happens when two
goroutines write it at once.*

### Config and vault surface

```go
// BitbucketConfig, internal/config/config.go
type BitbucketConfig struct {
    Enabled bool   `yaml:"enabled"`
    APIBase string `yaml:"api_base"`

    // Auth is "access_token" (a repository, project or workspace access token,
    // recommended) or "api_token" (an Atlassian account email plus API token).
    Auth string `yaml:"auth"`
}
```

Two auth modes, one `authorizer` type inside the client that turns a mode plus the vault into an `Authorization`
header, chosen once at construction. `access_token` reads a fourth vault key (`bitbucket.access_token`) and sends
`Bearer`; `api_token` reads the two reserved keys and sends `Basic`. **Default `access_token`**, reversing the
earlier plan in this document: the reserved key names implied `api_token`, but an operator following the reserved
names today would be storing the credential with the wider blast radius by default, and a default should be the
recommendation. `doctor` reports which mode is active either way.

Do **not** infer the mode from which keys happen to be present. A vault mid-setup contains a subset of everything,
and a client that silently picks the wider credential because the narrower one is not stored yet is the kind of
magic that is discovered during an incident.

What the daemon stores, in full:

| Vault key | Mode | Notes |
|---|---|---|
| `bitbucket.access_token` | `access_token` | bearer; `x-token-auth` is the literal username for clone |
| `bitbucket.email` | `api_token` | not itself secret, but half an `Authorization` header and half a clone URL |
| `bitbucket.api_token` | `api_token` | Basic password half |
| `bitbucket.webhook_secret` | both | HMAC key; hard-required, see above |

Nothing is cached, because nothing expires on a schedule — so there is no `authenticator` with a mutex and a token
map, and no `invalidate` on 401. That is genuinely simpler than the GitHub client, and the simplicity is bought with
a credential that never expires on its own. One consequence: **register the token with the redactor**
(`internal/redact`) the way `build.secrets` values are. An expiring installation token in a log is an
embarrassment; a non-expiring one is a credential.

One API-base constraint to encode rather than discover: as of 4 May 2026 Bitbucket requires authenticated REST
calls to go to `https://api.bitbucket.org` with the token as a bearer per RFC 6750, not to `bitbucket.org/api`.
`validate` should reject an `api_base` pointing at `bitbucket.org`, with an error naming the right value — that is a
403 with an unhelpful body otherwise.

The token is long-lived, which makes one more thing worth doing that GitHub did not need: register it with the
redactor (`internal/redact`) the way `build.secrets` values are. An expiring installation token in a log is an
embarrassment; a non-expiring one is a credential.

## Webhook verification

**Bitbucket Cloud has a real HMAC-SHA256 signature and this is confirmed, not recalled.** A webhook with a secret
token set is delivered with an `X-Hub-Signature` header whose value is `method=signature` per WebSub — in practice
`sha256=<hex>` — computed over the raw request body with the secret as the key. Atlassian's own documentation says
to compare it in constant time and warns that reformatting the body before hashing changes the digest, which is
the same pair of rules `internal/scm/github/webhook.go:33` already states. Sources:
[Manage webhooks](https://support.atlassian.com/bitbucket-cloud/docs/manage-webhooks/),
[Verify webhook signature (sample code)](https://support.atlassian.com/bitbucket-cloud/kb/bitbucket-cloud-python-sample-code-to-verify-webhook-signature/).

The header is `X-Hub-Signature`, **not** `X-Hub-Signature-256` — GitHub's name without the suffix. GitHub sends
both spellings (`-1` for the legacy SHA-1 digest, `-256` for the current one) and Bitbucket sends one, with the
algorithm named inside the value instead of in the header. So the digest is selected by parsing `method=` rather
than by the constant the GitHub client can hard-code, and Atlassian reserves the right to change it: *"Right now,
Bitbucket will send HMACs using sha256"*. Parse the method, accept only `sha256`, and reject anything else rather
than treating an unknown method as absent — an accepted-but-unverified delivery is a build trigger.

Getting the header name wrong is a 401 on every delivery with no diagnostic, by design
(`internal/daemon/ingress.go` answers a bare 401 and says nothing). That is why this is the one fact in this
document worth two sources.

The rest of the header set, all confirmed:

| Bitbucket | GitHub equivalent | Use |
|---|---|---|
| `X-Event-Key` | `X-GitHub-Event` | `pullrequest:created`, etc. — event *and* action in one string |
| `X-Request-UUID` | `X-GitHub-Delivery` | delivery ID → `scm.Event.Delivery` |
| `X-Hook-UUID` | — | identifies the *webhook subscription*, not the delivery |
| `X-Attempt-Number` | — | 1 on first delivery; higher means Bitbucket is retrying |
| `X-Hub-Signature` | `X-Hub-Signature-256` | HMAC, see above |

`X-Attempt-Number` has no GitHub counterpart and is worth logging rather than acting on. It tells an operator
reading the daemon log that Bitbucket thinks earlier attempts failed — which, given that a successful build already
answers 202 quickly, means either the tunnel dropped or something is slow. It must **not** be used to deduplicate:
`store.Enqueue` already collapses repeat work for the same preview (see
[04-concurrency.md](04-concurrency.md)), and a client that discarded attempt 2 would discard the retry of a
delivery that genuinely never landed.

### If the signature had not existed

It does, so this is recorded to close the question rather than as a live option. Had Bitbucket offered no HMAC, the
three alternatives and what each actually buys:

| Alternative | What it defends against | What it does not |
|---|---|---|
| **Secret in the URL path** (`/webhook/bitbucket/<32 random bytes>`) | someone who guesses the hostname | anything that sees the URL: TLS-terminating proxies, tunnel dashboards, browser history, the webhook config page itself, and every access log on the path. It is a bearer token in the one field of an HTTPS request that everything logs. Also unrotatable without editing the hook. |
| **IP allowlisting** (Atlassian publishes ranges at `ip-ranges.atlassian.com`) | random internet scanners | *anything else running on Atlassian's cloud*, which is a large multi-tenant estate — the allowlist authenticates a network, not a sender. Also breaks silently and asymmetrically when the published ranges change, and cannot work at all behind zrok or Frontdoor, where every request arrives from the tunnel's own address (see [10-security.md](10-security.md) on loopback not meaning local). |
| **mTLS** | everything, cryptographically | being unavailable: the sender must support client certificates, and a hosted SaaS webhook does not let you install one. Viable only for the tunnel hop, where it authenticates the tunnel rather than Bitbucket. |

The ranking is not close, and the reason is worth stating because it also explains why the fork refusal below is
non-negotiable: this endpoint's authentication is the *only* thing standing between an internet stranger and
`npm install` on the operator's machine. A path secret or an IP range would be a downgrade of exactly that
boundary. If Bitbucket ever removes the HMAC, the correct answer is to stop supporting Bitbucket, not to fall back.

The comparison logic itself is identical, which is exactly the problem: `verifySignature` already exists twice,
at `internal/scm/github/webhook.go:62` and `internal/scm/local/local.go:205`, byte for byte. Bitbucket would be
the third copy of a constant-time HMAC check, and the third place someone could accidentally write `==`. It
moves to `scm` before this client is written, not after.

```go
// scm/webhook.go
var ErrBadSignature = errors.New("webhook signature verification failed")

// VerifyHMACSHA256 checks header, of the form "sha256=<hex>", against the
// HMAC-SHA256 of body under secret. A method other than sha256 is a
// verification failure, not an unknown to be waved through.
func VerifyHMACSHA256(secret, body []byte, header string) bool
```

Both platforms spell the value `sha256=<hex>`, so the earlier plan to make the prefix a parameter was solving a
problem that does not exist; the two disagree about the *header name*, which is the caller's business. One function,
no configuration. GitHub also sends the legacy `X-Hub-Signature` with a SHA-1 digest, which is exactly why the
github client must keep reading `X-Hub-Signature-256` specifically and the bitbucket client must keep reading
`X-Hub-Signature` — the same header name means different things on the two hosts, and a shared "find the signature
header" helper would be the bug.

The order of operations in `VerifyWebhook` is not negotiable and the reasons are already written down at
`internal/scm/github/webhook.go:22`: verify before parsing, because the endpoint is internet-facing by design
and the JSON parser must never see unauthenticated bytes. The body cap is upstream in the ingress
(`internal/daemon/ingress.go:31`) and platform-independent, so Bitbucket inherits it for free.

**Missing secret is a hard failure, not a downgrade.** This is not hypothetical: the API documentation is explicit
that an empty or absent secret leaves the webhook unsigned, and that Bitbucket generates `X-Hub-Signature` *only*
when the secret is set. So the failure mode is a webhook that works perfectly, delivers real payloads, and carries
no authentication whatsoever — and a client that accepted unsigned deliveries when `bitbucket.webhook_secret` is
absent would turn one blank field in a web form into an unauthenticated build trigger. Absent header is
`ErrBadSignature` with a message naming the field to fill in, the same way the GitHub client does at
`internal/scm/github/webhook.go:38`. The local client is allowed to skip the signature
(`internal/scm/local/local.go:136`) because it is a loopback development loop; a hosted platform is not.

The corollary is a check `doctor` should make and GitHub's equivalent cannot: with an access token in hand, the
daemon can *read the webhook back* (`GET /2.0/repositories/{workspace}/{repo}/hooks`) and report a hook that has no
secret. An operator who has blanked the secret currently learns about it from a wall of 401s. This costs one more
token scope, `read:webhook:bitbucket` — verified by asking for that endpoint without it, see the error-envelope
section — so `doctor` must degrade to "cannot check, token lacks read:webhook:bitbucket" rather than failing.

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

**Bitbucket does have drafts, contrary to what this document previously said.** A real pull request object read
back from the API carries `"draft": false` as a first-class field, alongside `"state": "OPEN"`. So the GitHub rule
at `internal/scm/github/webhook.go:154` — a draft is still worth previewing, because drafts are where documentation
gets written — transfers directly: **do not filter on `pullrequest.draft`.** Which is the same as ignoring the
field, but ignoring it by decision rather than by not knowing it exists. Bitbucket's approval events have no GitHub
equivalent in this codebase because docpreview does not care who approved anything.

### The payload fields `model.PullRequest` needs

Field paths below were read off a live pull request object
(`GET /2.0/repositories/netfoundry/customer-connect-docs/pullrequests/20`), not from documentation. The webhook body
nests the same object under `pullrequest`, plus a top-level `repository` — which is the one part still worth
confirming against a captured delivery.

| `model.PullRequest` | Bitbucket path | GitHub path | Note |
|---|---|---|---|
| `Repo.Owner` | `repository.workspace.slug` | `repository.owner.login` | `full_name` split on `/` also works and needs no extra field |
| `Repo.Name` | `repository.slug` | `repository.name` | `slug` is URL-safe; `name` is the display name and can contain spaces |
| `Repo.CloneURL` | `repository.links.clone[]` (`{name: "https"\|"ssh", href}`) | `repository.clone_url` | an array to search by `name`, not a scalar |
| `Number` | `pullrequest.id` | `pull_request.number` | |
| `Branch` | `pullrequest.source.branch.name` | `pull_request.head.ref` | |
| `HeadSHA` | `pullrequest.source.commit.hash` | `pull_request.head.sha` | **abbreviated — see below** |
| `BaseBranch` | `pullrequest.destination.branch.name` | `pull_request.base.ref` | |
| fork? | `pullrequest.source.repository.full_name` vs `destination` | `head.repo.full_name` vs base | |
| `InstallationID` | — | `installation.id` | unused on Bitbucket, already documented as such |

### `source.commit.hash` is twelve characters, not forty

This is the finding most likely to cost a day if it is discovered during implementation instead of now. The live
pull request object returns:

```json
"source": { "commit": { "hash": "a4fd6c9db194" } }
```

Twelve hex characters. The same commit's full hash, visible in the `src` links inside the *diffstat* response for
the same pull request, is `a4fd6c9db1940992c8af5c48401462100bd7d2f1`. `GET /pullrequests/{id}/commits` also returns
the full forty.

So Bitbucket's pull request serialization abbreviates commit hashes and GitHub's does not. What that touches:

- **`git checkout` is fine.** Git resolves an unambiguous abbreviation, so the clone and build work and nothing
  visibly fails — which is exactly why this is dangerous.
- **Comparisons are not fine.** Any code that asks "is the SHA I built the SHA I was told about" against a hash
  from a different source compares 12 characters with 40 and finds them unequal. The supersede logic and anything
  keyed on `HeadSHA` need to agree about width.
- **The build status key is not fine.** A commit status posted against `a4fd6c9db194` and one posted against the
  full hash are, as far as an operator reading the pull request is concerned, two statuses on one commit.
- **The comment says the wrong thing.** `Report.Commit` is rendered, so a reviewer sees a hash that does not match
  what `git log` shows them locally.

**Decision: normalize to the full SHA in the client, at parse time, and treat a 12-character `HeadSHA` as a bug.**
Two ways to get it, and the cheap one is better: the clone already happens, so `git rev-parse HEAD` after checkout is
free and authoritative — but it is too late, because the preview is enqueued before the clone. So the client resolves
it with one request (`GET /2.0/repositories/{ws}/{slug}/commit/{abbrev}`, whose `hash` is full) during
`VerifyWebhook`, and a failure to resolve is a hard error rather than a fallback to the abbreviation. An extra API
call inside webhook verification is a real cost — it is on the path that must answer 202 quickly — and it is still
the right trade, because the alternative is a preview identity that silently disagrees with itself.

If the captured webhook payload turns out to carry a full hash where the REST object carries an abbreviation, this
whole subsection collapses to one line of validation. Check it first; it is the single highest-value item in the
testdata step.

**Fork pull requests are refused here too**, and this is the one piece of platform-specific logic that must not be
got wrong. GitHub compares `pull_request.head.repo.full_name` against the base repository
(`internal/scm/github/webhook.go:138`); Bitbucket carries `pullrequest.source.repository.full_name` and
`pullrequest.destination.repository.full_name` — both confirmed present and both equal on a same-repo pull request —
and the check is the same comparison against `pr.Repo.Slug()`. Invariant 5 of 09-scm.md and invariant 1 of
10-security.md apply to every platform, and a new client that omitted the check would satisfy every existing test.

Two Bitbucket-specific notes on the refusal:

- The repository object also carries a `parent` field, populated only when the repository is itself a fork. That is
  the answer to "is this repository a fork", not "is this pull request from a fork", and confusing the two would
  refuse every pull request on a forked repository while still building cross-repository ones. Compare the two
  `full_name`s; ignore `parent`.
- GitHub's known gap — a null `head.repo` from a deleted fork skips the refusal
  (`docs/design/12-github-roadmap.md`) — should not be reproduced. Bitbucket's `source.repository` can likewise be
  absent, and the Bitbucket client should treat **absent as untrusted** and refuse. Getting the safer default on the
  new platform costs nothing and makes the GitHub fix a matter of copying.

## ChangedFiles

The interface comment explains why this is a platform call and not a git call (`internal/scm/scm.go:84`): the
answer needs a merge base, a merge base needs history, and the whole appeal of a preview builder is cloning at
depth 1. Both hosts already know.

On Bitbucket Cloud that is the pull request **diffstat** endpoint,
`GET /2.0/repositories/{ws}/{slug}/pullrequests/{id}/diffstat` — cheaper than the full `/diff` and exactly the shape
`ChangedFiles` wants. A real response, read back from the live repository:

```json
{ "pagelen": 500, "size": 2, "page": 1,
  "values": [ { "type": "diffstat", "status": "modified",
                "lines_added": 23, "lines_removed": 5,
                "old": { "path": "docusaurus/docs/…/role-grants.md", "escaped_path": "…" },
                "new": { "path": "docusaurus/docs/…/role-grants.md", "escaped_path": "…" } } ] }
```

Four things fall out of that, and only the first was in the earlier plan.

**Renames must contribute both paths**, for the reason GitHub's client already handles at
`internal/scm/github/github.go:181`: a file moved out of `docs/` is a documentation change even though its new path
matches no doc glob. Bitbucket makes this easy — `old.path` and `new.path` are separate fields rather than GitHub's
`previous_filename` — and also easy to get wrong, because on `status: "added"` there is no `old` and on
`status: "removed"` there is no `new`. Both are nil-able; a naive `entry.Old.Path` is a panic on the first pull
request that adds a file.

**`pagelen` defaults to 500, not 10.** So the pagination loop almost never runs a second iteration, which means the
pagination code is almost never exercised, which means it will be wrong when it finally matters. Test it against a
fake server that returns two short pages; do not trust it because real pull requests pass.

**`next` is absent, not empty, on the last page.** The envelope here has `values`, `pagelen`, `size`, `page` and no
`next` at all. A loop conditioned on `next != ""` works; one conditioned on the field's presence in a map does not
generalize. There is also a `size` giving the total, so the loop has a cross-check available: if the paths
collected do not number `size`, something was dropped silently.

Following a server-supplied URL blindly is not something this codebase should do: check that `next`'s scheme and host
match the configured `api_base` before issuing the request, and keep the same hard page bound GitHub has
(`maxChangedFilePages`, `internal/scm/github/github.go:159`) with the same reasoning — past thirty pages the answer
is "yes, build it".

One incidental gift: the `old.links.self` and `new.links.self` hrefs inside a diffstat entry contain the **full
forty-character** commit hashes for the base and head. That is where the 12-versus-40 discrepancy above was
discovered, and it is a second way to resolve the full head SHA without an extra request — though a `src` link is a
worse thing to parse than a commit endpoint is to call, so it is a corroboration, not the plan.

## CloneURL

`CloneURL` returns a string that is a credential; the interface says so (`internal/scm/scm.go:79`) and the
cloner treats it accordingly, passing it as a git argument and scrubbing it out of every error
(`internal/pipeline/clone.go:64`, `:138`). Bitbucket has no equivalent of a one-hour installation token, so
whatever this returns is as long-lived as the stored credential. There is no short-lived form to reach for; the
mitigation is a narrower credential (a repository access token) rather than a shorter-lived one.

Under `auth: access_token` the URL is `https://x-token-auth:<token>@bitbucket.org/<workspace>/<slug>.git`. The
username is the **literal string `x-token-auth`** — not the token, as on GitHub, and not the workspace. Under
`auth: api_token` it is `https://<email>:<api-token>@bitbucket.org/<workspace>/<slug>.git`, and **the email must be
percent-encoded**, not merely appended.

**Construct this URL; do not decorate the one Bitbucket hands you.** The repository object's HTTPS clone link comes
back already carrying a username:

```json
"links": { "clone": [ { "name": "https",
  "href": "https://dovholuknf@bitbucket.org/netfoundry/customer-connect-docs.git" } ] }
```

Whose username that is depends on who asked. A client that inserted credentials into that string would produce
`https://x-token-auth:TOKEN@dovholuknf@bitbucket.org/…` — two `@` in the authority, git failing with a message
containing the token, and the scrubber's userinfo-boundary logic doing the work it was only just fixed to do. Parse
the workspace and slug, build the URL from `api_base`'s sibling host, and use `links.clone` only to *check* that
the repository is where you think it is. Note also that the array must be searched by `name == "https"` rather than
indexed: the `ssh` entry is also there, and its order is not a promise.

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

## The comment, and the marker that must change

This section is no longer speculation. Vercel's own Bitbucket integration is live on
`bitbucket.org/netfoundry/customer-connect-docs`, doing the same job docpreview does, and its comments were read
back through the API (`GET /2.0/repositories/{ws}/{repo}/pullrequests/{id}/comments`). Bitbucket returns both the
source and the rendering — `content.raw`, `content.markup: "markdown"`, `content.html` — so the renderer's
behaviour can be observed rather than guessed. Two findings, and the first one breaks `scm.Marker`.

### `<!-- -->` is escaped. The current marker would be a visible line of text.

Bitbucket Cloud's comment renderer escapes raw HTML. On `customer-connect-docs` PR 17, Vercel's own comment ends
with `<!-- vercel-commit-author-required -->` and Bitbucket rendered it as:

```html
<p>&lt;!-- vercel-commit-author-required --&gt;</p>
```

That is a paragraph of literal text on a public pull request, put there by a company whose whole business is this
feature. `scm.Marker` (`internal/scm/scm.go:161`) produces exactly the same construction, so shipping it unchanged
means every docpreview comment on Bitbucket ends with a visible `<!-- docpreview:a1b2c3 -->`. The upsert protocol
would still work — the marker is matched against `content.raw`, not against the rendering — so this is cosmetic,
but it is cosmetic on the most public surface this system has, and it is the kind of thing an operator screenshots.

### The technique that does work: a Markdown link reference definition

The *other* Vercel comment on the same pull request begins with:

```text
[vc]: #BCny46AmJOpyQ8M97Lhe+CT5i8xIQNMmijZxYpja8io=:eyJpc01vbm9yZXBvIjp0cnVlLCJ0eXBlIjoiYml0YnVja2V0Iiwi…
The latest updates on your projects. …
```

and the rendered `content.html` for that comment starts at `<p>The latest updates…`. **The first line is gone
entirely.** It is a CommonMark *link reference definition* — `[label]: destination` — which every conforming
renderer consumes as a definition and emits no output for, whether or not raw HTML is allowed. It needs no HTML
support, survives escaping because there is nothing to escape, and is invisible on GitHub for the same reason it is
invisible on Bitbucket. Note also that no blank line separates it from the following paragraph and it still
disappeared, so it does not need to be its own block.

So `Marker` becomes platform-dependent, and the honest shape is a marker *and* a matcher, both in `scm`:

```go
// MarkerStyle selects how the self-identifying marker is embedded in a comment
// body. The protocol is the same on every platform — list, find the marker, edit
// or create — but the syntax that renders to nothing is not.
type MarkerStyle int

const (
    // MarkerHTMLComment is "<!-- docpreview:<id> -->". Invisible on GitHub.
    MarkerHTMLComment MarkerStyle = iota
    // MarkerLinkRef is "[docpreview]: #<id>", a CommonMark link reference
    // definition. Invisible everywhere, because it renders to nothing rather
    // than relying on raw HTML being honored. Bitbucket Cloud escapes raw HTML,
    // which turns MarkerHTMLComment into a visible line of text; observed on a
    // real Vercel comment, see docs/design/15-bitbucket.md.
    MarkerLinkRef
)

func MarkerFor(previewID string, s MarkerStyle) string
func HasMarker(body, previewID string) bool // matches either style
```

`HasMarker` matching **both** styles is the load-bearing part, and it is why this is not simply "change the
constant". A daemon upgraded across this change will find comments it wrote in the old style, and a matcher that
only knew the new one would create a second comment on every open pull request at once — the exact failure
`scm.Marker`'s doc comment says the marker exists to prevent (`internal/scm/scm.go:151`). `Marker(previewID)` stays
as `MarkerFor(previewID, MarkerHTMLComment)` so no GitHub caller or test moves.

If this decision is reversed — if `MarkerLinkRef` is adopted for GitHub too, which is defensible since it is
invisible there as well — what breaks is nothing, provided `HasMarker` keeps recognising both forever. Deleting the
old branch of `HasMarker` is the change that cannot be made safely, and the comment on it should say so.

### Vercel also puts state in the marker, and that is worth copying later, not now

The base64 after Vercel's `#` decodes to JSON: `{"isMonorepo":true,"type":"bitbucket","bitbucketBranchUrl":…,
"projects":[{"name":"customer-connect-docs","projectId":"prj_…","rootDirectory":"docusaurus","inspectorUrl":…,
"previewUrl":"","nextCommitStatus":"FAILED"}]}` — preceded by what is almost certainly an HMAC of it, given the
shape (`#<44-char base64 with trailing =>:<base64 json>`). They are using the comment body as a signed,
tamper-evident state store, which is a genuinely good idea: it removes any need for a database row keyed to a
comment, and the signature stops a pull request author from editing the comment to change what the integration
believes.

docpreview does not need it. Its state is in sqlite (`internal/store/`) and `PreviewID` is derived, not stored
(`internal/model/model.go:75`), so there is nothing to carry. Recording it because it is the answer if a future
feature ever wants per-comment state, and because it explains why Vercel's marker looks like line noise.

### The protocol itself carries over

Comments live under the pull request, the body is `content.raw` rather than a bare `body` string, and the list
response is the standard `{values, page, pagelen, next}` envelope. Confirmed from the response object: `id` is a
JSON number (`831492365`), and there are `deleted` and `pending` booleans with no GitHub analogue.

Two consequences for `CommentStore`:

- **`Comment.ID` stays a string in the interface** even though both platforms happen to use integers. The interface
  should not care, and a string cannot be silently truncated by a JSON decode into the wrong-width integer.
- **`deleted: true` comments are still returned by the list endpoint.** A soft-deleted comment retains its body, so
  a marker match against one would produce an update to a comment nobody can see — and the preview would appear to
  publish successfully while showing nothing. `List` must drop `deleted` entries, and that is a "must not" with a
  test: *a deleted comment carrying our marker must not be matched.*

`pending: true` is a comment in an unpublished review draft. docpreview never creates one; skipping those on read
costs nothing and avoids one more way to match something invisible.

### Tables render, and `Flavor` is not needed after all

`content.html` for Vercel's status comment contains a real `<table>`, `<thead>`, `<td>`, and even an `<img>` from
Markdown image syntax. So pipe tables, bold, code spans, links and images all work, and the Markdown half of
`RenderComment` needs no change.

Which leaves the question of what raw HTML `RenderComment` actually emits — and the answer, read from the current
code rather than from this document's earlier draft, is **none**. `RenderComment`
(`internal/scm/comment.go:28`) emits the marker, a bold line, a pipe table, and for a failure one sentence of plain
prose. Its doc comment still describes a collapsed `<details>` block holding the build log, but that branch was
removed when failures stopped quoting build output into a public comment; there is no `<details>` in the function
any more. The doc comment is stale and should be corrected while this is fresh, independently of Bitbucket.

So the whole `Flavor` / `RenderCommentFlavor` design in the earlier plan is **dropped**. There is nothing to flavor.
The marker is the only raw-HTML dependency in the rendered comment, and `MarkerStyle` above handles it. If a future
change reintroduces a `<details>` block, `Flavor` comes back — and the note to leave behind is that it must, because
Bitbucket will render it as visible text.

One incidental observation while reading these comments: a four-space-indented `-` list immediately after a paragraph
line rendered as literal text with the dashes visible, in Vercel's other comment. Bitbucket's renderer is
CommonMark-ish but not GitHub-flavored Markdown, and nested lists are where it diverges. `RenderComment` does not
emit a nested list; it should not start.

The emoji in `stateIcon` (`internal/scm/comment.go:91`) are literal Unicode, not GitHub `:shortcode:` syntax, so they
carry across unchanged. That was luck rather than design and is worth a comment on the function, because switching to
shortcodes would silently break one platform.

## There are no check runs

GitHub gets a check run alongside the comment (`internal/scm/github/github.go:281`), keyed by name and head SHA,
updated in place through queued → in progress → conclusion. Bitbucket has **commit build statuses** instead:
keyed by a `key` string on a commit, with a state, a name, a description, and a URL.

The request body is `{state, key, url, name, description}`, of which **`state`, `key` and `url` are required** and
`name`/`description` are optional but displayed. The four states are `INPROGRESS`, `SUCCESSFUL`, `FAILED` and
`STOPPED`. Four consequences.

The state vocabulary is narrower. `checkConclusion` maps `StateSkipped` to GitHub's `neutral`
(`internal/scm/github/github.go:348`) — "this ran and deliberately did nothing", which is the honest report for a
pull request that touched no documentation. **Bitbucket has no neutral.** A skip has to be reported as either
`SUCCESSFUL`, which claims a preview exists, or `STOPPED`, which reads as a problem. Recommend `STOPPED` with a
description that says why, and accept that it looks worse than it is.

There is no queued state either. GitHub's check run starts at `queued` before the build picks the job up; Bitbucket's
narrowest equivalent is `INPROGRESS`, so a preview sitting in the queue is reported as building. Nobody is harmed by
that, but it means the build status cannot be used to distinguish "waiting" from "running" — the dashboard and the
comment can, and they are the sources of truth.

**`url` being required settles the earlier open question.** GitHub's check run only sets `details_url` once the state
is ready (`internal/scm/github/github.go:299`); here there is no such option, and before the preview exists the only
candidate is the dashboard — which under the ziti listener or a loopback-only daemon has no address a reviewer's
browser could open (`localOrigin`, `cmd/docpreview/main.go:276`, returns `""`). So build statuses are not merely
optional as a matter of taste: **the API cannot be called at all without a reachable URL.** Skip them entirely when
`localOrigin` is empty, and make them best-effort otherwise — logged and swallowed exactly as the check run is
(`internal/scm/github/github.go:205`), because the comment is the durable artifact and the status is a convenience.

`Retract` deletes the comment and deliberately leaves the check run alone, on the grounds that erasing what happened
to a specific commit is revisionist (`internal/scm/github/github.go:378`). The same rule applies to a build status,
for the same reason.

One caveat worth knowing before an operator asks: Atlassian documents that a commit build status created through the
API does not necessarily satisfy a pull request's *merge check* for builds. So "make the docs preview a required
check" is not a promise this feature can make on Bitbucket, and the runbook should not imply it.

## Rate limits, error envelopes, and retry

Both are different enough from GitHub that `errorFromResponse` and the retry predicate must be written fresh even
though `APIError` and `Retryable` are shared.

**The error envelope is nested and, unusually, tells you the fix.** A 403 for a missing scope, observed live:

```json
{"type": "error", "error": {
  "message": "Your credentials lack one or more required privilege scopes.",
  "detail": {"required": ["read:webhook:bitbucket"],
             "granted": ["read:repository:bitbucket", "read:pullrequest:bitbucket", …]}}}
```

GitHub puts its message at `{"message": …}`; Bitbucket puts it at `{"error": {"message": …}}` and sometimes adds
`error.detail`. When `detail.required` is present the parser should lift it into the error text, because that turns
an opaque 403 into *"the token lacks read:webhook:bitbucket; add it to bitbucket.access_token"* — which is the
house rule about errors naming the fix, handed over for free.

Note the scope spelling while it is in front of us: granular, colon-separated, product-suffixed —
`read:repository:bitbucket`, `write:pullrequest:bitbucket`, `read:webhook:bitbucket`. Not GitHub's
`contents: read`, and not the older Bitbucket `repository:write` spelling either. The runbook's scope table has to
use whatever the token-creation dialog shows, which is a third naming (checkbox labels), so **write the scope table
from a screenshot of the dialog, not from the API error strings.**

**Rate limiting has a different header set and a nastier `Retry-After`.**

| | GitHub | Bitbucket Cloud |
|---|---|---|
| Status | 403 *or* 429 | 429 |
| "how many left" | `X-RateLimit-Remaining: 0` | `X-RateLimit-NearLimit: true` once under 20% |
| "when can I retry" | `X-RateLimit-Reset`, epoch seconds | `Retry-After`, seconds **or an HTTP-date** |
| Ceiling | documented per endpoint class | rolling one-hour window, scaled by the workspace's paid seat count |
| Secondary limit | yes, a 403 with non-zero remaining | not documented as a separate mechanism |

Two traps, and this codebase has already been bitten by the first one on the other platform. `internal/scm/CLAUDE.md`
records that `X-RateLimit-Reset` was parsed as RFC3339 when it is epoch seconds, so every rate-limited response
reported a zero wait. Bitbucket's `Retry-After` is the mirror image: it is *usually* an integer and *may* be an
HTTP-date, so a parser that only handles the integer form will silently produce zero on the day it is a date. Handle
both, and keep the existing distinction between an explicit zero (retry now) and an absent header (no idea, use the
caller's fallback) — that one is also already documented as a bug that was fixed once.

The second trap is `X-RateLimit-NearLimit`. It is a **boolean**, not a count, and it is present only when the
remaining allowance has fallen below 20%. There is no equivalent of GitHub's "remaining: 0" to test, so
`Retryable()` for Bitbucket keys on the 429 alone. `NearLimit` is worth logging at warn — it is the only early
warning available that a supersede storm is about to start failing — and worth nothing at all as a control input,
because a client that slowed itself down on a boolean would slow down for the rest of the hour.

**The 5xx rule carries over unchanged and for the identical reason.** A 5xx is not retried even though `Retryable()`
would allow it, because the upsert finds-then-creates and a 500 may have created the comment before failing to say
so, so a retry posts a second comment. Bitbucket's create-comment endpoint has exactly the same shape and exactly
the same hazard.

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
