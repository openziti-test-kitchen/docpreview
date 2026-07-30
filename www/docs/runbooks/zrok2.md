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

A **name is an object with its own quota**, separate from a share, and docpreview manages the whole lifecycle:

1. **Registers the name** before creating the share. v2 does not create one implicitly for a named share — it
   answers `409` and complains it cannot find the name in the namespace — so this step is not optional.
2. **Creates a public share** bound to `{namespace, name}` and attaches a fresh overlay listener to it.
3. **Leaves the name alone** on every rebuild, supersede and shutdown. That is what keeps the URL in the pull
   request comment stable across pushes, and it is why one comment can be edited forever instead of replaced.
4. **Releases the name when the preview is torn down** — de-reserved first, then deleted, so a crash between the two
   leaves a non-reserved name that the controller collects on its own.

Two names per pull request, not one: the branch and, per build, the branch name with the short commit appended.

You do not need to pre-create names for branches. You **do** want a reserved name for the webhook ingress and
another for the public dashboard — see the [webhook tunnel runbook](./webhook-tunnel.md) — because those URLs live
in somebody else's settings and you do not want to edit them on every restart. Those two are yours, created by hand,
and docpreview never releases them.

### Watch the count

```powershell
zrok2 list names
```

One name per pull request accumulated slowly. One name per *build* does not. If a build's share fails with
`names limit reached; cannot reserve additional names`, the branch URL is unaffected and the failure appears only in
the daemon's log — see [`preview.keep_builds`](../reference/configuration.md#preview).

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

**`registering zrok name "..." in namespace "...": names limit reached; cannot reserve additional names`**
The account is out of reserved names. `zrok2 list names`, then close pull requests or lower
`preview.keep_builds`. Note that the same `409` status also carries "failed profanity or DNS check" for an
unusable name — the message distinguishes them, the status does not.

**`could not release an exposer name; it stays counted against the account's limit until it is deleted by hand`**
A teardown could not release a name. The preview is gone, the name is not. `zrok2 delete name <name>`.

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
