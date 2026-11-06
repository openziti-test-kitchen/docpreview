---
id: exposers
title: Exposers
sidebar_position: 5
---

# Exposers

An **exposer** turns a built preview into a URL somebody can open. It is the only part of NetFoundry Preview that
knows how traffic reaches you. That is why the same binary works on a laptop, on a VM, and inside a corporate
network with no inbound ports.

Pick **one**. Set it on the *Exposer configuration* panel at `/secrets`, which stores the choice and applies it at
the next restart, or in the server config:

```yaml
exposer:
  kind: zrok2   # or: frontdoor, ziti, local
```

A stored choice wins over the file, so a value set from the dashboard is the one in force. The config file is never
rewritten, because its comments are the part worth copying to another machine.

:::note One exposer, one URL per preview

`exposer.kind` is a single value. A preview is published one way, and the pull request comment carries one link.
Enabling an exposer turns off the one that was on. At the next restart every preview is republished at a new
address and every open comment is rewritten to match. The dashboard asks before doing it.

Publishing through several at once, and choosing per project, is designed but not built. See
[docs/design/21-multi-exposer.md](https://github.com/openziti-test-kitchen/docpreview/blob/main/docs/design/21-multi-exposer.md).

:::

| Kind | Reachable by | Public surface |
|---|---|---|
| [`zrok2`](#zrok2) | anyone with the link, or an OAuth-gated subset | yes |
| [`frontdoor`](#frontdoor) | whoever your IdP admits | yes |
| [`ziti`](#ziti) | only an enrolled tunneler | **none** |
| [`local`](#local) | you | loopback |

## `local`

Serves each preview under `/preview/<name>/` on the daemon's own listener, the same address as the dashboard. It
binds no port of its own. The comment gets a link like `http://127.0.0.1:8471/preview/docs-my-branch/`.

Useless for sharing and perfect for everything else. You can exercise the whole clone, detect, build and comment
path without an account anywhere. When the zrok path misbehaves, you have a reference to compare against. It is
also the shortest implementation, so read it first.

## `zrok2`

The default, and the one to use.

### Why v2 specifically

zrok v2 replaced v1's reserved-share model with **namespaces and names**. In v1 a stable public address meant a
reserved share, and the address was tied to that share: rebuild it and the URL changed. In v2, names live in a
namespace apart from any share:

> In v2.0, names are decoupled from shares. You create names, and then you attach those names to shares.

That is the primitive a preview system needs. A preview gets one share, bound to a name like `docs-my-branch`, and
keeps it. A rebuild of the branch serves the new build on that same share by swapping the handler behind it, so a
push costs no zrok calls at all. A restart stops serving but leaves the share on the controller, and the next start
binds a new listener to it. The URL a reviewer bookmarked on Monday still resolves on Thursday, and the comment's
link never has to change.

### Share creations are rationed

zrok answers 429 when an account creates shares too fast, and does not say what its limit is. So every share
creation on a machine goes through one gate: at most **15 per 10 minutes** per zrok environment. Two 429s in a row
mean 10 minutes of silence, because retrying inside a refusal only extends it. The daemon, `webhook-only` and
`dashboard-only` all spend from the same budget, which lives in a file in the zrok environment's directory.

A share that has to wait says so in the build log, with the time of the next slot. The dashboard footer shows
`zrok shares N/15`, and `/status` reports the same under `zrok_shares`.

### One share per branch, unless you ask for more

`exposer.build_shares: true` gives every successful build a second share of its own, named after the branch plus
the short commit and pinned to that commit. The dashboard then offers **Open build ↗** beside the build picker.
It is off by default, because each of those is a share creation, and one per commit is what runs an account into
the rate limit. Turn it off again and the next start reaps the per-build shares already published.

### How it is wired

The daemon links the zrok Go SDK, `github.com/openziti/zrok/v2`, instead of shelling out to the CLI. Three calls:

```go
shr, _ := sdk.CreateShare(root, &sdk.ShareRequest{
    ShareMode:      sdk.PublicShareMode,
    BackendMode:    sdk.ProxyBackendMode,
    NameSelections: []sdk.NameSelection{{NamespaceToken: ns, Name: "docs-my-branch"}},
    Target:         "docpreview:" + previewID,
})

listener, _ := sdk.NewListener(shr.Token, root)   // a listener on the OpenZiti overlay
http.Serve(listener, previewSite)                 // serve the preview into it
```

`sdk.NewListener` is what makes this better than running `zrok2 share public` as a subprocess. It returns a
`net.Listener` on the overlay, so the preview is served **straight into the mesh**: no local port, no loopback
hop, no reverse proxy. On a machine running twenty previews, `netstat` shows one listener, the daemon's own.

`Target` carries the preview ID. Nothing dials it, because the daemon serves the traffic itself, so it doubles as
an ownership tag, and zrok's share listing can filter on it. The reaper uses it to tell a share the daemon created
from one you made by hand in another terminal. Startup uses the same listing to find the shares it can adopt.

### The name is a separate object, and it has a quota

A name is registered before its share, left alone by every rebuild, and deleted when the preview is torn down. The
first keeps a reviewer's bookmark working. The second stops the account's reserved-name allowance from filling up.
See [what docpreview does with names](./guides/zrok2.md#what-docpreview-does-with-names).

### Configuration

```yaml
exposer:
  kind: zrok2
  build_shares: false             # true adds a share per build, pinned to its commit
  zrok2:
    namespace: ""                 # blank uses the environment's default namespace
    name_template: "{{.Repo.Name}}-{{.Name}}"    # {{.Name}} is the sanitized branch
    open: true                    # anyone with the link; false restricts to access_grants
    access_grants: []             # zrok accounts allowed when open is false
    oauth_provider: ""            # "google" or "github" to gate previews behind an IdP
    oauth_email_domains: []
```

`open: true` means anyone with the URL can read the preview. For unreleased content, set `open: false` and list
reviewers in `access_grants`, or put an OAuth provider in front.

See the [zrok guide](./guides/zrok2.md) for setup.

## `frontdoor`

:::note This is NetFoundry Frontdoor, not "zrok frontdoor"

Two products carry the name, and they are unrelated. This exposer means
[NetFoundry Frontdoor](https://netfoundry.io/docs/frontdoor/intro/), the commercial zero-trust ingress administered
at [nfconsole.io](https://nfconsole.io). zrok separately documents a feature it calls zrok frontdoor.

Searching for the bare word finds the zrok one first. Nothing it says applies here, and the account it has you
create issues none of the values this exposer needs.

:::

NetFoundry Frontdoor is the same idea as zrok, productized. An enrolled **agent** sits on the private side, a
hardened, globally distributed **frontend** faces the internet, and **shares** map a public route onto a private
target. It adds a WAF and IdP/MFA enforcement in front, which zrok alone does not give you.

It sits behind the same `Exposer` interface, so the switch is a config change:

```yaml
exposer:
  kind: frontdoor
  frontdoor:
    api_base: https://gateway.production.netfoundry.io/frontdoor/<frontdoorId>
    frontend: bMTHPrtQ            # a frontend ID, not a name like "public"
    env_z_id: ijcrWb-ZOq          # the enrolled agent's ziti identity
    agent_reachable_host: 127.0.0.1
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

It also needs an API token in the vault as `frontdoor.api_token`. The [Frontdoor guide](./guides/frontdoor.md) explains where each
value comes from. Nothing above the exposer changes: not the queue, not the builder, not the comment format.

### The one structural difference

zrok hands over a listener and the daemon serves into it. Frontdoor's agent dials **out** to a target URL, so a
Frontdoor preview binds a real TCP port the agent can connect to. That is what `agent_reachable_host` is for. If the
agent runs on the same host, leave it at `127.0.0.1`. If it runs elsewhere, set an address the agent can reach.

### Status

:::warning Unverified against a live tenant

The Frontdoor implementation's endpoint paths and JSON field names follow the documented API convention, routes
under `https://gateway.production.netfoundry.io/frontdoor/<frontdoorId>` with bearer auth and JSON bodies. They have
not been exercised against a real Frontdoor instance, because there is not one here yet.

Everything above the wire format (lifecycle, reaping, naming, idempotency) is the same code the other exposers use.
If the field names turn out different, the fix stays inside `internal/expose/frontdoor.go`: the `shareRequest` and
`shareResponse` structs and the share-listing decoder.

:::

## `ziti`

Publishes on an OpenZiti overlay, reachable **only** from a machine running a tunneler with an enrolled identity.
It is the one exposer with no public surface at all. The hostname does not resolve, the address is not routable,
and the service cannot be dialed without a right granted on the controller.

```yaml
exposer:
  kind: ziti
  ziti:
    identity_file: /etc/docpreview/docpreview.json   # docpreview's enrolled identity
    service: docpreview-svc
    domain: docpreview.ziti
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

A reviewer opens `http://docs-my-branch.docpreview.ziti/` and it works. Anyone else, including the same person with
the tunneler switched off, gets NXDOMAIN. For unreleased content that is a different posture from "public URL,
unguessable name".

### The one structural difference

zrok creates a share and a listener per preview. This one creates **a single wildcard service** covering every
preview that will ever exist, and separates requests by `Host` header. That works because the tunneler is a
layer-4 proxy, so the browser's `Host` arrives verbatim.

So publishing costs nothing remote. It is a map insert. A pull request adds zero management-API objects, against
four for a service-per-PR model, which would also churn DNS rules on every connected tunneler at each push.

The interface absorbs this unchanged: `Context.Listen` returns an `edge.Listener` embedding `net.Listener`, the
same shape the zrok implementation serves into.

:::warning One docpreview per service

Binding creates a ziti *terminator*. Two instances binding the same service create two, and the router balances
load between them. Each holds a different routing table, so every preview works about half the time and 404s the
rest. It looks like a flaky network, not a configuration error.

Give a second instance its own service and domain. A `docpreview preview` left running in another terminal is
enough to make the integration tests fail now and then.

:::

The map is the only registry, so two previews rendering to the same hostname label are refused, not quietly
renamed. A collision means `name_template` is too loose for your repositories. The default,
`{{.Repo.Name}}-{{.Name}}`, still collides for two owners with a same-named repository and branch. Add
`{{.Repo.Owner}}` to separate them.

### Status

The exposer works and has been exercised end to end against a real controller, a real edge router, and a real Ziti
Desktop Edge install. `docpreview configure ziti` creates every controller object, both identities and the config
file. See [the CLI reference](./reference/cli.md#configure-ziti) and the [Quickstart](./quickstart.md), which walks
it from nothing in four commands.

Per-identity authorization is still missing. One wildcard service serves every preview, and requests are separated
by the client-supplied `Host` header. Anyone holding the reader role attribute can reach every preview by sending
any hostname. See
[Tunneler-only previews](https://github.com/openziti-test-kitchen/docpreview/blob/main/docs/design/ziti-native-previews.md)
for the design and the research.

## Contract for implementations

If you write your own:

- **`Publish` must be idempotent on `Spec.Name`.** Publishing the same name twice must reuse or replace, never fail.
  Someone pushing three commits in a minute is the normal case.
- **`Validate` must reach the remote.** Local state is not enough. A zrok environment can be present and enabled
  locally while its account token has been revoked on the server. That shows up as an opaque 401 on the first pull
  request of the day instead of a startup error.
- **`Reap` must tag its own work.** At startup it gets the set of publications the database still claims, and
  deletes the rest of what it owns. Deleting resources you did not create ruins somebody's afternoon.

## Sources

- [Introducing zrok v2.0](https://blog.openziti.io/introducing-zrok-v2-0)
- [zrok public shares](https://netfoundry.io/docs/zrok/concepts/sharing-public/)
- [Frontdoor overview](https://netfoundry.io/docs/frontdoor/intro/)
