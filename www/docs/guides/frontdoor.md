---
id: frontdoor
title: Publish previews with NetFoundry Frontdoor
sidebar_position: 5
---

# Publish previews with NetFoundry Frontdoor

[NetFoundry Frontdoor](https://netfoundry.io/docs/frontdoor/intro/) is NetFoundry's zero-trust ingress. It is a
globally distributed, hardened frontend for private HTTP services, with no firewall changes, no port forwarding,
and no client software for the people reading your previews.

For NetFoundry Preview it is an alternative to zrok that plugs into the same `Exposer` interface with one line of
config. It adds three things zrok alone does not have: a WAF, IdP-enforced authentication with MFA, and central
control of who can reach what.

:::danger Check which Frontdoor you are reading about

This page is about **NetFoundry Frontdoor**, administered at [nfconsole.io](https://nfconsole.io). zrok has an
unrelated feature also called frontdoor, and a search for the bare word tends to find that one first.

Everything below assumes the NetFoundry product. The zrok feature has its own account, console and free tier, and
none of them issue the values Step 2 and Step 3 ask for. Following the wrong instructions costs you a sign-up and
leaves you with a configuration that can't publish.

:::

:::warning Implementation status

Nobody has checked the Frontdoor exposer's HTTP wire format against a live tenant yet. Read
[Exposers → Status](../exposers.md#status) before relying on it. Its lifecycle logic has its own tests, which mirror
the zrok exposer's cases against a fake gateway. The endpoint paths and JSON field names are what still need
checking.

:::

## The model

There are three pieces, and each has a zrok counterpart:

| Frontdoor | zrok | What it is |
|---|---|---|
| **Agent** | environment | A lightweight connector on your private network |
| **Frontend** | public frontend | The hardened public entry point |
| **Share** | share | A mapping from a public route to a private target |

## Step 1: Install and enroll an agent

Follow [Get started with Frontdoor](https://netfoundry.io/docs/frontdoor/frontdoor-get-started/). The agent runs on a
Linux VM or container that can reach the host running the daemon.

The simplest setup by far puts the agent on the **same host** as the daemon. Then `agent_reachable_host` stays
`127.0.0.1` and nothing else has to be reachable from anywhere.

## Step 2: Get an API token

Frontdoor's REST API uses bearer tokens. You mint them from an API account at [nfconsole.io](https://nfconsole.io)
under **Organization → Manage API Account**, and exchange the account's credentials for a token with
`grant_type=client_credentials`. See
[platform authentication](https://netfoundry.io/docs/platform/api-guides/authentication).

```powershell
"paste-the-token" | nfpreview vault set frontdoor.api_token
```

:::warning These tokens expire

The vault stores the exchanged token, not the credentials that mint it. It works until the token expires, and then
every publish fails with a 401. Run the exchange again and store the new value. Refreshing on its own from a stored
client id and password is not built yet.

:::

## Step 3: Configure

```yaml
exposer:
  kind: frontdoor
  frontdoor:
    api_base: https://gateway.production.netfoundry.io/frontdoor/<frontdoorId>
    frontend: <frontend-id>
    env_z_id: <agent-ziti-identity>
    agent_reachable_host: 127.0.0.1
    name_template: "{{.Repo.Name}}-{{.Name}}"
```

Three of those five are easy to get wrong, and none of them fail with an error that names the cause:

| Field | |
|---|---|
| `api_base` | **Must end in `/frontdoor/<frontdoorId>`.** The routes are unversioned and carry the tenancy in that segment, so without it every call answers 404. Startup refuses an `api_base` that ends in a bare `/frontdoor`. |
| `frontend` | **A frontend ID** such as `bMTHPrtQ`, not a name such as `public`. The API field is `frontendIds`. A name there is accepted and matches nothing, so you get a share that serves nothing. |
| `env_z_id` | The **enrolled agent's ziti identity**, from the Frontdoor console. Every share needs it. Startup warns once when it is missing, because no publish can succeed without it. |

`agent_reachable_host` is the one field with a real constraint. Frontdoor works the other way round from zrok.
zrok hands the daemon a listener on an overlay, while Frontdoor's agent **dials out** to a target URL. So a
Frontdoor preview binds a real TCP port, and this value must be an address the agent can connect to.

- Agent on the same host: `127.0.0.1`. Nothing is exposed beyond loopback.
- Agent elsewhere: the LAN address or hostname of the daemon's machine. Firewall that port to the agent.

## Step 4: Verify

```powershell
nfpreview doctor
```

The Frontdoor check lists shares through the gateway. A pass confirms the token is valid and the API base is right.

:::danger Frontdoor has never run against a live tenant

The code above the wire format (lifecycle, reaping, naming, retries, the collision rule) has tests that mirror the
zrok exposer's. The wire format itself was written from Frontdoor's documentation and checked against it once. Nobody
has seen a request in this guide succeed.

It is built to fail loudly. A create whose response can't be read is an error that names the two structs to fix. A
listing under an envelope key the exposer does not recognise, or whose shares have no ids, is an error rather than
an empty account. A share that comes back without the tag the daemon sent logs an error saying so. Expect the first
publish to teach you something, and read the daemon log, not the dashboard, when it does.

There is one known gap. Shares carry no tag field, so `Reap` can't recognise its own shares and deletes nothing. A
clean shutdown deletes every share the daemon published, so a normal restart leaks nothing. A crash or a kill skips
that, and leaves the shares behind for good, one per preview, until ownership moves into the share name. Use zrok
for anything you depend on today.

:::

## Why previews might belong behind Frontdoor rather than zrok

zrok's OAuth support puts a preview behind a Google or GitHub login, and that is often enough. Frontdoor is for when
it is not:

- **Your own IdP.** Okta, Entra, or whatever the rest of the organization already uses, with the group memberships
  already in it.
- **MFA enforcement.** Required for everyone and enforced at the frontend.
- **A WAF in front of untrusted content.** A preview renders HTML written by anyone who can open a pull request. The
  daemon sets `nosniff` and refuses non-read methods, which is hardening, not a WAF.
- **Central audit.** One place that knows who opened which preview, alongside every other private service.

The last one is usually the real reason. A preview of a docs site or the Ziti admin console is too small a thing to
need an access-control story of its own.

## Related

- [Exposers](../exposers.md)
- [Security model](../reference/security.md)
