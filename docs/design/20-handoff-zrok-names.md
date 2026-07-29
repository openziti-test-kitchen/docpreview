# Handoff: the zrok name leak, and the two bugs found next to it

Written mid-change, deliberately, because the research that unblocked this arrived after the implementation had
started and it changes the approach. Read this before touching `internal/expose/zrok.go`.

The evidence for every claim here is in [19-zrok-namespacing.md](19-zrok-namespacing.md), which was read out of
`github.com/openziti/zrok/v2@v2.0.4` in the module cache rather than from documentation.

## Where the code actually is

**Landed and tested.** Per-build shares work end to end: `expose.Spec.BuildID` and `Spec.Key()`, all four exposers
keyed on the publication, `publishBuildShare` best-effort after the branch share, `d.liveBuilds`, teardown closing
a preview's build shares, the reap keep-set listing every build with a recorded URL, `builds.name`/`url` with the
project's first schema migration, and `restoreBuildShares` republishing them at startup.

**Half-written, compiles, untested, not wired to anything.** `Zrok.ReleaseName` and the `expose.NameReleaser`
interface. Nothing calls `ReleaseName` yet. That is the state to pick up from, and the section below says why it
may be the wrong shape.

## The leak, and a better fix than the one being written

Teardown deletes a share. The *name* is a separate object that survives it, which is deliberate — it is what keeps
a preview's URL stable across rebuilds and restarts — and it is also the object an account's quota counts. Nothing
released it, so docpreview leaked one name per branch, and one per commit once builds got their own shares.
Confirmed by merging a real pull request: the share 404s, the name remains.

`ReleaseName` as written calls `DELETE /share/name`. That works. But the research found a cleaner mechanism:

`controller/unshare.go` **already deletes a name automatically** — but only where the name's `reserved` flag is
false. `createShareName` hardcodes `Reserved: true`, and that single line is the whole leak. `PATCH /share/name`
(`UpdateShareName`, body `{name, namespaceToken, reserved}`) flips the flag, has no live-share conflict check, and
works on a name a share is still bound to.

So the better teardown is: de-reserve the name, then delete the share, and let the controller reap the name. Two
reasons it beats an explicit delete:

- **It self-heals.** A crash between the two steps leaves a non-reserved name on a share that `Reap` will delete
  at the next startup, and the name goes with it. An explicit delete that never runs leaks silently forever.
- **No ordering hazard.** `DELETE /share/name` returns 409 while a share is bound, so it must come strictly after
  the share is gone. `PATCH` does not care.

`ReleaseName` is still worth keeping for a reaper that recovers names already leaked — there are some, from every
teardown so far.

## The hazard that gates all of it

`close(entry)` in `zrok.go` serves two callers that need opposite behaviour:

| Caller | Name must |
|---|---|
| a rebuild replacing a preview's share | **survive** — it is the reviewer's stable URL |
| teardown, the pull request is gone | **be released** |

Split that before wiring anything. Releasing on the rebuild path gives every rebuilt preview a new URL, silently,
while the pull request comment goes on advertising the old one — worse than the leak, and harder to notice.

## Two live bugs found beside it

**1. `isNameAlreadyExists` treats a quota rejection as success.** Five distinct situations return 409 from
`createShareName`. One is "name already exists", which is the success case it is looking for. Another is `"names
limit reached; cannot reserve additional names"` — and both arrive as `*share.CreateShareNameConflict`. So an
account at its name limit reports a registered name, and the `CreateShare` that follows fails for a reason that
does not mention quotas. Three of the five carry a payload and two do not, so payload presence is the only
discriminator available.

This matters more now than it did: one name per commit reaches a limit far sooner than one per branch.

**2. Nothing can read its own limit.** All six countable dimensions default to `Unlimited` with `Enforcing: false`,
so there is no number in the source to quote, and `adminListAppliedLimitClasses` is admin-only. The hosted free
tier publishes 5 GB/day, 25 environments, 50 share backends, 50 private frontends — and no reserved-name figure
anywhere. It has to be measured empirically; 19-zrok-namespacing.md describes the probe.

## The namespacing wish is dead

`85912e2.docpreview.share.zrok.io` requires a namespace *named* `docpreview.share.zrok.io`. That is legal, and
`createNamespace` requires `principal.Admin`, as does the namespace-frontend mapping — plus wildcard DNS and a
wildcard certificate. Not reachable from a hosted account.

The hostname comes from exactly one function, `util.NameInNamespace(name, ns) = name + "." + namespace`, and the
client never sends the suffix. So nothing in `zrok.go` can influence anything past the first label, and reading
`Share.FrontendEndpoints` back rather than composing a URL stays the correct approach.

Keep the flat namespace and put structure in the first label via `name_template`. Reversing that later costs
nothing.

Name constraints, since they bound any template: `^[a-z0-9-]{3,63}$`, lowercase, no dots, profanity-screened,
unique per namespace among non-deleted rows. A name must exist before a share can bind it, which is why
`ensureName` exists at all. The zrok doc comment says 3-64 characters; the regex says 63 and the regex is right.

## Also outstanding, unrelated to names

- **The placeholder share.** A share that exists from enqueue, serving a spinner and polling its own origin, then
  swapping its handler to the built site. It removes the destructive republish as a side effect. Full design in
  `TODO.md`.
- **`DeletePreview` leaves `builds` rows behind.** Bounded — `PruneBuilds` ages them out at `keep_logs`, and they
  render inert because `markOpenable` finds no artifacts — but accidental rather than decided.
- **Startup is serial.** 55 seconds for three previews, ~14 seconds per zrok round trip, nothing reachable until
  it finishes. Thirty pull requests would be seven minutes.
- **The Bitbucket spike** was still running when this was written; it writes to
  [15-bitbucket.md](15-bitbucket.md) incrementally, so whatever is in that file is what it got to.
