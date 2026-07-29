# zrok names: deletion, limits, and subdomain shape

Research note. Three questions, in the order they block work. The first blocks implementation of per-build shares
today; the third is a wish, not a blocker.

Everything here was read out of `github.com/openziti/zrok/v2@v2.0.4` in the module cache, or fetched from zrok's
documentation. Paths are cited inline and are relative to that module root; URLs are listed at the end.

Short version:

| Question | Answer |
|---|---|
| Can a reserved name be deleted? | Yes — `DELETE /share/name`, after the share. Or de-reserve it and let `unshare` do it |
| Per-account limits? | Six countable dimensions exist, all `Unlimited` and unenforced by default. No rate limit anywhere |
| Several shares under one owned subdomain? | Only by creating a namespace, which is admin-only plus wildcard DNS and TLS |

## 1. Can a reserved zrok name be deleted?

**Yes.** `DELETE /share/name`, operation id `deleteShareName`, reachable from the SDK we already use as
`client.Share.DeleteShareName`. The body is `{name, namespaceToken}` — the same two fields `ensureName` already
sends to create it — so plugging the leak is a mirror image of the call that opens it. The one rule that shapes
where the call goes in `zrok.go`: the controller refuses with **409** while any share is still bound to the name,
so a name can only be deleted *after* its share is gone. Delete the share, then delete the name, in that order.

### The call

| | |
|---|---|
| Operation | `deleteShareName` |
| Method and path | `DELETE /share/name` |
| Go SDK | `client.Share.DeleteShareName(params, auth)` where `client, _ := z.root.Client()` |
| Params ctor | `share.NewDeleteShareNameParamsWithContext(ctx)` |
| Body | `share.DeleteShareNameBody{Name: name, NamespaceToken: z.namespace}` |
| Success | `*share.DeleteShareNameOK` — 200, empty body |
| Auth | the same `X-TOKEN` header `z.auth()` already writes |

Unlike `sdk.CreateShare`, this operation **does** take a context, so a teardown can be bounded.

### What each failure means

The response codes come from `rest_client_zrok/share/delete_share_name_responses.go`; the reasons come from the
controller handler at `controller/deleteShareName.go`, which is the authority on which branch produces which code.

| Code | Generated type | Controller reason |
|---|---|---|
| 200 | `DeleteShareNameOK` | name deleted; stale mappings from already-deleted shares were cleaned up first |
| 401 | `DeleteShareNameUnauthorized` | this account does not own the name, or is not granted the namespace |
| 404 | `DeleteShareNameNotFound` | namespace token not found, **or** the name does not exist in it |
| 409 | `DeleteShareNameConflict` | a live share is still bound — carries a message naming the share token |
| 500 | `DeleteShareNameInternalServerError` | transaction or delete failed |

Two of these matter to the code that will call it. **404 is the idempotent success case**, exactly as
`CreateShareNameConflict` is for `ensureName` — a name that is already gone needs no deleting, and treating 404 as
an error would make every second teardown log a failure. Match on the type, not the message: like the create
conflict, the 404 arrives with an empty body.

**409 is a programming error, not a transient one.** The handler emits it from an explicit guard:

```go
// controller/deleteShareName.go
for _, mapping := range mappings {
	if !mapping.ShareDeleted {
		msg := rest_model_zrok.ErrorMessage("name '" + params.Body.Name + "' in namespace '" + ns.Token +
			"' is still attached to share '" + mapping.ShareToken + "'; unshare it before deleting the name")
		return share.NewDeleteShareNameConflict().WithPayload(msg)
	}
}
```

It does not clear on retry, because nothing about waiting deletes the share. A 409 means the caller ordered the
teardown wrong. The payload names the offending share token, so it is worth logging rather than swallowing — it is
the only error here that tells you which share to go delete.

Note also that the handler resolves a name by *namespace plus name* and then checks `an.AccountId`, so a name in a
namespace we do not own is a 401, and a name another account holds in a shared namespace is also a 401. Neither is
distinguishable from the other without reading the log on the controller. This is the same blindness
`isNameAlreadyExists` already documents in the other direction.

### The CLI equivalent

`zrok2 delete name <name>` — the mirror of the `zrok2 create name <name>` that `internal/expose/CLAUDE.md` already
records. It is not what docpreview should use: shelling out cannot report the 409 payload as a typed error, and the
SDK path is already wired.

### What it means for one-share-per-build

Per-build shares become viable. The leak is real today — teardown calls `sdk.DeleteShare` and nothing else, so
`internal/expose/zrok.go` accumulates one name per distinct `spec.Name` ever published — but it is a missing call,
not a missing capability. The fix has a shape:

1. `close(entry)` deletes the share (it already does), and only then deletes `entry.name`.
2. Treat `*share.DeleteShareNameNotFound` as success. Log a 409 with its payload, because it means the order broke.
3. **Do not delete the name on the supersede path.** `withdrawEntry` runs when a second push replaces a preview,
   and the whole reason the name outlives the share is that the reviewer's bookmark must survive a rebuild. A
   teardown that deletes the name on every rebuild churns the URL and defeats `name_template`.

That third point is the trap. There are two teardowns wearing one method: "this preview is finished" (delete the
name) and "this preview is being rebuilt" (keep it). `close` cannot tell them apart today, and the difference is
now load-bearing. Whatever plugs the leak has to split them, or the URL stability that `Publish`'s comment block
argues for is lost.

`Reap` has the same asymmetry. It deletes orphaned shares found on the controller but knows nothing about names,
and once a share is deleted the name it held is invisible to `ListShares` — so orphaned names cannot be found that
way at all. `ListNamesForNamespace` (`GET /share/names/{namespaceToken}`) and `ListAllNames`
(`GET /share/names`) are how a reaper would enumerate them.

## 2. Is there a per-account limit on names, shares, or a creation rate limit?

**Limits exist as a mechanism; the numbers are the operator's, not the software's.** zrok's controller has a
limit class with five counting dimensions, one of which is `uniqueNames`, and every one of them defaults to
`Unlimited` with enforcement *off*. So there is no number in the source to quote — the number that applies to a
hosted zrok.io account is whatever limit class the zrok.io operators applied to it, and that is data in their
database, not code in the module. **There is no rate limit of any kind.** Nothing in the controller counts
creations per unit time; the only temporal limit is bandwidth, measured over a period. What that means practically:
per-build shares cannot be throttled into failure, but they can walk into a count ceiling, and the ceiling is
invisible until it is hit.

### The dimensions that count

`controller/limits/config.go` and `controller/store/limitClass.go`:

| Dimension | Counts | Checked by |
|---|---|---|
| `environments` | enabled environments per account | `CanCreateEnvironment` |
| `shares` | live shares across all of an account's environments | `CanCreateShare` |
| `reservedShares` | names with `reserved = true` | `CanReserveName` |
| `uniqueNames` | names with `reserved = true` | `CanReserveName` |
| `shareFrontends` | frontends per share | share creation |
| bandwidth rx/tx/total | bytes over a period | every share access |

`DefaultConfig()` sets all five counts to `store.Unlimited` and `Enforcing: false`. Every check in
`controller/limits/agent.go` opens with `if a.cfg.Enforcing` and returns `true` otherwise, so on a default
self-hosted controller none of this is live.

Two of those rows are the same count. `CanReserveName` counts `Reserved` names once and compares that count against
both `uniqueNames` and `reservedShares`, so whichever is lower is the effective ceiling on reserved names. Both are
counted **per account, across all namespaces** — `FindNamesForAccount` filters by account id alone. Moving names
into a different namespace does not buy headroom.

The count is of *live* names. `DeleteName` is a soft delete (`update names set deleted = true`) and every query
filters `not deleted`, including the unique index `uk_names on names(namespace_id, name) where not deleted`. So
deleting a name both frees the quota slot and frees the name for re-creation. That is what makes the fix in section
1 actually a fix rather than a deferral.

Note which flag the *share* check gets. `controller/share.go:168` calls
`CanCreateShare(..., reserved: false, uniqueName: false, ...)`, so a share created through `POST /share` — which is
every share docpreview makes — is counted against `shares` only, never against `reservedShares`. The reserved
accounting lives entirely on the name. This is the mechanical reason the task's framing is right: **the name is the
quota-bearing object**, and a leaked name is a consumed slot with nothing to show for it.

### The failure this creates in docpreview today

`createShareName` returns **409 with the payload `"names limit reached; cannot reserve additional names"`** when
`CanReserveName` says no (`controller/createShareName.go:52-55`). That is the same HTTP status, and therefore the
same generated Go type `*share.CreateShareNameConflict`, that the handler returns for "name already exists".

`isNameAlreadyExists` in `internal/expose/zrok.go` matches on that type and returns success. So **hitting the name
limit is currently indistinguishable from the name already existing, and `ensureName` reports success for it.**
The publish then fails one call later inside `CreateShare` with an error that says nothing about limits, and the
operator gets "creating zrok share … failed" with no fix in it. The comment on `isNameAlreadyExists` already
concedes it cannot distinguish two cases; there are in fact four:

| Situation | Status | Payload |
|---|---|---|
| name already exists, this account owns it | 409 | empty |
| name already exists, another account owns it | 409 | empty |
| name limit reached | 409 | `names limit reached; cannot reserve additional names` |
| name still attached to a live share | 409 | `name '…' … is still attached to share '…'; unshare it before reusing` |
| name fails the DNS or profanity screen | 409 | `'…' is not a valid share name; failed profanity or DNS check` |

Three of the five carry a payload and two do not. **Payload presence is the discriminator**, and it is the only one
available. `ensureName` should treat an empty-payload 409 as success and a non-empty-payload 409 as an error that
propagates the payload verbatim — the payload is written to be shown to a human, and it names the fix, which is
what this codebase's error convention demands. Today all five are swallowed.

### What would have to be measured against a live account

None of the above says what a zrok.io account is actually allowed. To learn it:

- `zrok2 status` or the account detail endpoint reports the applied limit class if the API exposes it to a
  non-admin; `adminListAppliedLimitClasses` exists but is admin-only, so a hosted account cannot read its own
  ceiling that way.
- Failing that, the ceiling is discovered empirically: create reserved names in a loop until
  `createShareName` returns 409 with the limits payload, record the count, then delete them all. This is a
  destructive-ish experiment against a shared service and should be done on a throwaway account.
- The share ceiling is separate and needs its own probe, because `shares` and `uniqueNames` are independent.

Do not encode a number from this experiment as a constant. It is per-account operator policy and can change without
a zrok release.

## 3. Can several shares hang off one owned subdomain?

**Structurally yes, but not from docpreview and not on hosted zrok.io without NetFoundry doing it for you.** The
hostname is formed by exactly one function — `util.NameInNamespace(name, namespace) = name + "." + namespace` — so
the shape the owner wants, `85912e2.docpreview.share.zrok.io`, is a name `85912e2` in a namespace whose **name** is
`docpreview.share.zrok.io`. Namespace names are unconstrained multi-label strings (`varchar(255)`), so that
namespace is perfectly legal. What blocks it is who may create one: `createNamespace` refuses any principal without
`Admin`, and so does the namespace-to-frontend mapping that makes it resolve. On top of that, a new namespace needs
a wildcard DNS record and a wildcard TLS certificate for its zone. None of that is reachable from an account token,
which is all docpreview has. **Nothing in `internal/expose/zrok.go` can influence the hostname beyond choosing the
first label.**

### What `NamespaceToken` actually selects

`NameSelection.NamespaceToken` selects a row in the `namespaces` table by its `token` — an opaque-ish identifier
that on hosted zrok.io is the literal string `public` (it is the fallback default in `cmd/zrok2/deleteName.go:27`).
It is **not** the DNS suffix. The suffix is that row's `name` column, which the client never sends and cannot set.

The controller then composes the endpoint and hands it back:

```go
// controller/share.go, processNameSelections
endpoint = util.NameInNamespace(name.Name, ns.Name)   // "my-preview" + "." + "share.zrok.io"
```

and `buildFrontendEndpointsForShare` (`controller/util.go:134`) rebuilds the same string for every later read. That
is the value that lands in `Share.FrontendEndpoints`, which `Publish` reads. **So the hostname is entirely the
controller's to decide, and reading it back rather than composing it is the only correct approach** — the existing
comment in `zrok.go` about not guessing a DNS suffix is right, and remains right.

Two consequences of the composition being a single `+ "." +`:

- A name cannot contain a dot (see the constraints below), so a client cannot smuggle extra labels into the
  hostname by naming a share `a.b`. Nesting is a namespace decision, always.
- The namespace's `name` is the whole suffix. `docpreview.share.zrok.io` and `share.zrok.io` are unrelated
  namespaces as far as the controller is concerned; the nesting is a fact about DNS, not about zrok.

### Many shares under one suffix already works; many names per share also works

Two things get conflated in the question and they are different:

| Relationship | Supported | Enforced by |
|---|---|---|
| many names → one namespace | yes, this is what happens today | `uk_names on names(namespace_id, name)` |
| many names → one share | yes, `NameSelections` is a slice and the controller loops it | `FindNamesForShare` returns many |
| one name → many shares | **no** | `uk_share_name_mappings_name on share_name_mappings(name_id) where not deleted` |

So "several shares hanging off one owned subdomain" is already true in the weak sense: every docpreview share today
lives under `share.zrok.io`. The wish is for a *deeper* suffix that the owner controls. And "multiple names for one
share" — advertised as a v2 benefit — is real and usable: one share could answer to both a branch name and a commit
name, which is worth knowing if per-build URLs are wanted without per-build shares.

### What it would take to get `*.docpreview.share.zrok.io`

Every step is an administrator's, on the controller and in DNS:

1. `zrok2 admin create namespace --token <tok> --open docpreview.share.zrok.io` — admin-only
   (`controller/createNamespace.go:18`).
2. A dynamic frontend, and `zrok2 admin create namespace-frontend <namespaceToken> <frontendToken>` — admin-only.
3. Wildcard DNS `*.docpreview.share.zrok.io` pointing at the frontend, and a wildcard certificate for the same. The
   dynamic-proxy guide names both explicitly under its DNS and TLS troubleshooting.

On **hosted zrok.io** steps 1 and 2 are NetFoundry's to perform. The pricing page lists a "Custom Domain Frontend"
feature — "a public HTTPS frontend dedicated to your account" — without attaching it to the free tier, and directs
custom limits and dedicated infrastructure to a sales conversation. So the honest answer is: it is a commercial
arrangement, not an API call. On a **self-hosted** controller all three steps are ours, and the docs' own worked
example creates exactly this shape (`myshare.zrok.example.com`).

**What to do instead, today.** Keep the flat namespace and put the structure in the first label, which is the one
thing code controls. `name_template` already exists for this. `{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}` yields
`netfoundry-docpreview-my-branch.share.zrok.io` — ugly next to a real subdomain, but it is a prefix the operator
chooses, it collides across repositories no more than a real subdomain would, and it needs nothing from anybody.
Reversing this decision later costs nothing in the code: the same `name_template` output becomes the first label of
whatever namespace is configured, and `exposer.zrok2.namespace` already selects the namespace.

## Naming constraints

`util.IsValidNameInNamespace` in `util/names.go` is the whole of it, and it is enforced only on `createShareName` —
not on the ephemeral path, which uses the generated share token and is valid by construction.

| Constraint | Value |
|---|---|
| Pattern | `^[a-z0-9-]{3,63}$` |
| Length | 3 to 63 characters |
| Case | lowercase only; an uppercase letter is rejected, not folded |
| Allowed characters | `a-z`, `0-9`, `-` |
| Dots | **not allowed** — this is why nesting must come from the namespace |
| Leading/trailing hyphen | permitted by the regex, though not a legal DNS label |
| Uniqueness | per namespace, among non-deleted names (`uk_names`) |
| Profanity | screened by `github.com/TwiN/go-away` unless the controller sets `DisableNamespaceNameProfanityCheck` |
| Column limit | `varchar(255)`, so the regex is the binding limit, not the schema |

The doc comment says "3-64 characters" and the regex says `{3,63}`. **Trust the regex** — 63 is also the DNS label
limit, so the code is right and the comment is off by one.

**A name must be reserved before a share can bind it.** `processNameSelections` resolves a non-empty
`NameSelection.Name` with `FindNameByNamespaceAndName` and errors if it is absent; it never creates one. That
confirms the existing comment in `Publish` from the other side — the 409 docpreview observed was the controller
declining to invent the name. Leaving `Name` empty takes the other branch: the controller creates a name equal to
the generated share token with `reserved = false`, which is the ephemeral case and gives an unpredictable hostname.

Docpreview's `name_template` output is not validated against this regex anywhere. A template producing an uppercase
repository name, an underscore, or a two-character branch name yields a 409 whose payload says "failed profanity or
DNS check" — and that payload is currently discarded by `isNameAlreadyExists`, so the operator sees the downstream
`CreateShare` failure instead. Validating the rendered name locally, before the API call, would name the fix at the
point the operator can act on it.

## What it means for one-share-per-build

Per-build shares are viable. The three things that had to be true are:

- **Names can be deleted.** Yes, two ways, and the de-reserve path is self-healing.
- **No rate limit stands in the way.** Confirmed: the controller has no creation rate limiter at all.
- **The count ceiling is finite but unknown.** Hosted zrok.io's free tier advertises 50 share backends and 25
  environments; it does not publish a reserved-name number. With names deleted on teardown the steady-state count is
  the number of *live* previews, not the number of builds ever run, which is the property that makes per-build
  shares safe regardless of what the ceiling turns out to be.

The leak is the only hard blocker, and it is a missing call. What it changes about the code is not the call but the
teardown taxonomy: `close(entry)` currently means both "finished" and "being rebuilt", and only one of those may
delete the name. Conflating them either leaks names or churns preview URLs, and both failures are silent.

## What I could not verify

- **The actual limit applied to any real zrok.io account.** The numbers on the pricing page (5 GB/day, 25
  environments, 50 share backends, 50 private access frontends) are marketing copy for the free tier and do not map
  one-to-one onto the six limit-class dimensions in the code. In particular no reserved-name or unique-name number
  is published anywhere. `adminListAppliedLimitClasses` would answer it and is admin-only, so a hosted account
  cannot read its own ceiling. This needs the empirical probe described in section 2.
- **The exact `name` of the hosted `public` namespace** — whether previews land on `share.zrok.io` or
  `shares.zrok.io`. It does not matter to the code, which reads `FrontendEndpoints` and never composes a suffix,
  and it would matter a great deal to any code that guessed. One `zrok2 list namespaces` against the live account
  settles it.
- **That `deleteShareName` returns 404 rather than 500 for an already-deleted name.** The handler's
  `FindNameByNamespaceAndName` returns a wrapped `sql.ErrNoRows` and the handler maps any error from it to 404, so
  404 is what the code says. Untested against a live controller.
- **Whether hosted zrok.io will create a delegated namespace at all, and at what price.** The pricing page defers
  this to a sales conversation. Answering it is a question for NetFoundry, not for the source.
- **Whether a `NameSelections` slice with more than one entry works end to end on hosted zrok.io.** The controller
  loops the slice and the schema permits it, but docpreview has only ever sent one, and the rc8 CLI could not send
  even that (see `internal/expose/CLAUDE.md`).

## Order to build it in

1. **Split the two teardowns** in `internal/expose/zrok.go`. `withdrawEntry` on supersede must not touch the name;
   `Close`/final withdraw must. Nothing else here is safe until this distinction exists.
2. **Fix `isNameAlreadyExists`** to key on an empty payload rather than the status type, and propagate a non-empty
   payload verbatim. This is worth doing first regardless, because it is what makes every later failure legible.
3. **De-reserve then delete** on the final teardown. Two calls, ordered, both idempotent-ish.
4. **Validate the rendered `name_template` output** against `^[a-z0-9-]{3,63}$` before calling the API, with an
   error that names the template as the thing to change.
5. **Extend `Reap` to names**, using `ListNamesForNamespace`, deleting reserved docpreview-shaped names with no live
   share. This is the only way to recover what earlier versions already leaked. It needs an ownership marker in the
   name itself, because unlike a share a name has no `Target` field to stamp — which is the same instance-identity
   problem `internal/expose/CLAUDE.md` already records for shares.
6. Only then consider per-build shares.

## Sources

- Reserved names and namespaces — https://netfoundry.io/docs/zrok/concepts/namespaces/
- Manage reserved names (the `zrok2 create/list/modify/delete name` commands) —
  https://netfoundry.io/docs/zrok/how-tos/shares/manage-reserved-names/
- Dynamic proxy frontend migration guide, which is also the only place the wildcard DNS and wildcard certificate
  requirements are stated — shipped in the module at `website/docs/self-hosting/frontends/dynamic-proxy.md`, and
  published at https://docs.zrok.io/docs/guides/self-hosting/
- Configuring limits — https://netfoundry.io/docs/zrok/self-hosting/metrics-and-limits/limits/
- Hosted tier numbers — https://zrok.io/pricing/
- Introducing zrok v2.0 — https://blog.openziti.io/introducing-zrok-v2-0

The load-bearing files in the module, for anyone re-deriving this:

| File | Establishes |
|---|---|
| `controller/deleteShareName.go` | deletion semantics, the 409 guard, the ownership check |
| `controller/createShareName.go` | `Reserved: true` hardcoded, the limits 409, the DNS/profanity screen |
| `controller/updateShareName.go` | de-reserving, and that it has no live-share conflict check |
| `controller/unshare.go` | non-reserved names are deleted automatically; reserved ones are not |
| `controller/share.go` | `processNameSelections`; a name must pre-exist; the share limit flags |
| `controller/util.go` | `buildFrontendEndpointsForShare` — where `FrontendEndpoints` comes from |
| `util/names.go`, `util/template.go` | the name regex, and `name + "." + namespace` |
| `controller/limits/agent.go`, `controller/limits/config.go` | what is counted, and that nothing is by default |
| `controller/store/sql/postgresql/034_v2_0_0_namespaces.sql` | soft deletes, and the one-name-one-share index |
| `cmd/zrok2/deleteName.go` | the CLI equivalent, and that the default namespace token is `public` |
