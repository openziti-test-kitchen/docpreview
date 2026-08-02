---
id: zrok2
title: Runbook — zrok v2
sidebar_position: 2
---

# Runbook: zrok v2

Two ways get you from nothing to a working zrok environment. docpreview can do the whole thing itself —
sign up, enrol, and tell you it worked — or you can use the `zrok2` CLI and point docpreview at the result. The
first is fewer steps. The second is what you want if you already have a zrok account for something else.

## The short way: let docpreview do it

Open **`/secrets`** on the dashboard and use the panel headed *How previews reach the internet*. Or, on a host with
no browser:

```powershell
docpreview zrok invite you@example.com          # zrok emails a registration link
docpreview zrok register <the-link-from-that-email>
```

`register` asks for a password on stdin — that is the password for the **zrok account**, not for docpreview. It
creates the account, stores the account token in the vault, and enrols this host in one step, because the token
appears exactly once, in the registration response.

Already have a zrok account?

```powershell
docpreview zrok enable -token-stdin
```

Then set `exposer.kind: zrok2` in the config and restart. `docpreview zrok status` says what is enrolled and where.

:::note Signing up twice is refused

Every command and every button above refuses if an environment is already enrolled. A second account means a second
set of reserved names, and every preview URL already posted to a pull request lives on the first one.

:::

## Two possible environments, and they are usually two accounts

zrok keeps its account token and enrolled identity in a directory. There can be two:

| | Path | |
|---|---|---|
| **this machine** | `~/.zrok2` | what the `zrok2` CLI uses — you very likely already have this |
| **this installation** | `<data_dir>/zrok2` | docpreview's own, beside the vault |

Both existing is the ordinary case on a developer's machine, and they are different zrok accounts. This matters more
than it looks:

:::danger Startup deletes every share it recognises as its own

A daemon pointed at the wrong environment deletes the shares belonging to whatever else uses that account. See
[one daemon per exposer account](../troubleshooting.md).

:::

So the choice is explicit and stored, never guessed:

```powershell
docpreview zrok status                # both directories, and which is in use
docpreview zrok use project           # docpreview's own, beside the vault
docpreview zrok use system            # the one the zrok2 CLI uses
```

A change takes effect at the **next restart**. zrok's root directory is a process-wide setting read once when the
daemon starts, so the dashboard shows *chosen* and *in use* separately until you restart.

**`webhook-only` and `dashboard-only` need telling too.** They read no config file, so they cannot know which
directory the daemon chose:

```powershell
docpreview webhook-only   -zrok-name docpreview      -zrok-home <data_dir>\zrok2
docpreview dashboard-only -zrok-name docpreview-dash -zrok-home <data_dir>\zrok2
```

Omit `-zrok-home` and they use `~/.zrok2`. If that is a different account from the daemon's, the share they create
reserves a name the previews cannot use.

**In a container, use `project`** and mount `<data_dir>` as a volume. A home directory in a container is not
durable, so `system` means re-enrolling on every restart — and every enrolment spends an environment against the
account's quota and orphans the last one.

## Install the CLI (the long way, and for `zrok2 overview`)

The v2 binary is called `zrok2`, not `zrok`, and its config lives in `~/.zrok2` with a `ZROK2_` environment
prefix. That is deliberate: v1 and v2 coexist on one machine without interfering.

Download from [the install page](https://netfoundry.io/docs/zrok/get-started/install-zrok2/), then:

```powershell
zrok2 version
```

## Enable an environment with the CLI

You need an account token from [zrok.io](https://zrok.io) or your self-hosted controller.

```powershell
zrok2 enable <your-account-token>
```

This enrolls **this machine** as a zrok environment: it registers an OpenZiti identity and writes it under
`~/.zrok2`. docpreview loads that identity at startup — it never sees your account token in its own config.

Done this way, the environment is the **system** one, and a daemon that finds it enabled and has no stored choice
adopts it and records that. So the daemon keeps working with no further action.

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
shares and retries. If that fails, the name belongs to something it did not create. Check `zrok2 list shares`.

**`registering zrok name "..." in namespace "...": names limit reached; cannot reserve additional names`**
The account is out of reserved names. `zrok2 list names`, then close pull requests or lower
`preview.keep_builds`. The same `409` status also carries "failed profanity or DNS check" for an
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
