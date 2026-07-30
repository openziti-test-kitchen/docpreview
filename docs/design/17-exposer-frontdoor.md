# Getting the Frontdoor exposer into production

Every preview docpreview has ever served has come from the `local` exposer, which mounts previews at
`/preview/<name>/` on the daemon's own listener (`internal/expose/local.go:111`). That URL is reachable from the
machine the daemon runs on and nowhere else, which has been fine for proving the pipeline and is useless for the
thing the pipeline exists to do: give a reviewer a link. This document is about the exposer that is meant to
close that gap in a NetFoundry environment, what is actually implemented, and what has to happen before a
preview URL from it can be put in a pull request comment.

Read [02-exposers.md](02-exposers.md) first. This is a depth pass on one of the four implementations, not a
replacement for the abstraction it lives inside.

## What Frontdoor is here, and the one structural difference

Frontdoor and zrok solve the same problem with the same three primitives — an enrolled agent on the private
side, a hardened global frontend, and a share mapping a public route onto a private target
(`www/docs/runbooks/frontdoor.md:29`). The `Exposer` interface exists so that switching between them is a
one-line config change and nothing upstream can tell the difference
(`internal/expose/frontdoor.go:34`).

The difference that leaks through is the direction of the connection. zrok's SDK hands back a `net.Listener` on
the OpenZiti overlay and docpreview serves into it, so a zrok preview binds no local TCP port and does not
appear in `netstat` (`internal/expose/expose.go:10`). Frontdoor's agent dials *out* to a target URL, so a
Frontdoor preview must be something dialable. `Publish` therefore calls `Local.serve`, which binds an ephemeral
port and starts an `http.Server` on it (`internal/expose/frontdoor.go:158`, `internal/expose/local.go:214`), and
sends the resulting address as the share's target:

```go
body := shareRequest{
    Name:      spec.Name,
    Frontend:  f.cfg.Frontend,
    TargetURL: fmt.Sprintf("http://%s:%d", f.ports.host, local.port),
    Tag:       targetPrefix + spec.PreviewID,
}
```

`internal/expose/frontdoor.go:163`.

**One port per publication, not one shared port.** `serve` listens on `net.JoinHostPort(l.host, "0")` every time it
is called (`internal/expose/local.go:215`), and `Publish` calls it once per publish with no reuse. A hundred
open documentation pull requests means a hundred listeners and a hundred `http.Server` goroutines — precisely
the arrangement the `local` exposer was rewritten to escape (`internal/expose/local.go:27`). Under Frontdoor
that cost is not optional: the agent has an address, not a handler, so there is nothing to multiplex on. It is
also not, at this scale, a problem worth engineering around; ephemeral port ranges are tens of thousands wide
and the goroutines are idle. It is worth *knowing*, because it is the reason `frontdoor` is the one exposer
whose resource use grows with the number of open pull requests.

**Per-build shares multiply that, and the multiplier is the push rate rather than the pull request count.** A
preview now publishes once for the branch and once per build kept, so a hundred pull requests at `keep_builds: 10`
is up to eleven hundred listeners, shares and goroutines rather than a hundred — and each one is a remote share on a
paid tenant as well as a local port. Nothing enforces a ceiling on the Frontdoor side today. The disk caps in
`TODO.md` are written for zrok's free tier and explicitly should not be inherited here, but *some* bound on live
shares per tenant is the thing this exposer needs that the others do not.

### What the bound address must be, and what that costs

`agent_reachable_host` does double duty: it is both the interface the preview listens on and the host advertised
in `targetUrl`. That is one field, one value, two jobs, and it is the field with a real constraint
(`internal/config/config.go:345`).

- **Agent on the same host as docpreview** — `127.0.0.1`, the default (`internal/config/config.go:463`).
  Nothing is exposed beyond loopback. The loopback-plus-tunnel property that makes the `zrok2` and `ziti`
  exposers pleasant to reason about survives intact: the only route to a preview is through the frontend, which
  is where the WAF and the IdP are.
- **Agent elsewhere** — a LAN address. The preview is then served, unauthenticated, on that interface, to
  anything that can route to it. The frontend is no longer the only door. `nosniff` and read-only methods are
  hardening, not access control ([10-security.md](10-security.md)), and the content is HTML written by whoever
  opened the pull request. A host firewall scoped to the agent's address is doing the whole job, and nothing in
  docpreview checks that it exists.

So the honest statement is: **Frontdoor preserves "nothing exposed to the network directly" only when the agent
is co-located.** The remote-agent arrangement trades that property for deployment convenience, and the trade is
invisible in the config file — `config.validate` checks the exposer kind, the build driver, workers, the data
directory, the key source, the listeners and three durations, and nothing at all inside `FrontdoorConfig`
(`internal/config/config.go:511`). `0.0.0.0` is accepted and produces a listener on every interface plus a
`targetUrl` of `http://0.0.0.0:<port>`, which no agent can dial. Worth a validation rule.

## The token, and the boot-order bug one level down

Frontdoor is the only exposer that needs a credential to be *constructed*:

```go
case "frontdoor":
    v, err := w.Vault()
    if err != nil {
        return nil, err
    }
    token, err := v.MustGet(vault.KeyFrontdoorToken)
    if err != nil {
        return nil, err
    }
    return expose.NewFrontdoor(cfg.Exposer.Frontdoor, token, log)
```

`cmd/docpreview/main.go:307`. `local` needs nothing, `zrok2` reads an enabled environment from disk, `ziti`
reads an identity file. Only this one opens the vault, and `NewFrontdoor` refuses a zero token outright
(`internal/expose/frontdoor.go:83`).

`buildExposer` is called from `setup` before anything else and its error is fatal (`cmd/docpreview/main.go:217`).
`w.Vault()` returns `vault.ErrLocked` when there is no key source, no `DOCPREVIEW_MASTER_KEY` and no terminal
(`internal/vault/vault.go:66`, `internal/vault/vault.go:319`) — which is the *default* configuration, chosen
deliberately so that the key that decrypts the vault is not sitting somewhere a process can read it
(`internal/config/config.go:64`).

**This is now an inconsistency, and it is the exact bug that was just fixed one level up.** With `github.app_id`
set and the vault locked, `setup` no longer fails; it warns and starts without a GitHub client, and
`rewireGitHub` installs one the moment the vault opens (`cmd/docpreview/main.go:223`,
`cmd/docpreview/main.go:476`, [11-github-setup-state.md](11-github-setup-state.md)). The reason was that the
page which unlocks the vault is served by the daemon that refused to start. With `exposer.kind: frontdoor` that
reasoning applies unchanged and the fix was not applied: the daemon cannot boot to its own setup page. And the
setup page already lists `frontdoor.api_token` as a key it knows how to collect, gated on
`c.Exposer.Kind == "frontdoor"` (`internal/daemon/secrets.go:216`) — so the UI offers to store a credential the
daemon must already have in order to serve that UI. `docpreview init` compounds it by telling a frontdoor user
to set up a key source and store the token *before* running anything (`cmd/docpreview/init.go:369`), which is
correct advice and also the only path that works today.

The cost is concrete: a fresh frontdoor install, or any restart after a key rotation, is a terminal-only
recovery. Two `docpreview vault set` invocations and an edit to `key_source` before the process will start at
all.

The fix is not "tolerate the error and continue", because `d.exposer` is fixed at `daemon.New`
(`internal/daemon/daemon.go:123`), there is no `SetExposer`, and `Ingress.Handler` decides whether to mount a
path-serving exposer while wiring (`internal/daemon/ingress.go:128`). Two shapes work:

1. **A lazy token.** `NewFrontdoor` takes a `func() (vault.Secret, error)` instead of a `vault.Secret`, resolved
   inside `do` (`internal/expose/frontdoor.go:355`) rather than at construction. Construction then never
   touches the vault, `Validate` returns a clear "no Frontdoor token yet" that `validate` treats as a warning
   rather than fatal (`cmd/docpreview/main.go:357`), and `Publish` fails per-build with the same message until
   the token exists. This is the smaller change and it matches how the GitHub fix reads: the daemon boots into
   a known-incomplete state and says so.
2. **A swappable exposer**, mirroring `SetClient`. More faithful to the GitHub fix and considerably more
   surface: `d.exposer` is read from `recover`, `republish`, `runPipeline`, the reaper, `Status` and `Close`,
   and the mount decision happens before any of them.

Take (1). It leaves one exposer instance for the process's lifetime and confines the change to
`internal/expose/frontdoor.go` and one line of `buildExposer`.

## Publish, and where the URL comes from

The name is rendered from `exposer.frontdoor.name_template` (`internal/daemon/daemon.go:905`), defaulting to
`{{.Repo.Name}}-{{.Name}}` — repository plus sanitized branch (`internal/config/config.go:435`). It is sent as
the share's `Name`, and a second preview asking for a name another one holds is refused with an error naming the
holder and the template that would separate them (`internal/expose/frontdoor.go:147`).

But the URL is **assigned, not derived**. `Publish` uses whatever the controller returns:

```go
url := JoinURL(created.URL, spec.BaseURL)
```

`internal/expose/frontdoor.go:214`. Whether that URL is a function of the name we sent is unverified — it is
the controller's answer, and the code has no way to predict it. Two consequences follow from that alone. First,
Frontdoor cannot implement `PathExposer`, so it cannot answer `MountPath` before the build; that is fine,
because the base URL comes from `.docpreview.yml` and the host is appended to it, not the other way round
(`internal/expose/expose.go:39`). Second, **every publish creates a new share** — there is no reserved-name
reuse of the kind zrok v2 gets from decoupling names from shares (`internal/expose/zrok.go:35`). So on every
restart, `recover` reaps everything and republishes from artifacts (`internal/daemon/daemon.go:375`), each
republish creates a fresh share, and if the controller does not derive the hostname from the name, the URL moves.
`republish` handles a moved URL — it rewrites the row and publishes a fresh `ready` report, because a comment
pointing at a dead URL is worse than no comment (`internal/daemon/daemon.go:436`, [08-storage.md](08-storage.md)
§ "The URL can move") — so the failure mode is not breakage but comment churn: a restart edits every open pull
request's comment. Under the `local` exposer that stopped happening when URLs became name-derived; under
Frontdoor it is back, conditionally, and the condition is a fact about the product nobody here has checked.

**This is the first thing to verify against a live tenant**, because it decides whether a restart is quiet or
noisy, and because the answer might make a reserved-name flow worth implementing.

### The withdraw-then-create ordering

`Publish` withdraws this preview's own existing share before creating the new one, to free the name
(`internal/expose/frontdoor.go:141`, deliberately, mirroring `internal/expose/zrok.go:141`). That inverts the
property the daemon relies on elsewhere — publish the replacement, *then* close the old publication, so nothing
is ever unserved ([02-exposers.md](02-exposers.md) § "Close matched on key instead of identity"). Under
Frontdoor a rebuild whose share creation fails has already destroyed the working preview, and the failure path
returns an error from the commit phase (`internal/daemon/daemon.go:828`) without touching the database. The row
still says `ready` with a URL that now 404s, the comment still links to it, and nothing self-heals until the
next push or restart. The build correctly reports failed; the *preview* silently is one.

Fixing it properly means creating the new share before deleting the old, which the name uniqueness constraint
may forbid. The cheap correct fix is to write the truth: on a publish failure for a preview that had a live
publication, mark the row `failed` and publish a report saying the preview is gone. `unpublish` already exists
for exactly this consistency argument in the sibling case where the database write fails
(`internal/daemon/daemon.go:866`).

## Reap, orphans, and two daemons in one tenant

`Reap(ctx, keep)` lists shares, skips anything whose `Tag` lacks the `docpreview:` prefix, skips anything whose
publication key is in `keep`, and deletes the rest (`internal/expose/frontdoor.go:284`). The tag is
`docpreview:` + `Spec.Key()`, so a preview's branch share and each of its build shares tag differently — see
[02-exposers.md](02-exposers.md). The prefix is what stops it deleting a share an operator made by hand
(`internal/expose/zrok.go:24-32`). At startup `keep` is nil, so everything tagged is an orphan by definition
(`internal/daemon/daemon.go:463`); on the hourly tick `keep` is every preview id the database holds plus
`<preview>/<build>` for each build row with a recorded URL (`internal/daemon/daemon.go:1770`).

A hard kill leaves the remote shares behind and the local ports with the process. The ports are free the instant
it dies; the shares survive until the next startup's `Reap(ctx, nil)` collects them — which is why that call is
not merely tidiness. If names are unique per tenant, an orphan holding a name also blocks the republish of the
preview that owns it, and **Frontdoor has no equivalent of zrok's `reapName`**, which reclaims a name held by an
unknown share exactly once and retries (`internal/expose/zrok.go:177`). In the normal restart path startup
reaping runs first and the question does not arise. It arises after a partial reap failure, which is logged and
continued past (`internal/daemon/daemon.go:376`).

**Two daemons sharing one Frontdoor account destroy each other.** The tag carries a preview ID and nothing that
identifies the daemon, and `Reap` filters on that prefix alone. Daemon B's startup reap sees every one of daemon
A's shares as prefix-matching and not in B's `keep`, and deletes them — A keeps serving local ports that nothing
routes to, with rows that say `ready`. zrok does not have this problem because its listing is filtered by the
environment's ziti identity (`internal/expose/zrok.go:304`). The fix is the same shape: put an instance
identifier in the tag — `docpreview:<instance>:<previewID>` — and filter on it, with the instance name a config
field defaulting to the hostname. Until that exists it is a documented "one daemon per tenant" rule, and it
should be documented rather than discovered.

## Authorization: previews are as public as the frontend is

`shareRequest` has four fields — name, frontend, target URL, tag (`internal/expose/frontdoor.go:122`) — and
`FrontdoorConfig` has four settings, none of them about access (`internal/config/config.go:338`). docpreview
therefore asserts **nothing** about who may reach a preview. Compare zrok, where the same code path sets a
permission mode, access grants, and optionally an OAuth provider with email patterns
(`internal/expose/zrok.go:155`), all driven from config (`internal/config/config.go:322`).

So: whatever the named frontend's default policy is, that is the policy. If the frontend is open, every preview
is public to anyone with the URL, and the URL is guessable — it is the repository name and the branch. The
runbook sells Frontdoor precisely on IdP-enforced authentication and MFA (`www/docs/runbooks/frontdoor.md:83`),
and that is a truthful description of the product and not of this integration: the protection is entirely a
property of a frontend somebody configured elsewhere, docpreview neither sets it nor checks it, and there is no
per-preview access control of any kind. That is a weaker position than `zrok2` with `open: false`, and much
weaker than `ziti`, which reaches only enrolled identities. It should be stated in
[10-security.md](10-security.md) alongside the `ziti` Host-header gap rather than left to be inferred.

If Frontdoor's API supports per-share access policy, exposing it as `FrontdoorConfig` fields is the fix, and
`Validate` should refuse to run against an open frontend without an explicit `public: true` in the config —
the same "say what you mean" posture that `zrok.Open` takes.

## Failure modes

**A missing or unreadable token is fatal at startup** — `MustGet` fails, `setup` fails, the process exits
(`cmd/docpreview/main.go:312`). That covers both "never stored" and "vault locked", and it is the bug above; it
should boot, warn, and fail per-build until the token exists.

**A revoked or expired token** is caught by `Validate` at startup, which reports the gateway's status line plus
a 2 KiB snippet of the body — capped, because an error page from a misconfigured proxy can be megabytes and none
of it belongs in a log line (`internal/expose/frontdoor.go:113`, `internal/expose/frontdoor.go:367`). If it
expires while the daemon runs, live previews keep serving — the local ports and the remote shares both still
exist — while every publish and every reap fails and is logged. Previews continuing to serve is the right
behaviour; leaving "the exposer has stopped working" only in the log is not, and the dashboard is where it
belongs.

**An unreachable gateway** hits the 30-second client timeout (`internal/expose/frontdoor.go:98`) and then takes
the same paths. That is adequate.

**An agent that cannot dial the bound port is detected by nothing at all.** The share is created, `created.ID`
and `created.URL` come back non-empty, the response guard is satisfied, the publish succeeds, the row says
`ready`, and the comment carries a URL that answers nothing. Every other failure here is loud; this one produces
a green build and a dead link, which is the exact shape the response-validation guard was written to prevent one
layer up. A single fetch of the preview's own public URL after publishing would close it.

**Wrong JSON field names** are rejected at the first publish with an error naming both structs and the file
(`internal/expose/frontdoor.go:192`). That is already the right answer.

**A rate or quota limit** arrives as a non-2xx from `POST /shares`: the publish fails, the port is released, the
build reports failed, and it is indistinguishable from any other API error. Since the way a tenant reaches its
share limit is a reap that has been quietly failing, the fix is upstream of the message — escalate reap failures
rather than logging them, and count shares.

A failed `Publish` costs the build: `runPipeline` returns the error, the build is reported failed, and the
artifacts stay on disk for the next attempt. It does not cost the comment beyond changing its state to failed —
except in the withdraw-then-create case above, where the comment is left pointing at a URL that has been
deleted.

## Restart, and what the operator sees

```
config → store → exposer → scm clients → validate → daemon → recover → listeners → serve
```

`Validate` lists shares before anything binds (`cmd/docpreview/main.go:357`), so a dead token is one clear error
at boot rather than a mystery on the first pull request of the day. Then `recover` reaps every tagged share and
republishes each `ready` row from its artifacts (`internal/daemon/daemon.go:367`). For Frontdoor that means, per
preview: a fresh ephemeral port, a fresh share, possibly a fresh URL, and possibly a comment edit. The log line
is `recovered previews_restored=N jobs_pending=M` (`internal/daemon/daemon.go:402`) plus one `published preview`
per preview carrying the name, URL and share ID (`internal/expose/frontdoor.go:215`). That is enough to audit a
restart from the log, which is more than can be said for the tenant side: there is still no
`docpreview shares list` to compare what the tenant holds against what the database thinks it holds, and the
case that matters is a share created by a daemon whose database was then deleted — nothing claims it and nothing
looks (`TODO.md:105`).

## Operational requirements

An account with API access, a token minted for it, and an enrolled agent that can reach the docpreview host
(`www/docs/runbooks/frontdoor.md:35`). The agent is a separate process, not part of docpreview, and where it
runs is the whole deployment decision:

- **Same host** — `agent_reachable_host: 127.0.0.1` and nothing else is reachable from anywhere. This is the
  arrangement to reach for.
- **Same container** — two processes in one container, or a sidecar sharing a network namespace, which is the
  containerised spelling of the same thing. A sidecar in a different namespace is not: `127.0.0.1` stops
  meaning the same thing on each side, and the port has to be published on a shared interface.
- **Separate host** — a firewall rule scoped to the agent, and the acknowledgement that unauthenticated preview
  content is now on a network interface.

Under systemd, the agent is a second unit and docpreview does not depend on it for startup — `Validate` talks to
the gateway, not to the agent, so a docpreview that starts with the agent down will publish shares nothing can
reach. That is another argument for the reachability check in the failure table.

## Testing

`internal/expose/frontdoor_test.go` runs against an `httptest.Server` standing in for the controller
(`internal/expose/frontdoor_test.go:58`) and covers exactly the wire-format hazard: a response with the wrong
field names is rejected and the error names both structs and the file
(`internal/expose/frontdoor_test.go:77`), a well-formed response yields the base URL appended
(`internal/expose/frontdoor_test.go:103`), an ID-without-URL response has its half-created share deleted
(`internal/expose/frontdoor_test.go:120`), three failed publishes leave nothing in `live`
(`internal/expose/frontdoor_test.go:140`), and `deleteShare("")` refuses to send a request that would address
the collection (`internal/expose/frontdoor_test.go:162`).

What it does not cover, all testable against the same fake:

1. **Reap.** The fake's `GET /shares` returns an empty list unconditionally
   (`internal/expose/frontdoor_test.go:43`). Make it return a mix — docpreview-tagged in `keep`,
   docpreview-tagged not in `keep`, untagged — and assert exactly the middle set is deleted. This is the test
   that would have caught the two-daemons problem by making somebody write down what `keep` means.
2. **The replace-then-close order.** `local` has
   `TestLocalExposerSurvivesTheDaemonsReplaceThenCloseOrder`; Frontdoor's `withdrawEntry` identity check
   (`internal/expose/frontdoor.go:238`) is the same guard on a path where getting it wrong deletes a remote
   share, and it is unguarded by any test.
3. **Name collision.** Two preview IDs asking for one name must be refused with the holder named
   (`internal/expose/frontdoor.go:147`).
4. **The port is real.** Assert that the `targetUrl` the fake received is dialable and serves the handler that
   was published. That is the closest a test can get to the agent-cannot-dial failure, and it also pins the
   `agent_reachable_host`-as-bind-address behaviour.
5. **Ports are released.** The existing test checks `live` is empty; check the ports are actually closed by
   dialing the address recorded from the fake's request body.
6. **A locked vault boots.** Once the lazy-token change lands, the analogue of
   `cmd/docpreview/rewire_test.go`: `exposer.kind: frontdoor`, no token, `setup` succeeds, `Publish` fails with
   a message naming `docpreview vault set frontdoor.api_token`.

None of this needs a live account. What does need one is the wire format, whether the URL is derived from the
name, and whether per-share access policy exists — three questions, one afternoon, and everything above is
written so that the answers land in `shareRequest`, `shareResponse` and `FrontdoorConfig` and nowhere else.

## The order to build it in

1. ~~**Make a locked vault survivable.**~~ **Done.** `NewFrontdoor` takes an `expose.TokenFunc` resolved per
   request rather than the token itself, so wiring no longer touches the vault; `validate` downgrades a
   `vault.ErrLocked` failure to a warning; and `revalidateExposer` re-runs the check from the `rearm` callback on
   unlock and on the token being stored. Resolving per request rather than caching means rotating the token from
   the setup page takes effect without a restart. Covered by three tests in `internal/expose/frontdoor_test.go`,
   and verified by hand: `exposer.kind: frontdoor` with a locked vault now answers `/healthz`, and storing the
   token moves the validation error from "no such secret" to a network failure — which is the token being read
   from the vault at request time.
2. **Verify the wire format against a live tenant.** `POST /shares`, `GET /shares`, `DELETE /shares/{id}`, the
   field names, and the tag round-tripping through the listing. Fix `shareRequest`/`shareResponse`. This is
   step 2 and not step 1 only because step 1 is what lets you do it from the setup page.
3. **Answer the URL-stability question** and record it in this document. If the URL is name-derived, say so and
   the restart story is quiet. If it is not, either implement name reuse or accept comment churn on restart,
   explicitly.
4. **Write the Reap test with a non-empty listing**, then fix the two-daemon collision by putting an instance
   identifier in the tag and filtering on it.
5. **Add a reachability check.** After a successful publish, fetch the preview's own public URL once and fail
   the publish if it does not answer. This is the only defence against the silent failure, and it also proves
   the agent is up.
6. **Validate `agent_reachable_host`** in `config.validate`: reject wildcards, and warn when it is neither
   loopback nor resolvable locally.
7. **Fix the withdraw-then-create consequence** — on a publish failure for a preview that was live, write
   `failed` and report it, so the comment stops pointing at a deleted share.
8. **Settle authorization**, in code if the API allows per-share policy, and in
   [10-security.md](10-security.md) either way.
9. **Then switch a real repository's `exposer.kind` to `frontdoor`** and push three commits in a minute, which
   is the same smoke test [11-github-setup-state.md](11-github-setup-state.md) reserves for zrok.

## Not verified

Everything in this section is inference or recollection, not something read out of this repository.

- **Frontdoor is a NetFoundry product and there is no copy of its API specification in this tree.** Every claim
  about its behaviour — that share creation assigns rather than derives a URL, that names are unique per tenant,
  that a frontend has a default access policy, that shares are quota-limited, that per-share access policy is
  expressible at all — is a guess about the product, not a reading of code. No endpoint, parameter or flag
  beyond the three paths already in `internal/expose/frontdoor.go` is asserted anywhere above, and none should
  be invented. The endpoint paths and field names that *are* in the code are themselves flagged unverified by
  their own author (`internal/expose/frontdoor.go:38`).
- That the agent runs as a separate process on Linux, and the co-located/sidecar/separate-host taxonomy, comes
  from `www/docs/runbooks/frontdoor.md` rather than from anything executable.
- The claim that an orphaned share can block a republish depends on names being unique per tenant. If they are
  not, the name-collision refusal in `Publish` is local bookkeeping only and `reapName`'s absence costs nothing.
- Port-exhaustion sizing is arithmetic about ephemeral ranges, not a measurement. Nobody has run a hundred
  concurrent Frontdoor previews.
- The assertion that a remote agent means unauthenticated content on a LAN interface follows from
  `net.Listen` on the configured host and from docpreview having no authentication on preview content. It has
  not been observed on a real deployment because there has not been one.
