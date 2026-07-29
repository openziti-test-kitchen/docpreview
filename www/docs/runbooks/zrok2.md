---
id: zrok2
title: Runbook — zrok v2
sidebar_position: 2
---

# Runbook: zrok v2

docpreview assumes zrok v2 is installed and its environment is enabled. It does not enroll for you — it checks
at startup and tells you what is missing.

## Install

The v2 binary is called `zrok2`, not `zrok`, and its config lives in `~/.zrok2` with a `ZROK2_` environment
prefix. That is deliberate: v1 and v2 coexist on one machine without interfering.

Download from [the install page](https://netfoundry.io/docs/zrok/get-started/install-zrok2/), then:

```powershell
zrok2 version
```

## Enable an environment

You need an account token from [zrok.io](https://zrok.io) or your self-hosted controller.

```powershell
zrok2 enable <your-account-token>
```

This enrolls **this machine** as a zrok environment: it registers an OpenZiti identity and writes it under
`~/.zrok2`. docpreview loads that identity at startup — it never sees your account token in its own config.

Check it took:

```powershell
zrok2 overview
```

## Namespaces

A **namespace** is a zone that owns names, usually corresponding to a DNS zone. Names within it can be
ephemeral or reserved.

Most environments have a default namespace, and docpreview uses it when `exposer.zrok2.namespace` is blank.
Find out what yours is:

```powershell
zrok2 list namespaces
```

If there is no default, either set one or name it explicitly:

```yaml
exposer:
  kind: zrok2
  zrok2:
    namespace: my-namespace
```

## What docpreview does with names

Nothing, ahead of time. On each build it creates a public share with a name selection of
`{namespace, sanitized-branch-name}` and attaches a fresh overlay listener to it. Because v2 decoupled names
from shares, that name survives the share being torn down and recreated, which is what keeps the URL in the
pull request comment stable.

You do not need to pre-create names for branches. You **do** want a reserved name for the webhook ingress —
see the [GitHub App runbook](./github-app.md) — because that URL lives in GitHub's App settings and you do not
want to edit it on every restart.

## Verify docpreview can use it

```powershell
docpreview doctor
```

The zrok check does two things: confirms the environment is enabled locally, and makes a round trip to the
controller. The second matters because an environment can be present and enabled on disk while the account
token has been revoked server-side — which otherwise shows up as an unexplained 401 on the first pull request
of the day.

## Failure modes

**`zrok environment is not enabled; run 'zrok2 enable <account-token>'`**
You have `zrok2` installed but never enrolled this machine. Run the command.

**`zrok controller rejected this environment`**
The environment exists locally but the controller disagrees — a revoked token, or the environment was deleted
from the web console. `zrok2 disable` then `zrok2 enable <token>` again.

**`no zrok namespace configured and the environment has no default`**
Set `exposer.zrok2.namespace` in the config, or give the environment a default namespace.

**`creating zrok share "..." — name already in use`**
Something else holds that name. docpreview tries once to reclaim a name held by one of its own orphaned
shares and retries; if that fails, the name belongs to something it did not create. Check `zrok2 list shares`.

## Restricting who can see previews

By default previews are open — anyone with the link can read them. For unreleased documentation:

```yaml
exposer:
  zrok2:
    open: false
    access_grants:
      - alice@example.com
      - bob@example.com
```

Or put an identity provider in front:

```yaml
exposer:
  zrok2:
    oauth_provider: github
    oauth_email_domains:
      - "*@example.com"
```

For WAF and MFA on top of that, see [Frontdoor](./frontdoor.md).

## Sources

- [Introducing zrok v2.0](https://blog.openziti.io/introducing-zrok-v2-0)
- [zrok getting started](https://netfoundry.io/docs/zrok/get-started/install-zrok2/)
- [Public shares](https://docs.zrok.io/docs/concepts/sharing-public/)
