# The ziti exposer

The plain-OpenZiti exposer, `exposer.kind: ziti`. It publishes previews onto an overlay with **no public surface
at all**: no hostname that resolves, no address that routes, no port that answers. A reviewer reaches a preview
only from a machine running a tunneler with an enrolled identity.

That is the strongest boundary of the four exposers and it is also the one that costs the most. Every other
exposer produces a link that works in a browser; this one produces a link that is dead everywhere except on the
overlay. **The trade is real authorization for the casual "click the link in the PR" flow, which is the whole
point of a preview.** Nothing in this document makes that trade go away — the recommendation below only makes
the authorization it buys actually per-preview, which today it is not.

This document is about the plain-ziti *exposer*. The daemon can also *listen* on a ziti service
(`config.ZitiListener`, `internal/daemon/listen.go:76`) so that the dashboard and webhook endpoint have no
underlay address. Those are two different grants on two different services with two different identities, and
`internal/config/config.go:113` says so deliberately. Where this document means the listener it says so.

## What exists today

| Piece | Where | State |
|---|---|---|
| The exposer | `internal/expose/ziti.go` | Works. Bound, served, host-routed, exercised end to end. |
| Provisioning | `internal/zitiadmin/provision.go` | Works, idempotently, via `docpreview configure ziti`. |
| Config | `internal/config/config.go:293` (`ZitiConfig`) | Three required fields, validated in `NewZiti`. |
| Wiring | `cmd/docpreview/main.go:305` | `case "ziti": return expose.NewZiti(...)`. |
| Name template | `internal/daemon/daemon.go:907` | Its own template, separate from zrok's. |
| Offline tests | `internal/expose/ziti_test.go` | Seven tests, no controller needed. |
| Live tests | `internal/expose/ziti_integration_test.go` | Skip unless pointed at two identity files. |

`Validate` loads the identity, authenticates, binds the service and starts an `http.Server` on the resulting
`edge.Listener` (`internal/expose/ziti.go:110-149`). `Publish` is a map insert (`ziti.go:233`). `Reap` returns
`nil` (`ziti.go:278`). `Close` shuts the server, closes the listener, closes the ziti context (`ziti.go:290`).

One gap worth noting before anything else, because it is a one-line fix: `docpreview init` will not select this
exposer. `checkExposerKind` accepts only `zrok2`, `frontdoor`, `local` (`cmd/docpreview/init.go:316-323`) and
the interactive menu lists the same three (`init.go:96-99`), while `config.Server.validate` accepts `ziti`
(`internal/config/config.go:513`) and `configure ziti` sets it (`cmd/docpreview/configure.go:114`). So the
supported way in is `configure ziti`, and `init -exposer ziti` fails with "unknown exposer".

## The reviewer's experience, stated up front

A reviewer needs three things before a preview URL means anything: a tunneler (Ziti Desktop Edge or
`ziti-edge-tunnel`), an enrolled identity carrying the reader attribute, and the tunneler switched on. Without
them `http://my-branch.docpreview.ziti/` is NXDOMAIN — the `.ziti` TLD does not exist publicly and is not meant
to.

Consequences that follow, and that should be in the user documentation rather than discovered:

- The pull request comment contains a link that is dead for anyone not enrolled, including whatever renders
  link previews in chat. `www/docs/future/ziti-native-previews.md:405` records the alternative: intercept a real
  subdomain the organization owns, so a reviewer without the tunneler gets NXDOMAIN on a name that at least
  looks legitimate rather than an invalid TLD.
- Onboarding a reviewer is a one-time-token ceremony. `configure ziti` mints exactly one sample reviewer
  (`provision.go:436`) and there is no `docpreview identity add` yet, so today the answer to "add Bob" is
  "use the `ziti` CLI".
- There is **no revocation story**. `docs/design/10-security.md:118` calls this the largest known gap, and it is
  worse here than anywhere else, because on this exposer the enrolled identity *is* the authorization.

So: choose this exposer when the documentation under review must not exist on the internet, and accept that
every reviewer is an enrollment.

## The authorization hole

This is the headline, and it is the thing that gates calling the exposer finished.

One wildcard service carries every preview. The `intercept.v1` config covers `docpreview.ziti` and
`*.docpreview.ziti` (`provision.go:166`, `provision.go:209-239`), one Dial policy grants
`#docpreview-reader` (`provision.go:291-297`), and `route` picks the preview from the first label of the HTTP
`Host` header (`ziti.go:156-205`).

The `Host` header is client-supplied. Nothing in `route` consults the connection it arrived on. So **any
identity holding the reader attribute can reach every preview by sending any hostname** — with `curl -H Host:`
over the overlay, no tunneler required, exactly as `overlayClient` does in
`ziti_integration_test.go:59-80`. There is no per-preview grant anywhere in the system to violate; there is one
grant, "may dial the service", and it is total.

Why that specifically is not zero trust: the authorization decision is made once, at enrolment, on the question
"are you a reader" — and never again on the question "may you read *this*". The overlay authenticates the dialer
cryptographically and then hands it a fully trusted channel to a multiplexer that routes on an unauthenticated
string. Compare `internal/daemon/secrets.go:86-105`, which refuses to serve credential writes whenever any
listener is ziti, for precisely this reason:

> A ziti listener is arguably a stronger boundary than loopback — the overlay authenticates the dialer — but the
> admin surface does not yet check the dialing identity, so "enrolled at all" would be the whole authorization.

That is the same argument, and it reaches the same conclusion: an authenticated channel is not an authorization
decision. `TODO.md:43-50` records the question as **undecided**:

> **Per-preview authorization on the ziti exposer.** Today one wildcard service serves every preview and requests
> are separated by the HTTP `Host` header. That is not zero trust: the header is client-supplied, so anyone
> holding the `docpreview-reader` attribute can reach every preview by sending any hostname. Per-preview services
> with per-preview Dial policies would put authorization on the overlay where it belongs, at a cost of four
> management-API objects per pull request and DNS churn on every connected tunneler. A middle option exists —
> keep one service and check the dialing identity in the HTTP handler, since `edge.Conn` carries it — which gets
> real authorization without the object churn but leaves the boundary in the application rather than the network.

### Option A — a service per preview

Publishing creates the four objects `www/docs/future/ziti-native-previews.md:156-166` enumerates: an
`intercept.v1` config, a service, a Bind policy, a Dial policy naming only the identities allowed on *that*
preview. Authorization moves onto the controller, which is where a zero-trust purist wants it, and a reviewer
without the grant cannot even resolve the name.

What it costs:

1. **Publish stops being free.** Every push becomes a controller round trip — four creates, or four
   idempotent check-then-creates — inside the commit phase, which today is a mutex and a map write. Every
   failure mode of `internal/zitiadmin` (network, auth, partial create) moves into the build path.
2. **Policy churn on every connected tunneler.** A new service triggers a service-list update, and on Windows
   NRPT rule rewrites, on every attached ZDEW. A busy repository would have every reviewer's tunneler
   continuously reconfiguring its DNS.
3. **Twenty open pull requests is twenty entries in the reviewer's identity tile.**
4. **Four objects to leak per preview instead of zero** — see [Reap and orphans](#reap-and-orphans) below,
   where a leaked Dial policy is a standing grant nobody remembers making.
5. **It answers a question nobody has asked.** Who is the per-preview grant *for*? docpreview has no model of
   "these reviewers may see this pull request"; the SCM does. Wiring a Dial policy per preview needs a source of
   truth for its identity roles, and there is not one.

### Option B — a server-side allowlist keyed to the dialing identity

Keep one service. Keep the `Host` routing. Add a map from dialing identity to the set of preview names it may
reach, consulted in `route` before dispatch.

This has the shape of authorization without the substance, because docpreview would be the one inventing the
mapping. Where does it come from — config? A vault key? The pull request's reviewer list, which docpreview does
not fetch? Until there is a real answer, an allowlist is a second copy of the reader attribute with more places
to get it wrong. **Not recommended, and not because of cost — because the input does not exist.**

### Option C — check the dialing identity in the handler (recommended)

Keep the wildcard service, and make `route` verify that the connection it is serving belongs to an identity
allowed to be there — then log it, and enforce whatever policy exists.

**Step 1 of this is built, 31 July 2026.** `zitiConnContext` puts the dialing identity on the request context and
`route` logs it: Debug per request, Info the first time a given identity reaches a given preview, and the 404 line
now names who asked. Four offline tests cover it, including one that drives a real `http.Server.Serve` loop rather
than calling the hook directly — which is the part worth testing, since the question is whether `http.Server`
passes the conn the listener returned.

Two things the spike established that this document did not know:

**There is a gate that can silently disable it.** The hosting side only records the dialer when
`ziti.ListenOptions.DoNotSaveDialerIdentity` is false. `DefaultListenOptions()` leaves it false and
`Context.Listen(name)` uses those defaults, so the existing call already gets the identity — but anything that
switches to `ListenWithOptions` and sets that flag would blind this feature with no error anywhere.

**Logging is not authorization, and the secrets-surface refusal cannot be lifted yet.** Two reasons, and the first
is structural: the hook is on the *exposer's* server, while `internal/daemon/secrets.go` refuses based on the
*ingress* listener — a different server, a different service, a different identity. The same five lines have to go
there too. The second is that the refusal's stated reason is that the surface does not *check* the identity, and an
identity that arrives and is logged is still not one compared against an allowed set. That needs a list of admin
identities and a comparison that **fails closed on an empty id**, because empty is what both a non-overlay
connection and a router that never sent the header produce. The plumbing is no longer the blocker; the policy input
is.

The API is real and vendored. `edge.ServiceConn`, which `edge.Conn` embeds, carries:

```go
// sdk-golang@v1.9.0 ziti/edge/conn.go:119-120
GetDialerIdentityId() string
GetDialerIdentityName() string
```

Reaching it from an `http.Handler` needs `http.Server.ConnContext`, which is called with the accepted
`net.Conn` before the connection is served:

```go
z.srv = &http.Server{
    Handler:           http.HandlerFunc(z.route),
    ReadHeaderTimeout: 15 * time.Second,
    ConnContext: func(ctx context.Context, c net.Conn) context.Context {
        if ec, ok := c.(edge.Conn); ok {
            return context.WithValue(ctx, dialerKey{}, dialer{
                id:   ec.GetDialerIdentityId(),
                name: ec.GetDialerIdentityName(),
            })
        }
        return ctx
    },
}
```

`route` then reads it off `r.Context()`. What to do with it, in the order it should be built:

1. **Log it on every request, and put it in the 404.** Today a 404 says which host was asked for; it should say
   who asked. That alone turns "somebody is probing hostnames" from invisible into a log line, and it is the
   cheapest thing on this list.
2. **Enforce it per preview once there is a rule to enforce.** The natural rule is not a config list but the
   pull request: a preview is reachable by the identities docpreview can associate with that pull request, plus
   an always-allowed operator attribute. That is a feature with an owner, not a plumbing change.
3. **Make the admin listener use it too.** `internal/daemon/secrets.go:91` refuses to serve because the
   dialing identity is unchecked. The same `ConnContext` hook on the ingress server (`internal/daemon/listen.go`)
   is what unlocks that refusal, and it is the same five lines. Doing this once, in one place, for both servers
   is the argument for Option C over Option A on its own.

**Recommended: C, in that order, with step 1 done immediately.** The reason is not that C is stronger than A —
it is not; A puts the decision on the controller where an attacker who compromises docpreview cannot reach it.
The reason is that C is *honest about where the boundary is* and can be built now, whereas A requires inventing
the per-preview identity model that neither option can do without. The cost of C, stated plainly: the boundary
lives in docpreview's process, so a bug in `route` is a bypass, and there is no defence in depth behind it. That
should be written into `docs/design/10-security.md` under "what is deliberately not defended" the day it ships,
replacing the current **Open** paragraph at `10-security.md:108`.

If A is ever chosen instead, the migration is not free: the intercept config becomes per-preview, so the
`domain` field stops describing one wildcard and starts describing a naming scheme, and every reviewer's
tunneler has to re-sync. Deciding late is more expensive than deciding now.

## The single-binder constraint

**Exactly one docpreview process may bind a given service.** This is stated in three places — the type comment
at `internal/expose/ziti.go:36-42`, the ingress equivalent at `internal/daemon/listen.go:69-75`, and the
generated config comment at `cmd/docpreview/init.go:454` ("Exactly one docpreview may bind it") — because it is
not discoverable from a failure.

What happens if two try: binding creates a ziti **terminator**. Two bindings create two terminators on the same
service, and the router load-balances across them under the default strategy. Each docpreview holds a *disjoint*
`live` map, so each preview is served by exactly one of the two processes. A reviewer's requests are split
between them, and roughly half of them 404 — served by the instance that never published that preview. It looks
like a flaky network, or a flaky browser cache, or an intermittent build problem. It does not look like a
configuration error, and nothing logs anything unusual: a 404 for an unpublished host is a normal event
(`ziti.go:174-178`).

This was found in the tests, not in production, and the workaround is recorded there:
`ziti_integration_test.go:82-99` shares one `*Ziti` across the whole package precisely because a per-test
exposer produced "roughly half its requests answered by the previous test's still-closing instance".

The grant is also looser than it looks. The Bind policy is keyed on a role attribute, not an identity name:
`IdentityRoles: []string{"#" + p.o.HostIdentity}` (`provision.go:287`), and the hosting identity is created
carrying that attribute (`provision.go:382`). So a second identity given the same attribute silently becomes a
second legal binder. That is a convenient hook for a planned failover and a foot-gun for anyone copying
attributes around.

**This is the hardest exposer to run redundantly, and that should be said out loud.** Under `local` a second
daemon is a second port. Under `zrok2` and `frontdoor` the remote namespace arbitrates — a duplicate name is
refused by something outside the process. Here, two instances on one service is a *silently degraded* state.
The options, none of them free:

- **Two services, two domains, two docpreviews.** Works today, needs no code, and means two sets of preview
  URLs. Which is what `ziti.go:42` recommends: "Give a second instance its own service and domain."
- **Active-passive with an external lease.** The standby must not bind until the active's terminator is gone.
  docpreview has no leader election and sqlite is the only shared state; this is a real feature, not a flag.
- **Detect it.** After binding, docpreview could ask the controller how many terminators the service has and
  refuse to serve if the answer is more than one. That converts a silent 50% failure into a startup error, which
  is the single highest-value hour on this list. It needs management-API credentials at `serve` time, which the
  exposer does not have today — `ZitiConfig` has no controller endpoint, only an identity file
  (`config.go:293-301`).

Until one of those exists, "one docpreview per service" is an operational invariant enforced by a comment.

## Reap and orphans

Today `Reap` is honestly a no-op, and the comment explains why: "there is nothing on a controller to collect:
the wildcard service is permanent and previews never created remote objects" (`ziti.go:272-287`). Entries leave
the map through `Publication.Close`, which both teardown and supersede call. At startup, `keep` is nil and the
map is empty, so there is nothing to do (`daemon.go:373-377`).

That is correct *for the wildcard design* and it is exactly what stops being true under Option A. So this
section is the cost of Option A, written down before it is paid.

If publishing creates controller-side objects, a hard kill — `taskkill /f`, a power loss, a panic — leaves
behind whatever the last publish created and the process never deleted: a config, a service, a Bind policy, and
a **Dial policy**. The first three are clutter and a quota problem. The fourth is a standing authorization
grant, for a pull request that closed weeks ago, that nobody remembers creating and no code will ever revisit.
**Treat an orphaned Dial policy as a security defect, not as litter.** It is the same class of bug as a
forgotten SSH key.

Finding them requires what `zrok` already has and this exposer does not: a tag. `zrok` tags every share
`Target: "docpreview:<previewID>"`, and that prefix is how `Reap` distinguishes an object docpreview made from
one an operator made by hand (`docs/design/02-exposers.md:73-74`). Under Option A the same discipline applies —
every created object carries the preview ID, `Reap` lists by tag, and anything whose ID is not in `keep` is
deleted. Deleting untagged objects is not an option: the wildcard service, the admin service and the bootstrap
policies from `configure ziti` are all untagged, and reaping them would revoke the whole network.

Two ordering rules that a naive `Reap` gets wrong:

1. **Delete the Dial policy first.** If deletion is interrupted after the service is gone but before the policy
   is, the leftover is the grant — the most dangerous of the four. Deleting the grant first means an
   interrupted reap leaves an unreachable service, which is harmless.
2. **Reap at startup before republishing.** `daemon.go:373` already does this, and it must keep doing it under
   Option A: republishing first would find its own orphaned objects present and either fail or adopt them.

There is also an orphan class that no `Reap` will ever find, and it is worth stating because it applies today:
a docpreview whose database was deleted. Nothing claims its objects and nothing looks for them — the same gap
`02-exposers.md:187` records for zrok. A `docpreview ziti audit` that diffs the controller against the database
is the answer, and it does not exist.

## Identity and enrollment

docpreview's own identity is the credential that lets a process host previews. `configure ziti` creates it,
enrolls it, and writes the enrolled identity to `<out-dir>/<host-identity>.json` at mode `0600` with the comment
"this file is the private key that lets anything host docpreview's services" (`provision.go:355-404`). The
default location is `<data-dir>/ziti` (`cmd/docpreview/configure.go:79`). `exposer.ziti.identity_file` points at
it; `NewZiti` refuses to construct without it (`ziti.go:82-85`).

**Does it belong in the vault?** No, and the reasoning is worth recording so it is not relitigated. The vault
holds values docpreview *reads and passes on* — a GitHub private key, a Frontdoor token, build secrets — and its
whole purpose is that they never reach disk in the clear or a log (`docs/design/05-secrets.md`). A ziti identity
file is not passed anywhere: `ziti.NewContextFromFile` takes a path (`ziti.go:115`) and the SDK owns the key
material from there. Putting it in the vault would mean decrypting it to a temporary file for the SDK to read,
which is strictly worse than a `0600` file — the same secret, plus a temp file, plus a deletion to get wrong.
The honest framing is that the identity file is *peer* to the vault, not content of it: two files, both of which
compromise the deployment if read. That is worth one line in `05-secrets.md` and one in the `init` comments,
because today an operator has no way to know the vault is not the answer.

**Rotation** is the destructive path in `provision.go`, and it is deliberately narrow. `ensureHostIdentity`
authenticates the existing file rather than stat-ing it (`identityFileUnusable`, `provision.go:415-429`), and if
the controller rejects it, deletes the identity and recreates it — "the one destructive act in this package, and
it is confined to docpreview's own identity" (`provision.go:341-354`). The reasoning is that an enrollment token
is one-time: an identity whose file cannot authenticate is not a state worth preserving, because nothing can
ever host with it. So rotation is: delete the file, re-run `configure ziti`. That works and it is undocumented.

**Expiry** splits in two, and only one half is handled:

- An *enrollment token* that expires unused. For the hosting identity this cannot happen — `configure ziti`
  enrolls immediately (`provision.go:388`). For a reviewer it happens all the time, and re-running `configure
  ziti` does not fix it: `ensureReviewer` is never destructive, and a spent or expired token reads as
  `ReviewerEnrolled` with no `.jwt` written (`provision.go:436-479`). `TODO.md:69-71` records
  `docpreview identity add` as the fix.
- An *enrolled certificate* reaching end of life. The SDK handles renewal while the process is running and
  authenticated. A docpreview that has been *stopped* for longer than the certificate's life comes back with a
  `Authenticate` failure at `ziti.go:121`, whose error text names the identity file but does not say
  "expired" — an operator sees an authentication error and reasonably suspects the wrong file. **Not verified:**
  the certificate lifetime and the SDK's renewal behaviour are recalled, not read out of the vendored SDK.

## Naming and intercept addresses

Two independent things must agree on the same string:

```yaml
exposer:
  kind: ziti
  ziti:
    identity_file: /var/lib/docpreview/ziti/docpreview-host.json
    service: docpreview                 # exactly one docpreview may bind this
    domain: docpreview.ziti             # must equal the intercept.v1 addresses
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

`domain` decides the URL docpreview *publishes* — `JoinURL("http://"+name+"."+z.cfg.Domain, spec.BaseURL)`
(`ziti.go:252`). The service's `intercept.v1` addresses decide what the tunneler *resolves* —
`[]string{p.o.Domain, "*." + p.o.Domain}` (`provision.go:166`). `configure ziti` sets both from one `-domain`
flag, which is why the happy path works and why nobody notices the coupling. The generated config warns about it
at `cmd/docpreview/init.go:456-457` and `NewZiti` repeats it in the error text when `domain` is empty
(`ziti.go:90-91`), but nothing checks it: the exposer never reads the intercept config.

The failure mode from a user's point of view, if someone edits `domain` in the YAML and restarts:

1. docpreview starts clean. `Validate` succeeds — the identity is fine, the Bind policy is fine, the service
   binds. The log says `bound ziti service` with the new domain (`ziti.go:146`).
2. Builds succeed. The dashboard shows `ready`. The pull request comment gets a link.
3. The reviewer clicks it and the browser says the site cannot be reached. Not a 404 — a DNS failure. The
   tunneler still resolves `*.old-domain.ziti`, and was never told about the new one.
4. Meanwhile `http://anything.old-domain.ziti/` *does* resolve, reaches docpreview, and gets the 404 page
   listing previews with links built from the **new** domain (`ziti.go:191`) — which also do not resolve.

So the symptom is "every preview is unreachable, the daemon is healthy, and the one page that does load offers
links that also fail". Every layer reports success. The apex hostname is the only thing that answers, and it
answers with a page whose links are wrong.

The fix is cheap and belongs in `Validate`: docpreview binds the service, so it can read the service's configs
and compare the `intercept.v1` addresses to `cfg.Domain`, failing at startup with both values named. **Not
verified:** whether the addresses are reachable from `ziti.Context` alone or need the management API — if the
latter, the check belongs in `docpreview doctor`, which already has a home for controller-touching checks.

Naming otherwise follows the shared rules in `02-exposers.md`: `RenderName` then `SanitizeName`, with `ziti`'s
own `name_template` (`daemon.go:907`). The collision rule matters more here than elsewhere because the label
*is* the routing key: a second preview claiming a live label is refused with an error naming the incumbent and
the template that separates them (`ziti.go:242-248`, tested at `ziti_test.go:96`).

## Testing

**The offline tests are the real coverage and they are good.** `ziti_test.go` sets `z.bound = true` directly
(`ziti_test.go:38`) and calls `z.route` against an `httptest.NewRecorder`, which reaches the map, the collision
check, the withdraw-identity guard, the 404 listing, the `Host` escaping, and `hostLabel` — everything except
the bind. Seven tests, no network, and they run in `go test ./...` today.

**The integration tests do not run today, and nothing says so.** `zitiIdentities` skips unless both
`DOCPREVIEW_ZITI_HOST_IDENTITY` and `DOCPREVIEW_ZITI_READER_IDENTITY` are set and readable
(`ziti_integration_test.go:37-51`), and they additionally hardcode `Service: "docpreview-svc"` and
`Domain: "docpreview.ziti"` (`ziti_integration_test.go:107-108`) — so they need not just any two identities but
a network provisioned with exactly those names. `configure ziti` defaults are not guaranteed to match. The same
is true of `internal/zitiadmin/provision_integration_test.go`. A green `go test ./...` proves nothing about the
overlay, which is fine as long as it is not mistaken for proof.

What they buy when they do run is stated at `ziti_integration_test.go:32-35` and is exactly right: that Bind
actually binds, that a Dial policy grants a differently-attributed identity, and above all that the `Host`
header survives the overlay — the one assumption the wildcard design cannot survive without.

What can be tested without a controller, and is not yet:

- **The authorization check, once it exists.** `ConnContext` puts a value on the request context; a test can put
  the same value there directly and assert that `route` refuses a preview the identity may not see. That is the
  most security-critical logic in the exposer and it needs no overlay at all.
- **A fake `edge.Conn`.** `edge.ServiceConn` is an interface (`sdk-golang@v1.9.0 ziti/edge/conn.go:110`), so a
  stub returning a chosen `GetDialerIdentityId()` over an in-memory pipe makes the `ConnContext` wiring itself
  testable — including the `ok` branch where the conn is *not* an `edge.Conn`, which is the path a unit test
  would otherwise never take.
- **The domain/intercept agreement**, if the check lands as a pure function over two strings.
- **`Reap` under Option A**, against a fake management client, the way `frontdoor_test.go` fakes its API. The
  delete-Dial-policy-first ordering is a property worth a test rather than a comment.

The end-to-end path stays manual and stays documented: `scripts/ziti-trial/`, then a real ZDEW install
(`www/docs/future/ziti-native-previews.md:366-393`).

## Where plain ziti beats zrok, specifically

zrok is built on OpenZiti, so "use zrok" is the obvious first answer, and it is wrong for this — not marginally,
and not for reasons of taste. `www/docs/future/ziti-native-previews.md:48-89` closes it off with evidence:

- A zrok private share creates a service with exactly one config, `zrok.proxy.v1`, and **no** `intercept.v1`.
  Repository-wide greps for `intercept.v1`, `host.v1` and the tunneler config types return zero matches across
  the whole zrok tree, v1 and v2. A tunneler builds its DNS table and its intercepts from `intercept.v1`; a
  service without one gives it nothing to resolve and nothing to capture. **A zrok share cannot be reached by
  opening a URL in a browser behind a tunneler.** Both ends of a zrok share are SDK code, which is why zrok
  never needed to describe one.
- Dial rights are scoped shut by construction. Private share creation makes a Bind policy and a SERP and
  deliberately no Dial policy; dial rights come only from `POST /access`, which always grants
  `IdentityRoles: ["@"+envZId]` for a zrok *environment* identity owned by the calling account. **An
  externally-enrolled ziti identity cannot be granted dial rights through the zrok API at all.**

So the decision is not "which is nicer" but "which one can a tunneler reach". If reviewers must use a tunneler
and an enrolled identity, plain ziti is the only one of the two that works. Choose zrok2 whenever a link in a
browser is acceptable — it is the better product for that, it has a name/share split that keeps bookmarks alive
across rebuilds (`02-exposers.md:67-71`), and it needs no controller credentials. Choose plain ziti when the
requirement is that the address does not exist off the overlay.

The one thing zrok gives that this exposer will never get for free: a remote namespace that arbitrates. zrok's
controller refuses a duplicate name and enforces share quotas; here the map is the only registry
(`ziti.go:215-232`), which is why the collision check and the identity-guarded withdraw had to be written by
hand.

## The order to build it in

1. **Log the dialing identity.** `ConnContext` on the exposer's `http.Server`, the identity onto the request
   context, and into every request log line and the 404 page. Half a day, no policy decisions, and it makes the
   hole observable before it is closed.
2. **Make `init` offer this exposer.** Add `ziti` to `checkExposerKind` (`cmd/docpreview/init.go:316`) and to the
   interactive menu, with a line naming the trade: no public URL, every reviewer needs a tunneler. One hour.
3. **Fail fast when two instances bind.** Either a terminator count check at startup, or — if that needs
   management credentials the exposer does not have — a loud startup warning naming the invariant and pointing at
   the two-services answer. Highest value per hour on this list, because it converts a silent 50% failure.
4. **Check `domain` against the service's `intercept.v1` at startup**, in `Validate` or `doctor`, failing with
   both values named.
5. **Write down what the identity file is.** That it is peer to the vault and not content of it, that rotation is
   delete-the-file-and-re-run, and that `0600` is load-bearing. `docs/design/05-secrets.md` and the generated
   config comments.
6. **Decide the authorization question, and record it in `TODO.md` as decided.** The recommendation here is
   Option C. Without a decision, steps 7 and 8 cannot start.
7. **Per-preview authorization in the handler**, with the offline tests described above, and the honest note in
   `docs/design/10-security.md` that the boundary now lives in docpreview's process.
8. **Reuse the same hook on the ingress listener** and lift the secrets-surface refusal at
   `internal/daemon/secrets.go:91`. This is the payoff that makes C worth more than its own scope.
9. **`docpreview identity {add,list,remove}`** (`TODO.md:69-71`). Until `remove` exists there is no revocation,
   and on this exposer revocation is the only thing that undoes access.
10. **Make the integration tests runnable from `configure ziti` output** — read the service and domain from
    environment or config instead of hardcoding `docpreview-svc`, and say in the README that a green
    `go test ./...` proves nothing about the overlay.
11. **Only then, if per-preview overlay authorization is genuinely required:** Option A, with tagging and the
    delete-grant-first `Reap` ordering designed in from the start rather than retrofitted.

## Not verified

Everything above cited as `path/file.go:NN` was read. These were not:

- **OpenZiti terminator and load-balancing semantics.** That two Bind sessions create two terminators and that
  the router splits traffic between them under the default smartrouting strategy is taken from the comments at
  `internal/expose/ziti.go:36-42`, `internal/daemon/listen.go:69-75` and the test note at
  `ziti_integration_test.go:82-99`. Consistent across three places written by people who hit it, but not
  confirmed against ziti's router source, and the specific claim "about half the time" is illustrative.
- **Whether a service's `intercept.v1` addresses are readable from `ziti.Context`** without management-API
  credentials. Step 4 above depends on this and it was not checked in the vendored SDK.
- **Whether the controller exposes a terminator count** reachable with the credentials docpreview has at `serve`
  time. Step 3's cheap form exists either way; its good form depends on this.
- **Enrolled-certificate lifetime and the SDK's renewal behaviour**, including what happens to a docpreview
  stopped for longer than the certificate's life. Recalled, not read.
- **`ConnContext` receiving the `edge.Conn` unwrapped.** `http.Server` passes the `net.Conn` it accepted, and
  `edge.Listener.Accept` returns the edge connection, so the type assertion should hold — but no code in this
  repository does it yet and it has not been run.
- **The zrok findings** in [Where plain ziti beats zrok](#where-plain-ziti-beats-zrok-specifically) are quoted
  from `www/docs/future/ziti-native-previews.md`, which cites zrok source line numbers. Those citations were not
  re-verified against the zrok tree here.
- **That the integration tests have never been run in this worktree.** Inferred from the skip guard and the
  hardcoded service name, not from a test log.
- **NetFoundry-hosted networks** as an alternative to a self-hosted controller. `TODO.md:52-53` records the
  decision as unmade; nothing in this document assumes either.
