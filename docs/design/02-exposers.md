# Exposers

An exposer turns an `http.Handler` into a URL a reviewer can open. It is the seam that makes docpreview
portable across "on my laptop", "through zrok", "through a NetFoundry Frontdoor", and "on an OpenZiti overlay".

## The interface

```go
type Exposer interface {
    Kind() string
    Validate(ctx context.Context) error
    Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error)
    Reap(ctx context.Context, keep map[string]bool) error
    Close() error
}
```

`Spec` carries `PreviewID`, `Name`, `BaseURL` and the `PullRequest`. `Publication` carries the resulting `URL`,
the `Name` actually used, and a close function.

### Why a handler and not a port

zrok's Go SDK gives back a `net.Listener` on the OpenZiti overlay. A zrok preview binds no local TCP port and
does not appear in `netstat`. An interface that asked for a port would force zrok to bind one pointlessly to
satisfy an abstraction only Frontdoor needs — and Frontdoor needs it because its agent dials *in* from the
outside, which is Frontdoor's problem to solve internally.

## The four implementations

| Kind | URL is | Binds | Remote objects |
|---|---|---|---|
| `local` | a path on the daemon's own listener | nothing | none |
| `zrok2` | a zrok share hostname | nothing (overlay listener) | share, name |
| `frontdoor` | a Frontdoor hostname | one port per preview | share |
| `ziti` | a hostname on an OpenZiti service | nothing (overlay listener) | none per preview |

### `local` — paths, not ports

Previews are served at `/preview/<name>/` on the address the daemon already answers on. It implements a second,
optional interface:

```go
type PathExposer interface {
    MountPath(name string) string   // "/preview/<name>/"
    Handler() http.Handler          // routes every mounted preview
}
```

`Ingress.Handler()` mounts `Handler()` at `expose.MountPrefix` when the exposer implements it.

It used to bind an ephemeral port per preview. That was wrong three ways at once:

1. **A port does not survive a restart.** Every URL the database recorded went dead the moment the daemon
   restarted, while the row still said `ready`.
2. **A hundred open pull requests meant a hundred listeners.**
3. **`http://127.0.0.1:62725/` says nothing** about which preview it is.

**Invariant: `MountPath` is pure and callable before anything is published.** The daemon needs it *before the
build*, because Docusaurus bakes `baseUrl` in at build time — see [03-build-pipeline.md](03-build-pipeline.md).

`Handler()` strips nothing from the path. Each `preview.Site` was built knowing its full prefix and strips its
own; handing it a shortened path would make it 404 the only URL it accepts.

A withdrawn preview returns `404 no preview named X is published` rather than the dashboard's generic 404, so an
expired link is distinguishable from a routing bug.

### `zrok2`

zrok v2 decouples names from shares: a name lives in a namespace independently of any share, so a fresh share
can be attached to the same name on every rebuild and a reviewer's bookmark keeps working. That property is why
the pull request comment can be written once and edited thereafter.

Shares are tagged `Target: "docpreview:<previewID>"`. That prefix is how `Reap` tells a share docpreview made
from one an operator made by hand — deleting the latter would be rude.

If `CreateShare` fails because the name is held by a share we do not know about — left behind by a process that
died without cleaning up — `reapName` reclaims it once and retries. Exactly once: a retry loop against a name
somebody else legitimately owns is a busy-wait.

### `frontdoor`

Frontdoor's agent dials out to a target URL, so each preview needs a real TCP port the agent can reach. It
reuses `Local`'s port machinery (`serve`, `closeLocal`) rather than reimplementing it.

**Unverified.** The share endpoint path and payload field names are modelled on the documented API convention
but have never been exercised against a live tenant. The guard that makes this safe to ship: `encoding/json`
does not error on absent fields, it leaves them zero and reports success — so a wrong field name would make
every publish "succeed" with an empty ID and URL, comment a link to `/`, orphan the local listener, and leak the
remote share. `Publish` therefore rejects a response with an empty `ID` or `URL`, naming the two structs to fix.

### `ziti`

One wildcard service, with previews separated by the HTTP `Host` header. `hostLabel` takes the first DNS label.

**Open:** this is not zero trust. The header is client-supplied, so anyone holding the `docpreview-reader`
attribute can reach every preview by sending any hostname. Alternatives are recorded in `TODO.md`; the one worth
noting is that `edge.Conn` carries the dialing identity, so authorization could move into the handler without
multiplying management-API objects per pull request.

## Naming

```
name_template  →  RenderName  →  SanitizeName  →  the label
```

`config.DefaultNameTemplate` is `{{.Repo.Name}}-{{.Name}}`, where `{{.Name}}` is the sanitized branch.

The default used to be the branch alone — "the URL is the branch name", which reads better. It is not unique.
Four projects each with a `new-install-guide` branch all render to `new-install-guide`.

`SanitizeName` appends a short hash **only when the transformation is lossy**, so `main` stays `main` but
`feature/foo` and `feature_foo` do not collapse onto each other.

The template is re-sanitized after rendering, because a repository name can contain characters legal on GitHub
and illegal in a hostname.

Deliberately **not** the commit SHA by default. Vercel gives every deployment an immutable URL plus a branch
alias; docpreview has one comment per pull request edited in place, so the link a reviewer already opened has to
survive the next push. `{{.HeadSHA}}` is available for anyone who wants per-commit URLs.

## The two bugs that keying got wrong

Both were found by the demo, not by reading. Both are now guarded by tests.

### 1. Keyed by name instead of preview ID

Every implementation kept its live publications in a map keyed by `spec.Name`. Names are branches; branches are
not unique across repositories. So `mydocs/new-install-guide` publishing withdrew `handbook/new-install-guide`.

Nothing logged an error, because withdrawing a preview you believe you own is a normal thing to do. The only
symptom was a dashboard full of `ready` rows whose links answered connection-refused. Measured: **1 of 10 ready
previews answered.**

**Invariant: `live` is keyed by the publication in every implementation** — `Spec.Key()`, not `Spec.Name`.

Consequence, since the name is now free to collide: `zrok`, `frontdoor` and `ziti` **refuse** a name another
publication holds, with an error naming the other one and the template that would separate them. Serving the
wrong site under somebody else's URL is worse than failing the build. `local` refuses too, since the name is
the path.

### The key is the publication, not the preview

`Spec.Key()` is the preview id for a preview's own share and `<preview>/<build>` for one build's share. That
distinction arrived with per-build publishing and it was the whole blocker, because `Publish` withdraws whatever
holds the key before taking it — so while the key *was* the preview id, publishing a build share tore down the
branch share it was meant to sit beside. One live share per preview, by construction, in all four
implementations.

A preview's own share keeps the **bare preview id** as its key, which is not cosmetic: the key is the remote tag
(`docpreview:<key>`) and the vocabulary of `Reap`'s keep-set. Change the branch share's spelling and the first
sweep after an upgrade finds an unfamiliar tag on every share it just restored and deletes all of them.

`TestTwoPublicationsPerPreview` pins the property: three publications of one preview live at three URLs, and
closing one leaves its siblings alone — which matters because the daemon closes the old publication *after*
publishing the new one.

### One preview, several shares, and one thing that is not cleaned up

A pull request now has a **branch** share following its newest successful build and a share per **build** pinned
to its commit. The branch share is what the pull request comment links to and is the contract; a build share is
best effort. Every way the second publish can fail — a reserved-name quota, a collision, an exposer that will not
mint two — logs a warning and costs one URL, because the alternative is a comment saying the docs did not build
when they did.

**The zrok *name* is never released.** Teardown deletes the share; the name is a separate object that
deliberately outlives its share, since that is what keeps a URL stable across rebuilds and restarts. Nothing in
this codebase deletes one — `reapName` deletes shares *holding* a name, never the name. Confirmed by merging a
real pull request: the share 404s and the name remains.

That is a slow leak at one name per branch and an unbounded one at one name per commit, because **the name is the
quota-bearing object, not the share**. It is tracked as a prerequisite for per-build shares rather than a
follow-up — see `TODO.md` and [19-zrok-namespacing.md](19-zrok-namespacing.md).

### 2. Close matched on key instead of identity

The daemon replaces a preview in this order, deliberately, so nothing is ever unserved:

```go
pub, _ := exposer.Publish(...)          // installs the new entry
if old := d.live[id]; old != nil {
    old.Close()                         // ← removed the entry just installed
}
d.live[id] = pub
```

Both publications carry the same preview ID. A close that deleted by key alone tore down its own replacement.
Every **rebuilt** preview went 404 while the database still said `ready`; the only survivors were previews
nobody had pushed to twice. Under zrok and Frontdoor the consequence is worse — their close deletes the
*remote share*, so a republish would destroy the share it had just created.

**Invariant: a withdraw triggered by a `Publication`'s close must verify the map still holds that exact entry.**

```go
func (l *Local) unmount(previewID string, entry *mount) {
    if l.mounted[previewID] == entry {   // identity, not key
        delete(l.mounted, previewID)
    }
}
```

The same shape guards `d.running` in the daemon and `z.live` in the ziti exposer: **an object that outlives its
successor must not be able to clean up on its behalf.**

Test: `TestLocalExposerSurvivesTheDaemonsReplaceThenCloseOrder`. Verified to fail without the guard.

## Reaping

`Reap(ctx, keep)` deletes anything the exposer owns whose preview ID is not in `keep`.

- **At startup**, `keep` is nil. Nothing is serving yet, so everything docpreview-tagged is an orphan from a
  previous process. Not merely tidy: zrok accounts have share limits, and a daemon that leaks a share per
  restart eventually stops working.
- **On every reaper tick** (hourly), `keep` is the set of preview IDs the database still holds.
- **`Preview.TTL`** tears down previews nobody has touched, which removes them from `keep` and so from the
  remote on the next pass.

`local.Reap` was a no-op, on the reasoning that a loopback listener cannot outlive its process. True of a port;
false of a map entry. A mount left behind after its preview is deleted keeps serving a URL nothing records.

**Open:** there is no `docpreview shares list` to audit a namespace against the database. The gap that matters
is a share created by a daemon whose database was then deleted — nothing claims it, and nothing looks.

## Invariants

1. `live` is keyed by preview ID.
2. A close verifies entry identity before deleting.
3. Publishing the same preview twice replaces, never fails.
4. Publishing a *different* preview under a name in use fails, and says which preview holds it.
5. `MountPath` is pure, and callable before the build.
6. `Reap(ctx, nil)` means "keep nothing".
7. `Publication.Close` is safe to call twice.
