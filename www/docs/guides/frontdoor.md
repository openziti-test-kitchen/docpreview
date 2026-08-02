---
id: frontdoor
title: Publish previews with NetFoundry Frontdoor
sidebar_position: 5
---

# Publish previews with NetFoundry Frontdoor

[Frontdoor](https://netfoundry.io/docs/frontdoor/intro/) is NetFoundry's zero-trust ingress: a globally
distributed hardened frontend that fronts private HTTP services with no firewall changes, no port forwarding,
and no client software for the people reading your previews.

For docpreview it is a drop-in alternative to zrok — same `Exposer` interface, one line of config — that buys
you three things zrok alone does not: a WAF, IdP-enforced authentication with MFA, and central management of
who can reach what.

:::warning Implementation status

The Frontdoor exposer's HTTP wire format has not been verified against a live tenant. Read
[Exposers → Status](../exposers.md#status) before relying on it. The lifecycle logic is shared with the other
exposers and is exercised by the same tests. It is the endpoint paths and JSON field names that need checking.

:::

## The model

Three pieces, and they map cleanly onto zrok's:

| Frontdoor | zrok | What it is |
|---|---|---|
| **Agent** | environment | A lightweight connector on your private network |
| **Frontend** | public frontend | The hardened public entry point |
| **Share** | share | A mapping from a public route to a private target |

## Step 1 — install and enroll an agent

Follow [Get started with Frontdoor](https://netfoundry.io/docs/frontdoor/frontdoor-get-started/). The agent
runs on a Linux VM or container that can reach the host running docpreview.

The simplest arrangement by far is the agent on the **same host** as docpreview. Then
`agent_reachable_host` stays `127.0.0.1` and nothing else has to be reachable from anywhere.

## Step 2 — get an API token

Frontdoor's REST API uses bearer tokens, minted from an API account at
[nfconsole.io](https://nfconsole.io) under **Organization → Manage API Account** and exchanged for a token with
`grant_type=client_credentials`. See
[platform authentication](https://netfoundry.io/docs/platform/api-guides/authentication).

```powershell
"paste-the-token" | docpreview vault set frontdoor.api_token
```

:::warning These tokens expire

docpreview stores the exchanged token, not the credentials that mint it, so it works until the token expires and
then every publish fails with a 401. Re-run the exchange and store the new value. Storing the client id and
password so the daemon can refresh on its own is not built yet.

:::

## Step 3 — configure

```yaml
exposer:
  kind: frontdoor
  frontdoor:
    api_base: https://gateway.production.netfoundry.io/frontdoor/<frontdoorId>
    frontend: bMTHPrtQ
    env_z_id: ijcrWb-ZOq
    agent_reachable_host: 127.0.0.1
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

Three of those five are easy to get wrong, and all three fail in ways that do not name themselves:

| Field | |
|---|---|
| `api_base` | **Must end in `/frontdoor/<frontdoorId>`.** The routes are unversioned and carry tenancy in that segment — without it every call 404s. Startup refuses an `api_base` ending in a bare `/frontdoor`. |
| `frontend` | **A frontend ID**, like `bMTHPrtQ` — not a name like `public`. The API field is `frontendIds` — a name there is accepted and matches nothing, producing a share that serves nothing. |
| `env_z_id` | The **enrolled agent's ziti identity**, from the Frontdoor console. Required on every share. Startup warns once when it is missing, because without it no publish can succeed. |

`agent_reachable_host` is the one field with a real constraint. Frontdoor works the opposite way round from
zrok: instead of handing docpreview a listener on an overlay, its agent **dials out** to a target URL. So a
Frontdoor preview binds a real TCP port, and this value must be an address the agent can actually connect to.

- Agent on the same host → `127.0.0.1`. Nothing is exposed beyond loopback.
- Agent elsewhere → the LAN address or hostname of the docpreview machine. Firewall it to the agent.

## Step 4 — verify

```powershell
docpreview doctor
```

The Frontdoor check lists shares through the gateway, which confirms the token is valid and the API base is
right.

:::danger Frontdoor has never been exercised against a live tenant

Everything above the wire format — the lifecycle, reaping, naming, retries, the collision rule — is covered by the
same tests as the working zrok exposer. The wire format itself was written from Frontdoor's documentation and
corrected against it once. No request in this guide has been observed succeeding.

It is instrumented to fail loudly rather than quietly: a create whose response cannot be read is an error naming
the two structs to fix, a listing that decodes to nothing errors rather than reporting an empty account, and a
share that comes back without the tag docpreview sent logs an error saying so. Expect the first publish to teach
you something, and read the daemon log rather than the dashboard when it does.

The one known gap: shares carry no tag field, so `Reap` cannot recognise its own work and will delete nothing.
That leaks one share per preview per restart until ownership moves into the share name. Use zrok for anything you
depend on today.

:::

## Why previews might belong behind Frontdoor rather than zrok

zrok's OAuth support gates a preview behind Google or GitHub. That is often enough. Frontdoor is the answer
when it is not:

- **Your own IdP.** Okta, Entra, whatever the rest of the organization already uses, with the group
  memberships already in it.
- **MFA enforcement.** Not optional, not per-user, enforced at the frontend.
- **A WAF in front of untrusted content.** A preview renders HTML that anyone who can open a pull request
  wrote. docpreview sets `nosniff` and refuses non-read methods, but that is hardening, not a WAF.
- **Central audit.** One place that knows who opened which preview, alongside every other private service.

The last one is usually the real reason. "Documentation previews" is a small enough thing that nobody wants a
separate access-control story for it.

## Related

- [Exposers](../exposers.md)
- [Security model](../reference/security.md)
