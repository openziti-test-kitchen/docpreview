# Getting the zrok exposer into production

For a long time every real run of docpreview used the `local` exposer. Its URLs are paths on the daemon's own
listener (`internal/expose/local.go:111`), and the daemon listens on `127.0.0.1:8471`, so the link in the pull
request comment is reachable by exactly one person: whoever is sitting at the machine that built it. That is
fine for proving the pipeline and useless for the thing the pipeline exists to do.

`zrok2` is the first real answer because it is already a dependency, already the default in
`config.DefaultServer` (`internal/config/config.go:448`), and already the intended endpoint of the GitHub App
smoke test — step 4 of "Then" in [11-github-setup-state.md](11-github-setup-state.md) is literally "switch
`exposer.kind` to `zrok2`".

**It has now been run against a real zrok account**, and that is where most of what follows was either confirmed or
corrected. Two things stopped it working outright — a name that has to be registered before a share can bind it,
and a frontend endpoint that is a hostname rather than a URL — and **neither was reachable from the local git
simulator**, where nothing ever creates a share and no comment is ever rendered for a browser. Both are fixed and
written up below. The rest of this document is still the gap between "it publishes" and "it is production".

## What the code does today, end to end

`NewZrok` loads the on-disk zrok environment at construction (`internal/expose/zrok.go:64`) and resolves the
namespace, falling back to the environment's default (`zrok.go:77-85`). `Validate` checks `IsEnabled` and then
makes one `GetEnvironmentDetail` round trip so a revoked account token becomes a startup error rather than an
opaque 401 on the first pull request (`zrok.go:99-122`). `main.go:303-304` is the only construction site.

`Publish` (`zrok.go:131`) does six things in order:

1. Withdraws this preview's own earlier publication, so the name is free (`zrok.go:142`).
2. Refuses the name if a *different* live preview holds it (`zrok.go:143-153`).
3. Builds a `sdk.ShareRequest` — public share, proxy backend, `Target: "docpreview:" + spec.Key()`, one
   `NameSelection{Namespace, Name}`, permission and OAuth from config (`zrok.go:156-171`). The tag is the
   publication key, not the preview id, because a preview now has a branch share *and* one per build — see
   [02-exposers.md](02-exposers.md).
4. **Registers the name** in the namespace (`ensureName`, `zrok.go:178-180`), then calls `sdk.CreateShare`, retrying
   once behind `reapName` (`zrok.go:182-194`). Step 4 did not exist and nothing could publish without it; see
   below.
5. Opens an overlay listener for the returned share token and starts an `http.Server` on it
   (`zrok.go:196-214`). No TCP port is bound; this is why the `Exposer` interface takes an `http.Handler`
   rather than a port ([02-exposers.md](02-exposers.md)).
6. Takes `shr.FrontendEndpoints[0]` as the origin, **prefixes `https://` if it has no scheme**, joins the site's
   `base_url` onto it, and returns that as the URL for the comment (`zrok.go:227-248`). A share that came back
   with no frontend endpoints is torn down and reported as an error, because a preview with no URL is worse than
   a failed build.

The resulting URL is whatever the controller returned plus the base URL — the code does not construct the
hostname. Its *shape* is asserted in exactly one place: `endpointsMatchName` (`zrok.go:553`) assumes the
first DNS label of a frontend endpoint is the name we asked for.

**`FrontendEndpoints` reports a bare hostname, not a URL.** Observed against a real account. Without the scheme
prefix at step 6 the preview URL went into the pull request comment with no `https://`, so the link resolved
relative to github.com and every reviewer who clicked one got a 404 there. Nothing in the daemon was wrong and
nothing logged an error; the URL was simply not a URL. `JoinURL`'s tests use `https://x.share.zrok.io` as a
fixture (`expose_test.go:118`) — a plausible-looking value, not a recorded one — which is why no test caught it.

`Publication.Close` calls `withdrawEntry` with the entry identity (`zrok.go:253-256`), which is the guard that
stops a superseded publication from tearing down its own replacement — under zrok that would not merely 404 the
preview, it would delete the share (`zrok.go:269-284`). `close` shuts down the HTTP server, closes the overlay
listener, and calls `sdk.DeleteShare` (`zrok.go:298-313`) — and touches no *name*, for the reason in "Releasing a
name" below. `Reap` lists shares filtered by this environment's ziti identity and by the `docpreview:` target
substring, skips the tokens it knows are live, and deletes the rest whose publication key is not in `keep`
(`zrok.go:323-371`).

That is a complete implementation of the interface. The problems are not gaps in it; they are things it does
that are wrong, and things it assumes that nobody has checked.

## Reserved versus ephemeral is not the question any more

In zrok v1 a stable public address meant a *reserved share*, and the doc comment on `Zrok` still frames it that
way (`zrok.go:35-40`). In the vendored v2 SDK that framing is obsolete, and it is worth being precise about why,
because the answer changes what has to be built.

`sdk.ShareRequest` still declares `Reserved bool` and `UniqueName string`
(`sdk/golang/sdk/model.go:70-71`), and **neither field is read by anything**. `CreateShare` switches on share
mode and calls `newPublicShare`, which copies eleven fields and not those two (`sdk/golang/sdk/share.go:79-100`).
The wire model has no `reserved` field at all — `rest_model_zrok.ShareRequest` is `accessGrants`, `authScheme`,
`backendMode`, `basicAuthUsers`, `envZId`, `nameSelections`, `oauthEmailDomains`, `oauthProvider`,
`oauthRefreshInterval`, `permissionMode`, `privateShareToken`, `shareMode`, `target`. So there is no such thing
as a reserved *share* on this API surface. Every share docpreview creates is the same kind of share.

Permanence moved to the **name**. `rest_model_zrok.Name` carries `Name`, `NamespaceToken`, `ShareToken`,
`CreatedAt` and — the interesting one — `Reserved bool`. And there is a whole name lifecycle in the client that
docpreview never touches: `CreateShareName`, `DeleteShareName`, `UpdateShareName`, `ListNamesForNamespace`,
`ListAllNames`, `ListShareNamespaces` (`rest_client_zrok/share/share_client.go:174-350`).

This document originally argued that docpreview creates names *implicitly*, as a side effect of `NameSelections`
on the share request, and that a name bound to a share rather than owned outright was the right choice because
nothing then has to remember to release it.

**That was wrong, and the run against a real account is what showed it.** zrok v2 does not create a name
implicitly. `CreateShare` with a `NameSelection` naming an unregistered name fails 409 — see the next section —
so no named share could be created at all. The name has to exist first, and creating it is a separate call:
`CreateShareName`, wrapped as `ensureName` (`zrok.go:385`).

So docpreview owns names, and the cost the original argument was avoiding is real. A name outlives every share
bound to it, which is what keeps a preview's URL stable across rebuilds and restarts, and it means the account
accumulates one name per preview name ever published (`zrok.go:383-384`).

**For a long time nothing released them.** That leak is now closed, and the shape of the fix is the interesting
part — see "Releasing a name" below.

**Invariant, restated:** a name docpreview registers must be reusable by the next process without coordination.
`ensureName` treating "already exists" as success is what makes that hold.

The consequence for [08-storage.md](08-storage.md)'s "The URL can move" is the good outcome. The URL is a
function of the name, the name is a function of the template, and the template is a function of the repository
and the branch. So a restart republishes each recorded preview under `p.Name` (`daemon.go:785-790`), gets a new
share with the same name, gets the same hostname back, and `pub.URL != p.URL` is false — no row rewrite, no
comment churn (`daemon.go:804-814`). Each preview's build shares are then republished the same way, from
`builds.name` (`restoreBuildShares`, `daemon.go:529`). The comment written once and edited thereafter, which is the
whole product, survives.

## A name must exist before a share can bind it

This is the bug that stopped the whole exposer working, and it was invisible until a real zrok account was
involved. Nothing in the local git simulator or in any test creates a share.

`sdk.CreateShare` with `NameSelections: [{Namespace, Name}]` for a name that is not registered in that namespace
answers **409**. So every publish failed, every build reported failed, and no preview ever got a URL. The fix is
one call: `ensureName` registers the name before `CreateShare` (`zrok.go:178-180`).

`ensureName` is **idempotent by design rather than by check** (`zrok.go:385-403`). Asking whether the name exists
and then creating it is a race against another publish, and the create is the cheaper of the two calls anyway. So
"already exists" is the success case and is matched on rather than returned.

**Matched on the generated type *and an empty payload*, not on the message.** The 409 that means "already exists"
arrives with an empty body — the whole of it is `[POST /share/name][409] createShareNameConflict ""` — so there is
no text to match. But the type alone is not enough, and reading `controller/createShareName.go` is what showed it:
**four situations answer 409 and only one of them is this one.**

| Payload | Situation |
|---|---|
| empty | the name already exists — the success case |
| `names limit reached; cannot reserve additional names` | the account is at its reserved-name limit |
| `'…' is not a valid share name; failed profanity or DNS check` | the name will never be registrable |
| a stale-frontend-mapping message | the controller could not heal the mapping |

All four are `*share.CreateShareNameConflict`, so payload presence is the only discriminator available. Matching
the type alone — which this did — reported a *registered name* to an account that had hit its name limit
(`isNameAlreadyExists`, `zrok.go:427`), and the
`CreateShare` a few lines later then failed for a reason that never mentioned quotas. That went from theoretical to
likely the moment builds got their own shares: one name per commit reaches a limit far sooner than one per branch.
Covered by `TestQuotaConflictIsNotAnExistingName` (`zrok_test.go`), with the controller's own strings as fixtures.

**It still cannot tell a name this account owns from one another account holds.** Nothing can, given the empty
body. Treating both as success is safe because the `CreateShare` that follows binds the name and fails on its own
if it is not ours — one call later, with its own error, which is the error an operator can act on.

`CreateShareNameBody` carries only `Name` and `NamespaceToken`. There is no reserved flag to set, and the
controller hardcodes `Reserved: true` (`controller/createShareName.go`) — which turns out to be the whole leak, for
the reason the next section gives.

## Releasing a name

Teardown deletes a share. The name is a separate object that survives it — deliberately — and it is also the object
an account's quota counts. So something has to release it when the pull request is gone for good.

**The obvious fix is the wrong one.** `close` (`zrok.go:298`) serves three callers and only one of them is a teardown:
Publish's own withdraw on a rebuild, the `Publication.Close` the daemon uses when it supersedes a build, and
shutdown. Releasing there would rehost every rebuilt preview at a new address on every push, silently, while the
pull request comment went on advertising the old one — worse than the leak and much harder to notice. So releasing
is the daemon's decision, made once, in `Daemon.releaseNames` (`internal/daemon/daemon.go:980`), reached through the
optional `expose.NameReleaser` interface (`internal/expose/expose.go:112`), and `close` must never touch a name.
`TestRebuildMustNotReleaseTheName` (`internal/daemon/releasename_test.go`) is what keeps that true.

**`ReleaseName` de-reserves before it deletes, and the order is the point.** The controller's `unshare` already
deletes any name bound to the share it is deleting whose `reserved` flag is false
(`controller/unshare.go`, `cleanupShareNameMappings`). `PATCH /share/name` flips that flag, has no live-share
conflict check, and works on a name a share is still bound to. So:

1. `UpdateShareName` with `reserved` false (`zrok.go:478-492`). A 404 means the name is already gone, which is
   success.
2. `DeleteShareName` (`zrok.go:494-510`). A 409 — "still attached to share" — is the *expected* path when a share is
   still bound, and
   also a success: unshare will finish the job. A name with no share to reap it, which is the case for a preview
   whose republish failed after a restart, is deleted outright here.

That is what makes it self-healing. A crash between de-reserving and withdrawing leaves a non-reserved name whose
share the next startup's `Reap` deletes, and the name goes with it. An explicit delete that never runs leaks
silently and forever. Both orders work, so the daemon releases *before* withdrawing, leaving the flag already clear
if the withdraw is the thing that fails.

One trap in the generated client: `UpdateShareNameBody.Reserved` is tagged `omitempty`, so `false` is not
serialized at all — and the controller reads an absent flag as false. Setting it explicitly produces the same
request. Do not "fix" it with a pointer field.

**Which names.** Every name the preview ever took: its own and one per build share, and `ReleaseName` is called once
per distinct name rather than once per publication (`daemon.go:988-1023`). The live publications are not the whole
set, so `releaseNames` reads the `previews` and `builds` rows too — a preview restored after a restart
whose republish failed, or a build whose artifacts were pruned, has a recorded name and no publication, and those
are exactly the names most likely to be leaked already. It reads them before `DeletePreview` runs, which is a few
lines later in the same function.

**A release failure cannot fail a teardown.** The pull request is gone either way; what is left is one name against
a quota, which is worth a warning naming the name. Aborting would strand the artifacts, the workspace, the cache
and the comment as well (`TestTeardownSurvivesAReleaseFailure`).

**Names leaked before this existed are still there** — one per branch from every teardown so far, and the account
has no way to read its own limit. Recovering them means `ListNamesForNamespace` plus the same release path, which
is the `docpreview names prune` in the build order below.

## Names and collisions

`RenderName` renders the template, sanitizes the branch into `{{.Name}}`, and sanitizes the whole result again
(`internal/expose/expose.go:205-226`). `model.SanitizeName` lowercases, replaces every non-ASCII-alphanumeric run
with a single hyphen, trims hyphens, and appends a six-hex-character hash of the original **only when the
transformation was lossy or the result exceeded 48 characters** (`internal/model/model.go:118-151`). So `main`
stays `main`, and `feature/foo` and `feature_foo` do not collapse onto each other.

A build share's name is not rendered a second time. `publishBuildShare` appends `-<sha7>` to the branch share's name
(`internal/daemon/daemon.go:1442`), so a template that separates repositories keeps separating them here, and the two
names sort next to each other in any list of shares.

`DefaultNameTemplate` is `{{.Repo.Name}}-{{.Name}}` (`config.go:435`), not the branch alone. The init prompt
says why: "zrok names are unique per namespace, so two repos with a main branch would collide"
(`cmd/docpreview/init.go:141-142`).

Verified, and worse than the comment says. **`zrok2.namespace` left blank does not mean "your own namespace".**
`NewZrok` falls back to `root.DefaultNamespace()` and errors if it is empty (`zrok.go:77-82`), but
`DefaultNamespace` never returns empty — it returns the literal string `"public"` with source `"binary"` unless
config or `ZROK2_DEFAULT_NAMESPACE` overrides it (`environment/env_v0_4/api.go:87-103`). So that error branch is
dead code, and the default configuration publishes into a namespace shared with every other zrok user. In
`public`, `docs-main` is not contested by your other repository. It is contested by strangers.

What a collision looks like to a user depends on who holds the name:

- **Another live preview in this process.** `Publish` refuses, with an error naming the other publication key and the
  template that would separate them (`zrok.go:144-153`). This is the good case: a failed build, a comment that
  says so, and a fix in the message.
- **A share this daemon left behind.** `reapName` reclaims it once and `CreateShare` is retried once
  (`zrok.go:186-194`). Exactly once, because a retry loop against a name someone else owns is a busy-wait.
- **Anyone else, including another docpreview.** `CreateShare` fails, `reapName` finds nothing matching, and the
  build fails with `creating zrok share "docs-main" in namespace "public": …`. The operator has no way to tell
  from that message whether the name is held by a stranger or by their own second daemon.

Recommendations, in order of how much they matter:

1. **Require a namespace.** `config.validate` today performs no `ZrokConfig` checks whatsoever (`config.go:511`).
   It should reject `exposer.kind: zrok2` with an empty `zrok2.namespace`, because the alternative is silently
   publishing into `public`. Boot-time refusal with "run `zrok2 config set defaultNamespace <ns>` or set
   `exposer.zrok2.namespace`" is the right shape — same reasoning as `Validate` existing at all.
2. **Keep `{{.Repo.Name}}-{{.Name}}` as the default.** It is correct for two repositories, which the branch alone
   is not, and it is stable across pushes, which `{{.HeadSHA}}` is not.
3. **Recommend `{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}` in the error message**, which `Publish` already does
   (`zrok.go:149`), and in the init prompt for anyone watching more than one org.
4. **Leave `SanitizeName` alone.** 48 characters (`model.go:99-101`) is already conservative against the RFC 1035
   limit of 63, which leaves room for a namespace-qualified hostname. The lossy-only hash suffix is what keeps
   `main` readable while keeping `feature/foo` distinct from `feature_foo`.

## Two previews, one name, at the same time

The collision check releases the mutex before `CreateShare` (`zrok.go:144-154`) and does not take it again until
after the listener is up (`zrok.go:223-225`). The daemon's commit lock is **per preview**, so two different previews
are free to be inside `Publish` simultaneously — and two different previews rendering to one name is exactly the
situation the check exists for.

Both pass the check. Both call `CreateShare` with the same `NameSelection`. One wins. The loser's error path
calls `reapName`, and `reapName` — unlike `Reap`, which skips `liveTokens` (`zrok.go:354`) — deletes **any**
docpreview-tagged share whose endpoint matches the name, including the one the winner created seconds ago
(`zrok.go:537-548`). It then retries and succeeds. The loser takes the name; the winner is left holding an
overlay listener for a deleted share and a database row that says `ready`.

This is the same class of bug as the two already recorded in [02-exposers.md](02-exposers.md) — a name treated
as an identity — and it is the one remaining place where docpreview can serve one pull request's site at another
pull request's URL. Two fixes, both needed:

- **`reapName` must skip live tokens**, exactly as `Reap` does. A share this process is serving is never an
  orphan, by definition.
- **The name check and the share creation must be one atomic step.** Not by holding `z.mu` across a network call
  — that would serialize every publish behind one slow controller — but with a per-name reservation:

```go
// reserve claims name for previewID, or reports who holds it. Held across
// CreateShare, because the check is worthless if the window between checking and
// creating is wide enough to drive a second publish through.
func (z *Zrok) reserve(name, previewID string) (holder string, ok bool)
func (z *Zrok) release(name, previewID string)
```

## Reaping, orphans, and the footgun

`Reap(ctx, keep)` lists shares with `EnvZID` set to this environment's ziti identity and `Target` set to the
substring `docpreview:` (`zrok.go:328-339`), re-checks the prefix properly because the API filter is documented
as a substring match (`rest_client_zrok/metadata/list_shares_parameters.go:122-126`), skips live tokens, and
deletes everything whose trailing **publication key** is not in `keep` (`zrok.go:353-369`). The daemon assembles that
set from the `previews` rows *and* every `builds` row with a recorded URL, and skips the sweep entirely if it cannot
read all of it — see [08-storage.md](08-storage.md).

At startup `keep` is nil (`daemon.go:463`), and `keep[x]` on a nil map is false, so **everything
docpreview-tagged in this environment is deleted**. The documented reasoning is sound: nothing is serving yet,
so every share is from a previous process ([08-storage.md](08-storage.md), `daemon.go:455-497`). Then each
`ready` row with artifacts on disk is republished, and then its build shares, in that order, because reaping
afterwards would delete what was just restored.

**A hard kill leaves shares behind and that is fine.** The share lives on the controller; the process that made
it is gone. `Reap(ctx, nil)` on the next start removes it, and because names go with shares, the name is free
for the republish that follows moments later. Nothing needs a shutdown hook to be correct. This is the one part
of the design that is already right for the failure mode Windows actually produces — background processes there
are killed hard (`TODO.md`, verification gaps).

**Two daemons sharing one zrok environment destroy each other.** This is the footgun, and it deserves the
prominence:

The filter is `EnvZID` plus `docpreview:`. `EnvZID` is `z.root.Environment().ZitiIdentity` — a property of the
on-disk environment at `$HOME/.zrok2` (`environment/env_v0_4/dirs.go:9-24`), not of the daemon. Two docpreview
processes on one host under one user account share that environment, share that identity, and are
indistinguishable to `Reap`. So starting the second one deletes every share the first one is serving. The first
daemon keeps its `http.Server` running on an overlay listener for a share that no longer exists, its `live` map
still holds the publication, its dashboard still says `ready`, and every comment link is dead. Nothing logs an
error, because deleting a share you believe you own is a normal thing to do.

This is not hypothetical for this repository. [11-github-setup-state.md](11-github-setup-state.md) describes
exactly two configurations, deliberately separated — the demo on `:8493` and the GitHub App work on `:8471` —
with separate config files, separate data directories, and *the same `$HOME`*. Running the demo would reap the
App daemon's previews.

The fix has two halves, and the first is the important one:

1. **A per-instance zrok environment. Built** — `internal/expose/zrokenv.go`, `cmd/docpreview/zrok.go`.
   `environment.SetRootDirName` accepts an absolute path (`environment/api.go:11-13`,
   `env_v0_4/dirs.go:15-18`), and `applyZrokScope` calls it from `setup`, before `buildExposer` — `NewZrok`
   calls `LoadRoot` at construction, so afterwards is too late. Two installations then have two environments,
   two ziti identities, two `EnvZID`s, and `Reap` separates them for free.

   Two departures from the sketch above, both learned in the writing:

   - **A stored setting, not a config key.** `exposer.zrok2.root_dir` would have been a path an operator has to
     invent, in a file that is hand-written and full of comments a rewrite would destroy. What the operator
     actually has is a *choice between two*: the machine's `~/.zrok2`, which a developer very likely already
     has, and this installation's own at `<data_dir>/zrok2`. So it is `exposer.zrok.scope`, values `system` and
     `project`, in the settings table — settable from the dashboard and from `docpreview zrok use`.
   - **The choice is never guessed once it can be recorded.** With one environment enabled and nothing stored,
     startup adopts it *and writes it down*: without the write, enrolling the other one later would move the
     daemon to a different zrok account at the next restart, and the first thing it would do there is reap.
     With **both** enabled it adopts `project`, warns, and records nothing — so the dashboard keeps asking,
     because either answer may be an account something else is reaping. `chooseZrokScope` is that decision,
     separated out precisely so it can be tested: the alternative touches a process-wide global that cannot be
     undone inside a test binary.

   `rootDirName` being a mutable package variable is also why `UseZrokRoot` refuses a second, different call, and
   why `InspectZrokRoots` — which has to load a root that is *not* the one in force — takes a mutex and restores
   the global afterwards. `TestInspectingDoesNotChangeWhichRootIsInForce` is the guard on that.

   The cost is that `webhook-only` and `dashboard-only` have to be told separately, with `-zrok-home`: they read
   no config file, which is the whole reason they exist, so they cannot derive the daemon's answer. A share
   created from the wrong account reserves a name the previews cannot use.

2. **An instance token in the target.** Belt and braces for the case where an operator does point two daemons at
   one environment anyway. `Target` is free-form for our purposes — with an SDK listener nothing dials the
   target (`zrok.go:23-31`) — so `docpreview:<instance>:<previewID>` costs nothing but a two-way split instead
   of a `TrimPrefix`. Keep the `docpreview:` prefix as the outer guard, because that is what stops `Reap`
   deleting a share an operator made by hand.

**Invariant: `Reap` must not be able to delete a share belonging to a docpreview instance that is not this one.**

The remaining orphan class is unchanged and still unaddressed: a share created by a daemon whose database was
then deleted. Nothing claims it and nothing looks. With `ListNamesForNamespace` and `ListShares` both available
on the client, `docpreview shares list` is now a small command rather than a research project, and it is the
right home for the audit.

## Private shares, access grants, and who can actually read a preview

The config offers three controls (`config.go:311-335`) and they are not three points on one scale. Two of them
gate *zrok accounts* and one gates *browsers*, and conflating them is how a preview ends up either unreadable by
its reviewers or readable by the internet.

`Publish` always uses `sdk.PublicShareMode` (`zrok.go:156`). It never creates a zrok private share, and it
should not: a private share is reached with `zrok access`, which means every reviewer installs a zrok client and
runs a command — for a documentation link in a pull request comment that is not a review workflow. (For the
audience that would accept an installed client, that is what the `ziti` exposer is for.) Note also that
`NameSelections` is only sent for public shares (`sdk/share.go:64-77` versus `79-100`), so a private share would
have no stable name and the whole edit-one-comment design would collapse.

`open: true` sets `PermissionMode: open`; `open: false` leaves it `closed` with `AccessGrants` attached
(`zrok.go:160-165`). Both go on the wire (`sdk/share.go:88-91`). `access_grants` is a list of **zrok account
identifiers** — `init` says so, "Comma-separated zrok accounts allowed to reach a closed share"
(`init.go:150`). A reviewer on a documentation pull request does not have a zrok account. So `open: false` plus
`access_grants` is not "authorization for previews"; it is "authorization for people who already use zrok",
and for everyone else it is an outage.

`oauth_provider` is the one that gates a browser. When set, `Publish` copies the provider, the email patterns,
and a three-hour refresh interval onto the request (`zrok.go:166-170`), and `CreateShare` promotes the share's
auth scheme to `oauth` (`sdk/share.go:43-45`). The reviewer opens the link, is bounced to Google or GitHub, and
comes back authenticated. `oauth_email_domains` narrows it further, and blank means any account at that provider
— which is barely a gate at all, since anyone can make a Google account.

Two things follow, and both need saying plainly:

**The shipped default is "anyone with the link."** `DefaultServer` sets `Open: true` and no OAuth provider
(`config.go:448-451`), and `init`'s summary line for that combination is the honest one — "anyone with the link"
(`init.go:293-294`). For public documentation that is correct and is what Vercel does. For a private repository
it means the contents of an unreleased documentation change are one guessable hostname away from being public,
and the hostname is `<repo>-<branch>` in a shared namespace, which is not guessable so much as enumerable.

**`open` and `oauth_provider` are orthogonal, not exclusive.** Setting both leaves permission mode `open` while
the auth scheme is still `oauth`, so the OAuth gate applies. That is the combination a private repository wants:
no zrok-account requirement, a real identity check at the frontend. `init` presents them as a sequence of
questions and its `visibility` helper reports OAuth first (`init.go:289-300`), which happens to describe the
result correctly.

Recommendations:

1. **Reject `open: false` with neither `access_grants` nor `oauth_provider`** in `config.validate`. It produces a
   share nobody can reach, and `init`'s own summary calls it "closed, no grants yet" (`init.go:298`) — a state
   the file should not be allowed to persist in.
2. **Validate `oauth_provider` against `{google, github}`.** `init` restricts the choice (`init.go:156-157`); a
   hand-edited config does not, and a typo is currently sent to the controller.
3. **Warn at boot when a preview is world-readable**, once, naming the namespace. The operator who chose this
   deliberately loses nothing; the one who inherited the default finds out before a reviewer does.

## Failure modes

**The environment is not enabled.** `NewZrok`'s error message — "loading zrok environment (is zrok2 installed
and enabled?)" (`zrok.go:66`) — will almost never be the one you see, because `LoadRoot` does not fail on a
missing environment: `Assert` returns false, and `Default()` returns a `Root` with a nil `env`
(`environment/api.go:15-20`, `env_v0_4/api.go:22-33`, `root.go:50-75`). The check that actually fires is
`Validate`'s `IsEnabled` (`zrok.go:100-102`), which says `run 'zrok2 enable <account-token>'`. That is a good
message; it is just reached from the other function. Since `validate` runs before anything binds
([01-architecture.md](01-architecture.md)), the operator gets it at boot, which is the point.

**The account token is missing or revoked.** `Validate`'s `GetEnvironmentDetail` round trip exists for exactly
this (`zrok.go:109-114`), and its message names the fix. Revocation *while running* is not covered: the next
`CreateShare` fails, the build fails, and the comment says the build failed. Reasonable, given the alternative
is polling.

**The zrok service is unreachable.** `root.Client()` is not a cheap accessor — it constructs a transport and
then performs a `ClientVersionCheck` round trip every time it is called (`env_v0_4/api.go:42-62`). `zrok.go`
calls it in `Validate`, `Reap` and `reapName`, and `sdk.CreateShare` and `sdk.DeleteShare` each call it again
internally (`sdk/share.go:47`, `sdk/share.go:107`). So one publish is two version checks plus the share call,
and a controller upgrade that rejects this client's version breaks every publish with `building zrok client:
client version error accessing api endpoint …`. Worth a specific mention in the error wrapping, because
"unreachable" and "you are too old" want different responses from an operator.

**Share limits.** `ShareSummary` carries `Limited bool` (`rest_model_zrok/share_summary.go:30`), so a share can
be present and disabled. Nothing in docpreview reads it. A limited share leaves a `ready` row pointing at a URL
that does not serve, and `Reap` will not touch it because its publication key is in `keep`. Reading `Limited` during
`Reap` and logging it is a two-line change and the only warning an operator would get before the account stops
accepting new shares.

**`CreateShare` takes no context.** `sdk.CreateShare(root, request)` has no `ctx` parameter
(`sdk/share.go:14`), and neither does `sdk.NewListener` — which additionally carries a hard-coded 30-second
connect timeout and `WaitForNEstablishedListeners: 1` (`sdk/listener.go:11-16`). This is the load-bearing fact
behind the commit lock (`daemon.go:299-310`): a publish already in flight cannot be cancelled, and publishing a
name is destructive to whoever holds it, so "am I still the current build?" and "publish" have to be one atomic
step or a superseded build tears down the newer preview and replaces it with older content. Nothing here should
change. What should be recorded is the consequence for latency: a preview publish can occupy its commit lock for
up to thirty seconds with no way to interrupt it, and the daemon's `Publish` call site passes a `ctx` that the
SDK simply ignores (`daemon.go:822`).

**The overlay listener leaks a ziti context per publish.** `NewListenerWithOptions` builds a fresh
`ziti.NewContext` from the identity file on every call and returns only the listener
(`sdk/listener.go:18-40`). Nothing in the SDK or in docpreview ever closes that context — `entry.listener.Close()`
(`zrok.go:306`) closes the listener, not the context that created it. A long-lived daemon rebuilding previews on
every push accumulates one ziti context, control channel and API session per publish, for the life of the
process. This is upstream behaviour, not a docpreview bug, but it is docpreview's problem: it makes a restart a
periodic necessity rather than a casual convenience, and it is the first thing to measure once real traffic
exists.

## Restart behaviour

What the operator sees: `reap` deletes every docpreview share, then each `ready` preview is republished in a
loop (`daemon.go:379-396`). Each iteration is a `CreateShare` (two HTTP round trips including the version check)
plus a `NewListener` (a new ziti context, a control-channel connection, up to 30 seconds of connect timeout).
Serially. Ten previews at two seconds each is twenty seconds of every preview link being dead; one preview whose
listener hits the timeout stalls the previews behind it.

Comment URLs do not churn, for the reason given above: the name comes out of the database and the hostname is a
function of the name, so `pub.URL == p.URL` and neither the row nor the comment is rewritten
(`daemon.go:436-446`). That is the property worth protecting, and the one that most obviously breaks if anyone
ever makes the URL depend on the share token.

Recovery should be made concurrent, bounded by `workers`. It is embarrassingly parallel — each republish touches
one preview ID, and the commit lock is per preview — and it converts "twenty seconds dark" into "two seconds
dark" for the cost of an `errgroup`.

## What has to be true operationally

A zrok account, an enrolled environment on the daemon's host, and a namespace that is not `public`.

The environment is a directory: `$HOME/.zrok2`, holding `metadata.json`, `config.json`, `environment.json` and
`identities/environment.json` (`env_v0_4/dirs.go:9-64`). The account token and the ziti identity name live in
`environment.json`; the ziti identity file under `identities/` is what `NewListener` loads
(`sdk/listener.go:19-27`). Losing that directory means `zrok2 enable` again, a new `EnvZID`, and every existing
share becoming invisible to `Reap` — orphans with nothing to claim them.

So it is state, and it has to be treated as state:

- **In a container**, `$HOME` is whatever the image says (commonly `/root`), and an ephemeral container
  filesystem means the environment does not survive a restart. Either mount a volume at the environment
  directory, or — better — set `exposer.zrok2.root_dir` to a path inside a volume you already mount, since
  `SetRootDirName` accepts an absolute path (`env_v0_4/dirs.go:16-18`). Do not bake an enabled environment into
  an image: it contains an account token, and two containers from that image would share an `EnvZID` and reap
  each other.
- **Under systemd**, a service running as a dedicated user needs `HOME` set or `root_dir` configured; `User=`
  alone does not set `HOME` in every systemd version, and `os.UserHomeDir()` returning the wrong directory
  silently produces a fresh, unenabled environment. `StateDirectory=docpreview` plus
  `exposer.zrok2.root_dir: /var/lib/docpreview/zrok` is the arrangement with no ambiguity in it.

```yaml
exposer:
  kind: zrok2
  zrok2:
    # Required. Blank silently means the shared `public` namespace.
    namespace: acme-docs
    # Project and branch. The branch alone collides across repositories.
    name_template: "{{.Repo.Name}}-{{.Name}}"
    # Anyone with the link. Correct for public docs, wrong for a private repo.
    open: true
    # Gates browsers, not zrok accounts. This is the one a reviewer notices.
    oauth_provider: ""
    oauth_email_domains: []
    # Proposed: one environment per instance, so Reap cannot cross instances.
    root_dir: /var/lib/docpreview/zrok
```

`docpreview doctor` should report the environment root, whether it is enabled, the resolved namespace *and where
that value came from* — `DefaultNamespace` and `ApiEndpoint` both return a `(value, source)` pair
(`env_v0_4/api.go:64-103`) and the exposer already logs the namespace source at debug (`zrok.go:84`). "Namespace
`public`, from binary" is the single line that would have prevented the whole class of collision this document
spends a section on.

## Testing

Almost everything worth testing does not need an account, but none of it is reachable today, because
`sdk.CreateShare`, `sdk.DeleteShare` and `sdk.NewListener` are package-level functions and `Reap` reaches
through `z.root.Client()` into a generated OpenAPI client. `env_core.Root` *is* an interface
(`environment/env_core/model.go:6-35`), but it has twenty-four methods and faking it still leaves the
package-level SDK calls unfaked. The seam has to be narrower and it has to be ours:

```go
// zrokAPI is the slice of zrok the exposer actually uses. The real
// implementation wraps env_core.Root and the package-level sdk functions; the
// test one is a map. Deliberately not env_core.Root: that interface has
// twenty-four methods and does not cover CreateShare at all.
type zrokAPI interface {
    Enabled() error                                        // Validate's checks
    Namespace() (name, source string)
    EnvironmentID() string
    CreateShare(req *sdk.ShareRequest) (*sdk.Share, error)  // no ctx: the SDK has none
    DeleteShare(token string) error
    CreateShareName(ctx context.Context, name string) error // idempotent; swallows the empty-body 409 only
    SetNameReserved(ctx context.Context, name string, reserved bool) error // PATCH; 404 is success
    DeleteShareName(ctx context.Context, name string) error  // 409 means a share is still bound: success
    ListShares(ctx context.Context, envZID, target string) ([]shareRow, error)
    Listen(token string) (net.Listener, error)
}

// shareRow is the subset of rest_model_zrok.ShareSummary Reap reads.
type shareRow struct {
    Token             string
    Target            string
    FrontendEndpoints []string
    Limited           bool
}
```

`zrokShare.listener` is already typed as `interface{ Close() error }` (`zrok.go:56`), so half of this shape is
anticipated. `NewZrok` keeps its signature and constructs the real implementation; a `newZrokWithAPI` constructor
is what tests call, mirroring how `ziti_test.go` reaches past `Validate` by setting `z.bound` directly
(`internal/expose/ziti_test.go:35-39`) and how the offline unit tests sit beside a separate
`ziti_integration_test.go`.

With that seam, testable offline — and each of these should have a test, because each corresponds to something
argued above:

| What | Why it needs a test |
|---|---|
| Name templating and sanitization | Already covered (`expose_test.go:23-112`); no fake needed |
| Config validation: namespace, closed-with-no-grants, oauth provider spelling | All three are new refusals |
| A second preview is refused a live name, naming the incumbent | The invariant from [02-exposers.md](02-exposers.md) |
| Concurrent publish of two previews with one name: the winner survives | The race in "Two previews, one name" |
| `reapName` does not delete a live share | Same bug, other half |
| Republish keeps the URL | The property the whole comment design rests on |
| Replace-then-close leaves the replacement live | `withdrawEntry`'s identity check (`zrok.go:269-284`) |
| `Reap(ctx, nil)` deletes all, `Reap(ctx, keep)` deletes the complement | The startup contract |
| `Reap` skips shares whose target merely contains the prefix | Guarded in code (`zrok.go:359`), untested |
| `Reap` ignores a share from a different `EnvZID`/instance | The footgun, once fixed |
| A share with no frontend endpoints is torn down, not returned | `zrok.go:236-246` |
| A build share and its branch share coexist, and closing one leaves the other | `Spec.Key()`; done as `TestTwoPublicationsPerPreview` |
| A rebuild does not release the name | Done as `TestRebuildMustNotReleaseTheName` (`internal/daemon`) |
| An empty-payload 409 is "exists" and a quota 409 is not | Done as `TestQuotaConflictIsNotAnExistingName` |
| `ReleaseName` de-reserves before deleting, and a 409 from the delete is success | Needs the seam below; the calls have been read, not run |
| `ensureName` treats an empty-body 409 as success and any other error as fatal | The bug that stopped every publish |
| A publish fails if the name cannot be registered, before any share exists | Otherwise the failure surfaces one call later, from `CreateShare` |
| A scheme-less frontend endpoint becomes an `https://` URL | The relative-link bug; needs a fixture without a scheme |
| `Limited` is surfaced | New |

What genuinely needs an account, and belongs in a build-tagged integration test next to
`ziti_integration_test.go`:

- That the frontend endpoint's first DNS label really is the requested name. `endpointsMatchName`
  (`zrok.go:553`) and the whole stable-URL argument depend on it and nothing has confirmed it.
- That a registered name is reusable by a new share after the previous share is deleted, without a delay.
- That the OAuth gate actually challenges a browser, and that `oauth_email_domains` excludes what it should.
- What `permissionMode: closed` plus `access_grants` does to a *public* share — the one place the security story
  is asserted from the field names rather than from behaviour.
- Share limits, and what `Limited` looks like when tripped.

## The order to build it in

Done, out of order, because a publish that fails is not a plan item:

- ~~**Register the name before creating the share.**~~ `ensureName`. This was the difference between an exposer
  that publishes and one that fails every build.
- ~~**Give the frontend endpoint a scheme.**~~ Otherwise the link in the comment is relative to github.com.
- ~~**Run the smoke test.**~~ A real account, a real namespace, a real pull request. It found both of the above and
  nothing else in this document changed as a result.

What is left:

1. **Require a namespace, and reject the incoherent visibility combinations,** in `config.validate`
   (`config.go:511`). Cheapest change, and it is the difference between publishing into your namespace and
   publishing into `public`.
2. **Add the `zrokAPI` seam** and a fake, converting nothing else. This is the change everything below depends
   on being testable. `CreateShareName` belongs on it, and the empty-body 409 is a case a fake has to be able to
   produce.
3. **Fix the name race**: `reserve`/`release` held across `CreateShare`, and `reapName` skipping live tokens.
   With tests, because this is the last way one pull request's site reaches another's URL.
4. **Add `exposer.zrok2.root_dir`** and call `environment.SetRootDirName` in `buildExposer` before `NewZrok`
   (`main.go:293-320`), plus the instance token in `Target`. Two daemons must not reap each other.
5. **Restart the daemon with previews live** and confirm the URLs and the comments do not move. Then kill it hard
   and confirm the next start reaps and republishes cleanly. Now cheap to do, since publishing works.
6. **`docpreview names prune`**, over `ListNamesForNamespace` and `ReleaseName`, to recover the names leaked before
   teardown released them. Teardown itself is done; this is the backlog of names it cannot reach.
7. **Surface `Limited`, and improve the version-mismatch error** — both are things a longer run will have taught
   you the shape of.
8. **Make recovery concurrent**, bounded by `workers`.
9. **`docpreview shares list`**, over `ListShares` and `ListNamesForNamespace`, closing the audit gap in
   `TODO.md` for both zrok and Frontdoor. Now has names to audit as well as shares.
10. **Then decide whether the ziti-context leak needs an upstream fix**, with a measurement from a daemon that
    has been up for a week rather than from reading `sdk/listener.go`.

## Not verified

Everything above cited to a file and line was read. These were not:

- **The frontend endpoint hostname format.** Now partly known: the endpoint is a **bare hostname with no scheme**,
  which is why `Publish` prefixes `https://`. Whether its first DNS label is always the requested name is still an
  assumption — `endpointsMatchName` (`zrok.go:553`) and `reapName` depend on it, and it has not been checked against
  a name that would make the two differ. The `JoinURL` tests still use `https://x.share.zrok.io`
  (`expose_test.go:118`), a plausible-looking fixture rather than a recorded value.
- **What `permissionMode: closed` and `accessGrants` enforce on a public share**, and against what identity. The
  fields are sent (`sdk/share.go:88-91`) and `init` describes them as zrok accounts (`init.go:150`); the
  server-side behaviour is not in the vendored client.
- ~~**Whether deleting a share releases its name.**~~ Answered from the controller source, not the field names:
  `createShareName` hardcodes `Reserved: true`, and `unshare` deletes a bound name only when that flag is false. So
  a share deletion releases nothing unless the name is de-reserved first, which is what `ReleaseName` now does.
  What remains unknown is whether the *release* actually lands against a live account — the two calls have been
  read, not run.
- **Whether `CreateShare` fails, or silently succeeds with a different name, when the requested name is registered
  but bound to somebody else's share.** `Publish`'s retry-once logic (`zrok.go:182-194`) assumes it fails. The
  unregistered case is now known to fail with 409. If the taken case instead succeeds under an assigned name, the
  returned endpoint would not match the requested name and the comment would carry a URL the database did not
  predict — the `pub.URL != p.URL` path (`daemon.go:436`) would cover it, but noisily.
- **Name limits on an account.** The error is now known — `names limit reached; cannot reserve additional names`,
  as a 409 payload from `POST /share/name`, which is what `isNameAlreadyExists` had to learn to tell apart. **The
  number is not**, and nothing can read it: all six countable dimensions default to `Unlimited` with
  `Enforcing: false` in the source, and `adminListAppliedLimitClasses` is admin-only. The hosted free tier
  publishes 5 GB/day, 25 environments, 50 share backends and 50 private frontends, and no reserved-name figure
  anywhere. It has to be measured — see [19-zrok-namespacing.md](19-zrok-namespacing.md).
- **Share and name limits on a zrok account**: that they exist is asserted in `zrok.go:316-322` and
  `02-exposers.md`; the numbers, and what the controller returns when one is hit, are not known.
- **What `ListShares` returns for a large account.** The parameters expose filters but no limit or offset
  (`list_shares_parameters.go:60-143`), so either it returns everything or it truncates somewhere invisible to
  this client. `Reap` correctness depends on completeness.
- **`ziti.NewContext` leak severity.** That a context is created per listener and never closed is read from
  `sdk/listener.go:18-40`; what it costs in file descriptors, memory and controller-side sessions over a week is
  not measured. It may be that the context is reference-counted internally and this is a non-issue.
- **Windows behaviour of `os.UserHomeDir()` under a service account**, which determines where the zrok
  environment lands for a daemon run as a Windows service. This matters for the same reason the systemd note
  does and has not been checked.
