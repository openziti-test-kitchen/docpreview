---
id: frontdoor
title: Runbook — NetFoundry Frontdoor
sidebar_position: 3
---

# Runbook: NetFoundry Frontdoor

[Frontdoor](https://netfoundry.io/docs/frontdoor/intro/) is NetFoundry's zero-trust ingress: a globally
distributed hardened frontend that fronts private HTTP services with no firewall changes, no port forwarding,
and no client software for the people reading your previews.

For docpreview it is a drop-in alternative to zrok — same `Exposer` interface, one line of config — that buys
you three things zrok alone does not: a WAF, IdP-enforced authentication with MFA, and central management of
who can reach what.

:::warning Implementation status

The Frontdoor exposer's HTTP wire format has not been verified against a live tenant. Read
[Exposers → Status](../exposers.md#status) before relying on it. The lifecycle logic is shared with the other
exposers and is exercised by the same tests; it is the endpoint paths and JSON field names that need checking.

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

Frontdoor's REST API lives at `https://gateway.production.netfoundry.io/frontdoor` and uses bearer tokens.
See [the API guides](https://netfoundry.io/docs/frontdoor/reference/api-guides/auth-providers/) for how to
mint one.

```powershell
"paste-the-token" | docpreview vault set frontdoor.api_token
```

## Step 3 — configure

```yaml
exposer:
  kind: frontdoor
  frontdoor:
    api_base: https://gateway.production.netfoundry.io/frontdoor
    frontend: public
    agent_reachable_host: 127.0.0.1
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

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
