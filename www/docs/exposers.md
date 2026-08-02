---
id: exposers
title: Exposers
sidebar_position: 5
---

# Exposers

An **exposer** turns a built preview into a URL somebody can open. It is the only part of docpreview that
knows anything about how traffic reaches you, which is what makes the same binary work on a laptop, on a VM,
and inside a corporate network with no inbound ports.

Pick **one**. Either on the *Exposer configuration* panel at `/secrets`, which stores the choice and applies it at
the next restart, or in the server config:

```yaml
exposer:
  kind: zrok2   # or: frontdoor, ziti, local
```

A stored choice wins over the file, so a value set from the dashboard is the one in force. The config file is never
rewritten — its comments are the part that survives being copied to another machine.

:::note One exposer, one URL per preview

`exposer.kind` is a single value: a preview is published one way and the pull request comment carries one link. So
enabling an exposer turns off whichever one was on, and at the next restart every preview is republished at a new
address and every open comment is rewritten to match. The dashboard asks before doing it.

Publishing through several at once, and choosing per project, is designed but not built — see
[docs/design/21-multi-exposer.md](https://github.com/openziti-test-kitchen/docpreview/blob/main/docs/design/21-multi-exposer.md).

:::

| Kind | Reachable by | Public surface |
|---|---|---|
| [`zrok2`](#zrok2) | anyone with the link, or an OAuth-gated subset | yes |
| [`frontdoor`](#frontdoor) | whoever your IdP admits | yes |
| [`ziti`](#ziti) | only an enrolled tunneler | **none** |
| [`local`](#local) | you | loopback |

## `local`

Binds an ephemeral loopback port and serves there. The comment gets a `http://127.0.0.1:54321/` link.

Useless for sharing and perfect for everything else: you can exercise the whole clone → detect → build →
comment path without an account anywhere, and when the zrok path misbehaves you have a reference to compare
against. It is also the shortest implementation, so it is the one to read first.

## `zrok2`

The default, and the one to use.

### Why v2 specifically

zrok v2 replaced v1's reserved-share model with **namespaces and names**. In v1, a stable public address meant
a reserved share, and the address was tied to that specific share — rebuild it and the URL churned. In v2,
names live in a namespace independently of any share:

> In v2.0, names are decoupled from shares. You create names, and then you attach those names to shares.

That is precisely the primitive a preview system needs. docpreview attaches the name `my-feature-branch` to a
**fresh share on every rebuild**, and the URL a reviewer bookmarked on Monday still resolves on Thursday.
Without it, the "edit one comment" behaviour would be pointless, because the link inside the comment would
change every time anyway.

### How it is wired

docpreview links the zrok Go SDK directly — `github.com/openziti/zrok/v2` — rather than shelling out to the
CLI. Three calls:

```go
shr, _ := sdk.CreateShare(root, &sdk.ShareRequest{
    ShareMode:      sdk.PublicShareMode,
    BackendMode:    sdk.ProxyBackendMode,
    NameSelections: []sdk.NameSelection{{NamespaceToken: ns, Name: "my-feature-branch"}},
    Target:         "docpreview:" + previewID,
})

listener, _ := sdk.NewListener(shr.Token, root)   // a listener on the OpenZiti overlay
http.Serve(listener, previewSite)                 // serve the preview into it
```

`sdk.NewListener` is the part that makes this better than running `zrok2 share public` as a subprocess. It
returns a `net.Listener` on the overlay, so the preview is served **directly into the mesh** — no local port,
no loopback hop, no reverse proxy, nothing bound on the host. On a machine running twenty previews, `netstat`
shows one listener: the webhook ingress.

`Target` is stamped with the preview ID. Nothing dials it — we serve the traffic ourselves — so it doubles as
an ownership tag, and zrok's share listing supports filtering on it. That is how the reaper distinguishes a
share docpreview created from one you made by hand in another terminal.

### The name is a separate object, and it has a quota

A name is registered before the share, left untouched by every rebuild, and deleted when the preview is torn down.
Both halves matter to an operator: the first is what makes a reviewer's bookmark keep working, and the second is
what stops the account's reserved-name allowance filling up one push at a time. See
[what docpreview does with names](./runbooks/zrok2.md#what-docpreview-does-with-names).

### Configuration

```yaml
exposer:
  kind: zrok2
  zrok2:
    namespace: ""                 # blank uses the environment's default namespace
    name_template: "{{.Repo.Name}}-{{.Name}}"    # {{.Name}} is the sanitized branch
    open: true                    # anyone with the link; false restricts to access_grants
    access_grants: []             # zrok accounts allowed when open is false
    oauth_provider: ""            # "google" or "github" to gate previews behind an IdP
    oauth_email_domains: []
```

`open: true` means anyone with the URL can read the preview. For unreleased documentation, set `open: false`
and list reviewers in `access_grants`, or put an OAuth provider in front.

See the [zrok runbook](./runbooks/zrok2.md) for setup.

## `frontdoor`

NetFoundry Frontdoor is the same idea, productized: an enrolled **agent** on the private side, a hardened
globally-distributed **frontend**, and **shares** mapping a public route onto a private target — with a WAF
and IdP/MFA enforcement in front, which is the part you cannot get from zrok alone.

Because it sits behind the same `Exposer` interface, moving from zrok to Frontdoor is one line:

```yaml
exposer:
  kind: frontdoor
  frontdoor:
    api_base: https://gateway.production.netfoundry.io/frontdoor
    frontend: public
    agent_reachable_host: 127.0.0.1
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

Nothing above the exposer changes — not the queue, not the builder, not the comment format.

### The one structural difference

zrok hands us a listener and we serve into it. Frontdoor's agent dials **out** to a target URL, so a Frontdoor
preview has to bind a real TCP port the agent can connect to. That is what `agent_reachable_host` is for: if
the agent runs on the same host, leave it at `127.0.0.1`; if it runs elsewhere, this must be an address it can
actually reach.

### Status

:::warning Unverified against a live tenant

The Frontdoor implementation's endpoint paths and JSON field names follow the documented API convention —
`/{resource}` under `https://gateway.production.netfoundry.io/frontdoor`, bearer auth, JSON bodies — but have
not been exercised against a real Frontdoor instance, because there is not one here yet.

Everything above the wire format — lifecycle, reaping, naming, idempotency — is the same code the other
exposers use. If the field names turn out to be different, the fix is confined to two structs,
`shareRequest` and `shareResponse` in `internal/expose/frontdoor.go`.

:::

See the [Frontdoor runbook](./runbooks/frontdoor.md).

## `ziti`

Publishes on an OpenZiti overlay, reachable **only** from a machine running a tunneler with an enrolled
identity. The one exposer with no public surface at all: the hostname does not resolve, the address is not
routable, and the service cannot be dialed without a right granted on the controller.

```yaml
exposer:
  kind: ziti
  ziti:
    identity_file: /etc/docpreview/docpreview.json   # docpreview's enrolled identity
    service: docpreview-svc
    domain: docpreview.ziti
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

A reviewer opens `http://my-branch.docpreview.ziti/` and it works. Anyone else — including the same person
with the tunneler switched off — gets NXDOMAIN. For unreleased documentation that is a different posture from
"public URL, unguessable name".

### The one structural difference

Every other exposer creates a listener per preview. This one creates **a single wildcard service** covering
every preview that will ever exist, and separates requests by `Host` header — which works because the tunneler
is a layer-4 proxy, so the browser's `Host` arrives verbatim.

That means publishing costs nothing remote. It is a map insert. Zero management-API objects per pull request,
against four for a service-per-PR model that would also churn DNS rules on every connected tunneler each time
someone pushes.

The interface absorbs this without changing: `Context.Listen` returns an `edge.Listener` embedding
`net.Listener`, the same shape the zrok implementation already serves into.

:::warning One docpreview per service

Binding creates a ziti *terminator*. Two instances binding the same service create two, and the router
load-balances between them — each holding a disjoint routing table, so every preview works about half the
time and 404s the rest. It presents as a flaky network rather than a configuration error.

Give a second instance its own service and domain. (Found the hard way: a `docpreview preview` left running
in another terminal made the integration tests fail intermittently.)

:::

Because the map is the only registry, two previews rendering to the same hostname label is refused rather
than silently resolved — the default name template is the branch alone, so two repositories both with a
`main` branch collide. The error names `{{.Repo.Name}}-{{.Name}}` as the fix.

### Status

The exposer works and has been exercised end to end against a real controller, a real edge router, and a real
Ziti Desktop Edge install. Provisioning is no longer by hand: `docpreview configure ziti` creates every controller
object, both identities and the config file — see [the CLI reference](./reference/cli.md#configure-ziti) and
[Quickstart](./quickstart.md), which walks it from nothing in four commands.

What is still missing is per-identity authorization. One wildcard service serves every preview and requests are
separated by the client-supplied `Host` header, so anyone holding the reader role attribute can reach every preview
by sending any hostname. See [Tunneler-only previews](./future/ziti-native-previews.md) for the design and the
research.

## Contract for implementations

If you write your own:

- **`Publish` must be idempotent on `Spec.Name`.** Publishing the same name twice must reuse or replace,
  never fail. Someone pushing three commits in a minute is the normal case.
- **`Validate` must reach the remote.** Checking local state is not enough. A zrok environment can be present
  and enabled locally while the account token has been revoked server-side, which then surfaces as an opaque
  401 on the first pull request of the day rather than as a startup error.
- **`Reap` must tag its own work.** At startup it is called with an empty keep-set, because nothing this
  process published can have survived it. Deleting resources you did not create is a way to ruin somebody's
  afternoon.

## Sources

- [Introducing zrok v2.0](https://blog.openziti.io/introducing-zrok-v2-0)
- [zrok public shares](https://docs.zrok.io/docs/concepts/sharing-public/)
- [Frontdoor overview](https://netfoundry.io/docs/frontdoor/intro/)
