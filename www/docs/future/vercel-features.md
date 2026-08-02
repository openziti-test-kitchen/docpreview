---
id: vercel-features
title: What else Vercel does
sidebar_position: 2
---

# What else Vercel does

:::info Research, not a feature

Nothing on this page is built. It is a survey of Vercel's preview surface as documented in July 2026, judged
against the system docpreview actually is, so that the next thing built is chosen rather than stumbled into.
Where an idea is already tracked, this page says so and links to it instead of proposing it again.

[How Vercel previews work](../background/vercel.md) covers the mechanism docpreview already copies — the Git
integration, the single edited comment, the lifecycle. This page is everything else.

:::

## The verdicts, first

| | Idea | Verdict |
|---|---|---|
| 1 | [Comments anchored to the rendered page](#1-comments-anchored-to-the-rendered-page) | Skip |
| 2 | [Deployment protection, decided per project](#2-deployment-protection-decided-per-project) | Build it now |
| 3 | [Bypass tokens and sharable links](#3-bypass-tokens-and-sharable-links) | Skip |
| 4 | [Promote or roll back a build onto the branch URL](#4-promote-or-roll-back-a-build-onto-the-branch-url) | Build it when the switchable handler lands |
| 5 | [Immutable per-commit URLs](#5-immutable-per-commit-urls) | Shipped — one follow-up |
| 6 | [A framework build cache, not just a package cache](#6-a-framework-build-cache-not-just-a-package-cache) | Build it now |
| 7 | [Several documentation sites in one repository](#7-several-documentation-sites-in-one-repository) | Build it when a second site appears |
| 8 | [Checks that grade the build](#8-checks-that-grade-the-build) | Build the link checker; skip the scores |
| 9 | [What changed on the site](#9-what-changed-on-the-site) | Build it when branch previews land |
| 10 | [Log drains](#10-log-drains) | Skip |
| 11 | [Retention, and what must never be deleted](#11-retention-and-what-must-never-be-deleted) | Build the exception list |
| 12 | [Skew protection, ISR, edge functions](#12-skew-protection-isr-edge-functions) | Skip, by construction |

Three of those are worth starting: **9**, then **4**, then **2**. The reasoning is at the [end](#what-to-do-first).

## 1. Comments anchored to the rendered page

**What Vercel does.** The Vercel Toolbar is injected into every preview deployment, and Comments is a feature of
it: a reviewer clicks anywhere on the page or highlights text, and that opens a discussion thread pinned to that
spot, with email notification to the pull request owner and optional mirroring into a Slack thread. It is on by
default for every preview on every plan at no charge, and the one hard requirement is that **everyone who
comments has a Vercel account** ([Comments](https://vercel.com/docs/comments),
[Vercel Toolbar](https://vercel.com/docs/vercel-toolbar)).

**What it would mean here.** Injecting script into the served output, which means `internal/preview/` stops being
a file server and starts rewriting HTML. Threads become tables in the sqlite store, a write path appears on the
daemon that accepts input from the public internet — every exposer except `ziti` publishes to it — and
`internal/scm/`'s comment upsert either grows a second comment kind or these threads never reach the pull request
at all.

**What it costs.** An identity model for reviewers, which docpreview does not have in any form. The zrok exposer
can put an OAuth gate in front of a share, and the ziti exposer knows the dialing identity is *some* holder of the
`docpreview-reader` attribute — neither yields a person to attribute a comment to. Vercel's requirement that
commenters hold a Vercel account is not incidental; it is the cheapest way out of exactly this problem, and
docpreview has no equivalent account to require. Building this means building authentication first, and the
dashboard [does not have any either](../reference/security.md).

**Verdict: skip it.** The pull request already has a comment thread, anchored to the diff, with the reviewers
already authenticated by the platform. Vercel needs its own because it is not the code host. docpreview is not the
code host either, and that is an argument for sending people to the one that is.

## 2. Deployment protection, decided per project

**What Vercel does.** Deployment Protection is configured per project, as a **method** crossed with a **scope**:
Vercel Authentication on all plans, Passport (your own IdP) and Trusted IPs on Enterprise, and Password Protection
on Enterprise or as a $150/month Pro add-on; scoped to everything except production domains, or to everything
([Deployment Protection](https://vercel.com/docs/deployment-protection)). A generated preview URL is public by
default until one of those is turned on
([Generated URLs](https://vercel.com/docs/deployments/generated-urls)).

**What it would mean here.** Most of it exists and is in the wrong place. `internal/expose/zrok.go` already sends
`PermissionMode`, `AccessGrants`, `OauthProvider` and `OauthEmailAddressPatterns` on every share, driven by
`exposer.zrok2` in the server config — so the gate is a property of the **daemon**, not of the repository whose
documentation is behind it. One instance serving a public project and an embargoed one has to pick a single
posture for both. Moving those four fields onto `expose.Spec`, sourced from the project row, is the whole feature:
the projects page already holds per-project build settings and per-project secrets
([Projects](../reference/projects.md)), and the plumbing from a project row into a build already exists.

**What it costs.** Little, and the cost is honesty rather than code. The other three exposers answer differently
and the page has to say so: `ziti` is already the strongest form of this and needs no setting, `frontdoor` puts an
IdP and a WAF in front but through its own tenant configuration rather than through the share request, and `local`
is unreachable from anywhere else by construction. A per-project field that silently does nothing under two of
four exposers is worse than no field, so this needs `Exposer` to report which gates it can apply — a small
capability interface next to the existing optional `Adopter` and `NameReleaser`.

**Verdict: build it now.** The reason docpreview exists is documentation that cannot go to a SaaS
([What this is](../intro.md)), and "the whole daemon is either public or not" is the wrong granularity for that
claim. This is the smallest change that makes the stated purpose true.

## 3. Bypass tokens and sharable links

**What Vercel does.** Two escape hatches from the gate above. Protection Bypass for Automation is a per-project
secret sent as the `x-vercel-protection-bypass` header or query parameter, optionally with
`x-vercel-set-bypass-cookie: true` so a browser session inherits it; the current secret is injected into the build
as `VERCEL_AUTOMATION_BYPASS_SECRET`, and rotating it invalidates deployments built before the rotation
([Protection Bypass for Automation](https://vercel.com/docs/deployment-protection/methods-to-bypass-deployment-protection/protection-bypass-automation)).
Sharable links are the human version: with protection on, "Anyone with the link" mints a URL that carries the
bypass, for a reviewer with no account
([Sharing a Preview Deployment](https://vercel.com/docs/deployments/sharing-deployments)).

**What it would mean here.** A signed token checked in `internal/preview/`'s handler rather than at the exposer,
because that is the only layer all four exposers share — the exposer hands over an `http.Handler` and knows
nothing about what it serves ([Architecture](../architecture.md#exposer)). The daemon would mint per-project
secrets into the existing vault under the `project/<platform>/<owner>/<repo>/` prefix that already holds
per-project credentials.

**What it costs.** Not much to build, and it buys nothing yet. Vercel needs a bypass because its automation lives
outside the platform and can only reach a deployment over HTTP. Everything docpreview would point at a preview is
*inside the same process*: a link checker reads the artifact directory off disk, the machine-readable preview
status is a route on the daemon, and both are already designed that way in
[`TODO.md`](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md). A bypass header solves a problem created
by being a hosted service.

**Verdict: skip it, and revisit only alongside idea 2.** The half that would matter is the human half — a
reviewer with no zrok account and no enrolled identity who still needs to read one preview. That is worth having
the moment protection becomes per-project and somebody is locked out by it, and not before.

## 4. Promote or roll back a build onto the branch URL

**What Vercel does.** Instant Rollback repoints a project's domains at a deployment that previously held them.
It is a re-alias and not a rebuild, so it completes immediately, it carries the old deployment's environment and
cron state with it, and it switches **off** auto-assignment of production domains until explicitly undone
([Instant Rollback](https://vercel.com/docs/instant-rollback)). `vercel promote` is the deliberately different
operation: it triggers a complete rebuild with production environment variables rather than re-pointing anything
([Promoting a preview deployment](https://vercel.com/docs/deployments/promote-preview-to-production)).

**What it would mean here.** Almost nothing new, which is what makes it interesting. Every ingredient is already
on disk: artifacts live at `artifacts/<preview>/<build>`, `builds.name` and `builds.url` record each build's own
share, and the dashboard's log pane already lets an operator select an older build and open it. The missing verb
is "make **this** build the one the branch URL serves" — which under the
[switchable placeholder handler](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md) is one atomic pointer
swap in the branch publication, with no exposer call at all. Without the switchable handler it is a republish of
the branch share against a different artifact directory, which is more expensive and hits the destructive-publish
rule in `internal/expose/`.

**What it costs.** Two decisions, not two implementations. First, what the pull request comment says: the comment
is a rendered snapshot keyed off the newest build, so a branch URL deliberately serving an older commit makes the
comment lie unless `scm.Report` carries the served commit separately from the built one. Second, what the next
push does — Vercel's answer is to stop auto-assigning until a human undoes it, and copying that means a pinned
flag on the preview row that `Daemon.build`'s commit phase has to respect, or a pin that silently evaporates on
the next webhook.

**Verdict: build it when the switchable handler lands.** It is the natural payoff of per-build shares and it
answers the question those raised — an operator can already *see* five builds and can only *serve* the newest.
Doing it before the switch means writing the expensive version of a cheap feature.

## 5. Immutable per-commit URLs

**What Vercel does.** Two generated URLs per branch, and the distinction is the point.
`<project>-<hash>-<scope>.vercel.app` is pinned to one commit forever; `<project>-git-<branch>-<scope>.vercel.app`
always serves the branch's newest build. The pull request's **View deployment** button goes to the commit URL and
the comment's **Visit Preview** goes to the branch URL. Anything longer than 63 characters before the suffix is
truncated, and Pro and Enterprise can replace `vercel.app` with a custom domain as a Preview Deployment Suffix
([Generated URLs](https://vercel.com/docs/deployments/generated-urls)).

**What it would mean here.** It is shipped. A preview has a branch share following its newest successful build
plus one share per commit built, keyed by `expose.Spec.Key()`, and the dashboard carries the two links separately
— the row's Open for the branch, `Open build ↗` in the log pane for whichever build is selected. Written up in
[Exposers](../exposers.md) and tracked in
[`TODO.md`](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md).

**What it costs — and what Vercel does differently.** For Vercel a generated URL is a free consequence of having
uploaded the deployment. Here **each name is a quota-bearing object on the zrok account**, counted per account
across all namespaces, with a ceiling nobody has measured and no API a non-admin account can read it from. That
asymmetry is the whole difference, and it is why the open work under this heading is accounting rather than
features: prune the names leaked before teardown released them, and probe the actual ceiling.

One cheaper shape is available and unused. zrok's `ShareRequest.NameSelections` is a slice the controller loops
over, and the schema permits many names bound to one share — so a single share could answer to both the branch
name and the commit name, halving the share count for the same two URLs. It has never been sent with more than
one entry from here, which is the thing to verify before relying on it.

Vercel's Preview Deployment Suffix is the same wish this project already has and cannot buy: the
`<sha7>.docpreview.shares.zrok.io` shape needs a delegated namespace, which on hosted zrok.io is admin-only and a
commercial conversation rather than an API call. Vercel gates it behind a plan; zrok gates it behind a sales call.
Both are the same answer, which is worth knowing before anyone spends time on it.

**Verdict: shipped.** The follow-up is two names on one share, once there is a live account to try it against.

## 6. A framework build cache, not just a package cache

**What Vercel does.** At the start of every build the previous build's cache is restored before the install
command runs, covering `node_modules/**` plus a set of directories chosen by the project's framework preset. The
cache key is account, project, framework preset, root directory, Node version, package manager **and git branch**,
so a new branch starts from the last production deployment's cache and then keeps its own. The cap is 1 GB per
key, retained one month, and a failed build does not modify it
([Troubleshooting Build Errors](https://vercel.com/docs/deployments/troubleshoot-a-build)).

**What it would mean here.** `internal/pipeline/` already does the hard half. `cacheMounts` gives each preview a
named docker volume per package manager, keyed `docpreview-cache-<previewID>-<manager>`, and that survives between
pushes to the same branch — the same lifetime Vercel's branch-scoped key gives. What is not cached is the
framework's own work: `node_modules` is mounted as an **anonymous** volume removed with the container, so
Docusaurus's `.docusaurus` directory and the webpack persistent cache under `node_modules/.cache` are thrown away
on every build. Keeping them is another named mount alongside the three that exist.

There is a chance to be better than the thing being imitated here rather than merely equal to it. Vercel's cached
paths are chosen by framework preset, and Docusaurus is reported not to be among the presets whose cache
directories are preserved — so on the one framework this project exists to build, Vercel discards the same cache
docpreview discards. Treat that as unverified: it comes from a Docusaurus issue thread rather than from Vercel's
documentation, whose per-framework table is rendered client-side and could not be read.

**What it costs.** Correctness, in the one direction that is expensive to debug. A stale package cache produces a
slow build; a stale framework cache can produce a *wrong site* — the failure mode is a preview that renders the
previous commit's content and looks entirely healthy, which is precisely the class of bug the base-URL check
exists to catch loudly rather than quietly. So this needs what Vercel has and docpreview does not: a key that
includes everything a build's output depends on, and a documented way to bypass it. Vercel offers three
(`VERCEL_FORCE_NO_BUILD_CACHE=1`, `vercel --force`, and an unchecked box on redeploy); here it would be a checkbox
on the Rebuild button. It also needs the byte cap this project has
[not written yet](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md), because a persistent volume per
preview with no ceiling is the disk problem again.

**Verdict: build it now, with the bypass in the same commit.** The measurable win is real — prerendering is
already the phase that visibly stalls — and it is one mount plus a cache key. Shipping it without the bypass is
what turns it into a support conversation.

## 7. Several documentation sites in one repository

**What Vercel does.** A monorepo is not one project with several sites; it is several **Projects** pointed at one
repository, each with its own Root Directory, and the pull request comment lists every one with its own URL. Every
commit would deploy all of them, so Vercel derives the workspace dependency graph from `package.json` and
`pnpm-workspace.yaml` and skips projects the commit did not affect — GitHub only, and only for repositories that
follow the workspace conventions. Repositories that do not qualify fall back to an Ignored Build Step, a shell
command whose sense is inverted from every instinct: **exit 1 continues the build, exit 0 cancels it**. A
cancellation that way still consumes a concurrent build slot and counts against the deployment quota, which the
built-in skip does not — the stated reason to prefer the built-in
([Using Monorepos](https://vercel.com/docs/monorepos),
[Project settings](https://vercel.com/docs/project-configuration/project-settings)).

**What it would mean here.** This is already filed as a gap in
[`TODO.md`](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md) and in
[Architecture](../architecture.md) as a deliberate limit: one `.docpreview.yml` builds one site. The shape Vercel
uses is the right one — a project per site rather than a list inside one config — and the projects page is already
where a project's build settings live.

**What it costs.** Two primary keys that assume one site per repository, and neither is cosmetic.
`model.PullRequest.PreviewID()` is `sha256("platform|owner|repo|number")`, so two sites built from one pull
request are one preview: same database row, same artifact directory, same publication key, same comment marker.
And the projects table keys on platform, owner and repo, so the second project row for a repository collides with
the first. Adding a site component to both is mechanical but it touches the identity every durable thing is keyed
by — the row, the artifacts, the log directory, the cache volume, the in-flight build map — which is the one part
of this codebase where a partial change is worse than none.

The skip half is comparatively cheap and already half-built: `ChangedFiles` plus the path check decides whether to
build at all today. Extending it to "which of these sites did the change touch" is a per-site path prefix, not a
dependency graph, because a documentation site is not a package with dependents.

**Verdict: build it when a repository with two documentation sites actually shows up.** The identity change is
worth doing once, deliberately, against a real second site — not speculatively against an imagined one. Note it
also does not compose with idea 4 for free: pinning a build implies pinning *which* site's build.

## 8. Checks that grade the build

**What Vercel does.** The Checks API runs assertions after a successful deployment and **holds the aliases until
every check has reported a conclusion** — `deployment.created` invites integrations to register checks,
`deployment.ready` starts them, and only when all of them conclude do the domains point at the new deployment.
Vercel groups them into four flows: Core (does the page return 200), End-to-end (broken pages, missing images and
assets), Optimization (bundle and asset size), and Performance, which collects Core Web Vitals and **compares them
against the previous deployment** — `output.metrics` carries `LCP`, `CLS`, `FCP` and `TBT` each with a
`previousValue`, and the dashboard renders the delta ([Working with Checks](https://vercel.com/docs/checks),
[Creating checks](https://vercel.com/docs/checks/creating-checks),
[Speed Insights](https://vercel.com/docs/speed-insights)). The checks themselves are marketplace integrations
rather than built-in, and only an OAuth2 integration may register one.

**Where the gate actually sits is the detail worth stealing.** A blocking check does not hold the build; it holds
**domain assignment**. While checks run, the per-commit deployment URL resolves and the branch URL and custom
domains do not — so the artifact is always inspectable, and only the shared address waits. docpreview already has
that exact split: a build share pinned to the commit, and a branch share the pull request comment advertises. The
mechanism to publish the first and withhold the second until a check concludes is already in the shape of the
code, which is the opposite of what "gate the publish" sounds like it would cost.

**What it would mean here.** The reporting channel exists: `internal/scm/github` already upserts a check run for
the head commit alongside the comment, so a graded result has somewhere structured to land, and on GitHub it can
be made a required status. The gate does not exist — "publish the branch share only after the checks conclude"
means the commit phase in `internal/daemon/` grows a step, and that block is deliberately one irreversible
sequence under a per-preview lock.

**What it costs.** Split it by what needs a browser.

The deterministic checks need nothing docpreview lacks. A post-build link checker walking the artifact tree is
already filed in [`TODO.md`](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md), modelled on
`pipeline.Detector` — bounded by a timeout and unable to fail the build — and it covers Vercel's Core and
End-to-end flows for a static site, including the one failure docs actually ship: an internal link or heading
anchor that no longer resolves. Output size and file count for the Optimization flow is a directory walk.

The toolbar's **Accessibility Audit** — a WCAG 2.0 A/AA pass over the rendered page
([Vercel Toolbar](https://vercel.com/docs/vercel-toolbar)) — has a useful subset that needs no browser either.
Missing `alt` text, a missing `lang` attribute, skipped heading levels and a link whose only text is "here" are all
findable by parsing HTML that is already on disk, and all four are things documentation actually gets wrong. The
rest of an audit needs computed styles and a real DOM, and does not survive the cost argument below.

The Performance flow is where the cost is real, and it is not the browser. Note first that Vercel has two different
things here and neither is Lighthouse: the Checks metrics are measured by whichever integration registered the
check, and Speed Insights is real-user telemetry from actual visitors. Both are only meaningful as a comparison
against the previous deployment on comparable infrastructure. Here the previous build is on the same disk, but the
*serving path* is a zrok share over an OpenZiti overlay to a laptop, so a Largest Contentful Paint measurement
describes the tunnel and the machine's current load at least as much as it describes the commit — and a
documentation preview has no real users to collect telemetry from in the first place. Adding headless Chromium to
the build image to produce a number nobody can act on is a large dependency bought for noise.

**Verdict: build the link checker now — it is already filed and it is the half that needs no model and no
browser. Skip the scores.** Publishing behind a gate is a separate decision and should not ride along: the comment
saying "built, and these six links are broken" is more useful to a reviewer than no preview at all.

## 9. What changed on the site

**What Vercel does.** Not this, and the negative result is worth recording. Vercel ships no native screenshot or
visual diff between deployments; nothing in its documentation offers one. What it offers instead is the Checks API
above plus the marketplace's testing category, so visual regression is Percy, Chromatic or Argos running as a
check — and the closest first-party thing is a Chrome extension for attaching screenshots to a
[comment](https://vercel.com/docs/comments) by hand.

**What it would mean here.** [`TODO.md`](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md) records
"Preview diffing. Vercel shows what changed visually between deployments. Not attempted." The premise is worth
correcting on two counts: Vercel does not ship it, and *visual* is the expensive answer to the question a
documentation reviewer is actually asking, which is **which pages changed, and how**.

The cheap answer is available and needs no browser at all. Two builds are two directory trees of rendered HTML on
one disk. Extract the text of each route, diff the route sets and the per-route text, and the pull request comment
gains the table that makes a docs review fast: pages added, pages removed, pages whose rendered content changed,
and a link straight to each one on this build's own pinned share. That reaches things a source diff cannot — a
sidebar reorder, a broken MDX component that silently renders nothing, a changed partial that alters forty pages
from a one-line commit.

**What it costs.** A baseline, which is the one thing missing. Diffing against the branch's previous build answers
"what did my last push change", which is not the review question; diffing against the default branch answers
"what does this pull request change", and that needs a default-branch build to exist. That is exactly the
**branch previews** work in flight at the top of the backlog — a permanent preview of the default branch, kept
current, with its artifacts on disk. Everything else is HTML-to-text extraction and a route-set
comparison, both deterministic and unit-testable with no network. The comment grows a section, so
`scm.RenderComment` has to keep it bounded — forty changed pages is a summary and a count, not forty rows in a
public comment.

Pixel diffing, if it is ever wanted, is the same feature with a screenshot step, and it is a different order of
cost: headless Chromium in the build image, one image per route per build against the disk cap this project has not
set yet, and antialiasing noise to threshold. Text first. It is most of the value at a fraction of the cost, and it makes the pixel version an
increment rather than a project.

**Verdict: build it when branch previews land**, and treat it as the reason to finish them. This is the largest
reviewer-facing win on this page and the only one that needs no new dependency, no identity model and no second
exposer feature.

## 10. Log drains

**What Vercel does.** Drains forward observability data — runtime, build and static logs, OpenTelemetry traces,
Speed Insights, analytics, audit logs — to a custom HTTP endpoint or a native integration, on Pro and Enterprise,
billed by volume ([Working with Drains](https://vercel.com/docs/drains)).

**What it would mean here.** A sink configuration and a fan-out. `buildlog.Writer` already multiplexes a build's
output to disk and to live subscribers, so a drain is a third subscriber; the daemon's own log already tees
through an `io.MultiWriter` to a file.

**What it costs.** Little, and it answers a question nobody here is asking. Vercel needs drains because the logs
are on their infrastructure and the operator cannot reach the disk — and because they expire fast: runtime logs are
retained one hour on Hobby, one day on Pro and three days on Enterprise
([Runtime Logs](https://vercel.com/docs/logs/runtime)), so a drain is how anything is kept at all. docpreview's
operator **is** on the machine, with `log_file` and the build logs in a directory under a retention window they
chose. The genuinely useful part of this family is already filed and
is not a drain: [log search](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md), because the viewer tails
and downloads, so finding a line in a 5000-line build means downloading the whole thing.

**Verdict: skip it.** Build log search instead. Revisit only if docpreview ever runs somewhere its operator does
not have a shell.

## 11. Retention, and what must never be deleted

**What Vercel does.** Retention is configured per project and per deployment state — canceled, errored, preview,
production. An expired deployment answers **410** and stays restorable for 30 days before its resources are
removed. The interesting part is the exception list: a deployment is kept regardless of policy if it is among the
last 10 in the project, the last 20 Ready production or last 20 Ready non-production deployments, if it holds a
production alias, or if it is **the latest preview deployment of a branch whose pull request is still open**
([Deployment Retention](https://vercel.com/docs/deployment-retention)).

**What it would mean here.** `preview.keep_builds` (default 10) and `build.keep_logs` are the count-based half,
and they already refuse to prune the build that just published — a clock stepping backwards would otherwise delete
what is being served. What is absent is everything the exception list encodes. `PruneBuilds` and the artifact prune
count rows; they do not know that one of those builds is the one the branch share currently serves, or that a
build's log is the only evidence of a failure somebody is reading right now.

The other half worth taking is the report, not the policy. Setting `VERCEL_BUILD_SYSTEM_REPORT=1` makes every build
print a breakdown of source, `node_modules` and output sizes and flag files over 100 MB
([Build features](https://vercel.com/docs/builds/build-features)). That is three lines at the end of
`Builder.Build` here, it is the number the byte cap needs in order to be set to anything defensible, and it answers
the dashboard gap already filed as "a dashboard that does not say how much disk the previews are using is a
dashboard nobody can use to decide what to delete".

**What it costs.** Nearly nothing, and it is a bug fix wearing a feature's clothes. The keep-set logic already
exists in the right shape — `Exposer.Reap` takes exactly this kind of set, and the rule that an incompletely
assembled keep-set **skips the sweep** rather than under-deleting is the precedent to copy, because an incomplete
set does not delete too little, it deletes live shares. The byte and total caps are the harder, already-filed
half; they need a documented eviction order, and Vercel's answer is the model: cap by policy, then exempt by role.

Two things here are worth not copying. The 410-plus-recovery-window is a hosted service's answer to accidental
deletion, and a self-hosted tool with the artifacts on a local disk and a backup story has no use for a tombstone.
And Vercel's unlimited-retention default is only affordable to somebody billing for storage; this project's
problem is the opposite, and its
[open item](https://github.com/openziti-test-kitchen/docpreview/blob/main/TODO.md) is a ceiling, not a floor.

**Verdict: build the exception list now, alongside the byte caps it belongs with.** Do not build the recovery
window.

## 12. Skew protection, ISR, edge functions

**What Vercel does.** Skew Protection pins a client's framework-managed requests to the deployment that served
its first page, via a `?dpl=` parameter, an `x-deployment-id` header or a `__vdpl` cookie, keeping that deployment
reachable for a configurable maximum age that defaults to one day
([Skew Protection](https://vercel.com/docs/skew-protection)). Incremental Static Regeneration, edge functions and
routing middleware are the rest of the platform: request-time behaviour, in Vercel's runtime, in Vercel's regions.

**What it would mean here.** Nothing, and the reason is structural rather than a matter of effort. Skew
protection exists because a client holds JavaScript that talks to a server contract; a documentation site is HTML,
CSS and assets whose only "API" is more files, so the failure it prevents cannot occur. And what it does
mechanically — keep older deployments individually addressable — docpreview already has as per-build shares, for a
different reason.

ISR, edge functions and middleware are refused by design and the refusal is
[documented as a feature](../background/jamstack.md): docpreview builds static output and serves a directory
through `http.FileServer`. Anything needing a server at request time is the wrong tool, and the
[intro](../intro.md) says so before anyone installs it. The one request-time behaviour a static site does need —
serving the site's own `404.html` instead of Go's plain-text one — is already in `internal/preview/`.

**Verdict: skip, and keep skipping.** This entry exists so the question is answered once rather than reopened.

## What to do first

**9, then 4, then 2.**

**[What changed on the site](#9-what-changed-on-the-site)**, because it is the only idea here that makes a
reviewer's job faster rather than an operator's, it needs no dependency docpreview does not already have, and its
one prerequisite — a default-branch preview to diff against — is already the top item in flight. It also converts
a vague backlog entry into something buildable, and corrects the premise underneath it.

**[Promote or roll back a build](#4-promote-or-roll-back-a-build-onto-the-branch-url)**, because per-build shares
already put every ingredient on disk and left the obvious verb missing: an operator can see five builds and serve
only the newest. After the switchable handler it is a pointer swap, so the work is the two decisions about what
the comment says and what the next push does — and those are worth making deliberately rather than discovering.

**[Protection per project](#2-deployment-protection-decided-per-project)**, because the fields already exist on
the zrok share request and are attached to the wrong object. Moving four settings from the daemon onto the project
row is the smallest change that makes the reason this tool exists actually true, and the honest part — saying which
exposers can enforce which gate — is the part worth spending the time on.

Then [the build cache](#6-a-framework-build-cache-not-just-a-package-cache), with its bypass, and
[the retention exception list](#11-retention-and-what-must-never-be-deleted) with the byte caps.

## Sources

Fetched from Vercel's documentation on 31 July 2026. Where a claim could not be confirmed it is marked as such in
the section above.

- [Comments Overview](https://vercel.com/docs/comments),
  [How comments work](https://vercel.com/docs/comments/how-comments-work) and
  [Vercel Toolbar](https://vercel.com/docs/vercel-toolbar)
- [Deployment Protection](https://vercel.com/docs/deployment-protection)
- [Protection Bypass for Automation](https://vercel.com/docs/deployment-protection/methods-to-bypass-deployment-protection/protection-bypass-automation)
  and [Sharable Links](https://vercel.com/docs/deployment-protection/methods-to-bypass-deployment-protection/sharable-links)
- [Sharing a Preview Deployment](https://vercel.com/docs/deployments/sharing-deployments)
- [Instant Rollback](https://vercel.com/docs/instant-rollback),
  [Promoting a deployment](https://vercel.com/docs/deployments/promoting-a-deployment) and
  [Promoting a preview deployment](https://vercel.com/docs/deployments/promote-preview-to-production)
- [Accessing Deployments through Generated URLs](https://vercel.com/docs/deployments/generated-urls)
- [Troubleshooting Build Errors](https://vercel.com/docs/deployments/troubleshoot-a-build) — the build cache
  section is here rather than under a heading of its own — and
  [Build features](https://vercel.com/docs/builds/build-features)
- [Using Monorepos](https://vercel.com/docs/monorepos) and
  [Project settings](https://vercel.com/docs/project-configuration/project-settings)
- [Working with Checks](https://vercel.com/docs/checks),
  [Creating checks](https://vercel.com/docs/checks/creating-checks) and
  [Speed Insights](https://vercel.com/docs/speed-insights)
- [Working with Drains](https://vercel.com/docs/drains) and [Runtime Logs](https://vercel.com/docs/logs/runtime)
- [Deployment Retention](https://vercel.com/docs/deployment-retention)
- [Skew Protection](https://vercel.com/docs/skew-protection),
  [Incremental Static Regeneration](https://vercel.com/docs/incremental-static-regeneration) and
  [Vercel Global Config](https://vercel.com/docs/global-config)

Two renames matter when reading older material about Vercel: **Log Drains** is now Drains, and **Edge Config** is
now Vercel Global Config.

### What could not be confirmed

**Vercel ships no visual diffing.** Its documentation describes no native screenshot or visual comparison between
deployments. The route it offers is the Checks API plus a marketplace integration, and the Marketplace's testing
category currently lists Endform, Autonoma and Meticulous AI — not Percy, Chromatic or Argos, which integrate with
Vercel previews as ordinary CI tools rather than as Vercel features
([Marketplace: testing](https://vercel.com/marketplace/category/testing)). Treat any claim that Vercel ships
preview diffing as false, and idea 9 is not a copy of anything.

**The per-framework build-cache path list.** Vercel's table of which directories each framework preset caches is
rendered client-side and could not be read. `.next/cache` for Next.js and `.cache` for Gatsby are confirmed
elsewhere; the claim that Docusaurus is not among them comes from a Docusaurus issue thread, not from Vercel.

**Comment drafts, and sharable-link expiry.** Neither is documented. What Comments documents is resolve,
follow/unfollow and notification levels; what Sharable Links documents is creation and manual revocation, with no
time-to-live. Do not design against either.

## Related

- [How Vercel previews work](../background/vercel.md) — the mechanism docpreview already copies
- [Tunneler-only previews](./ziti-native-previews.md) — the other research page, and the strongest existing
  answer to idea 2
- [Exposers](../exposers.md) — why a name is a quota-bearing object, which idea 5 answers to
- [Jamstack](../background/jamstack.md) — why idea 12 is refused rather than deferred
