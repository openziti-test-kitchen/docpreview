---
id: zrok2
title: Publish previews with zrok v2
sidebar_position: 4
---

# Publish previews with zrok v2

There are two ways to get from nothing to a working zrok environment. NetFoundry Preview can do the whole thing
itself: sign up, enrol, and tell you it worked. Or you can use the `zrok2` CLI and point it at the result. The first
takes fewer steps. Pick the second if you already have a zrok account for something else.

## The short way: let NetFoundry Preview do it

Open **`/secrets`** on the dashboard and use the section headed *Exposer configuration*. On a host with no browser:

```powershell
nfpreview zrok invite you@example.com          # zrok emails a registration link
nfpreview zrok register <the-link-from-that-email>
```

`register` asks for a password on stdin. It is the password for the **zrok account**, not for NetFoundry Preview.
The command creates the account, stores the account token in the vault, and enrols this host in one step, because
the registration response is the only time the token appears.

Already have a zrok account?

```powershell
nfpreview zrok enable -token-stdin
```

Then set `exposer.kind: zrok2` in the config and restart. `nfpreview zrok status` says what is enrolled and where.

:::note Signing up twice is refused

`invite`, `register` and the dashboard's sign-up buttons refuse if either environment is already enrolled.
`zrok enable` refuses only if the environment it would write is already enrolled. A second account means a second
set of reserved names, and every preview URL already posted to a pull request lives on the first one.

:::

## Two possible environments, usually two accounts

zrok keeps its account token and enrolled identity in a directory. There can be two:

| | Path | |
|---|---|---|
| **this machine** | `~/.zrok2` | what the `zrok2` CLI uses. You very likely have this already |
| **this installation** | `<data_dir>/zrok2` | the daemon's own, beside the vault |

On a developer's machine both usually exist, and they are different zrok accounts. That matters:

:::danger Startup deletes the shares its database does not claim

At startup the daemon adopts the shares in its environment that its database claims, and deletes every other share
it recognises as its own. A daemon pointed at the wrong environment deletes the previews of whatever else uses it.
See [one daemon per exposer account](../troubleshooting.md).

:::

So the choice is explicit and stored, never guessed:

```powershell
nfpreview zrok status                # both directories, and which is in use
nfpreview zrok use project           # the daemon's own, beside the vault
nfpreview zrok use system            # the one the zrok2 CLI uses
```

A change takes effect at the **next restart**. zrok reads its root directory once, when the daemon starts, for the
whole process. Until you restart, the dashboard shows *chosen* and *in use* separately.

**`webhook-only` and `dashboard-only` need telling too.** They read no config file, so they can't know which
directory the daemon chose:

```powershell
nfpreview webhook-only   -zrok-name nfpreview      -zrok-home <data_dir>\zrok2
nfpreview dashboard-only -zrok-name nfpreview-dash -zrok-home <data_dir>\zrok2
```

Leave out `-zrok-home` and they use `~/.zrok2`. If that is a different account from the daemon's, the tunnel fails
unless that account also holds the name, and then the URL belongs to an account the daemon never sees.

**In a container, use `project`** and mount `<data_dir>` as a volume. A container's home directory does not last,
so `system` means enrolling again on every restart. Each enrolment uses up an environment from the account's quota
and orphans the last one.

## Install the CLI (the long way, and for `zrok2 overview`)

The v2 binary is called `zrok2`, not `zrok`. Its config lives in `~/.zrok2` and its environment variables start with
`ZROK2_`, so v1 and v2 can sit on one machine without interfering.

Download it from [the install page](https://netfoundry.io/docs/zrok/get-started/install-zrok2/), then:

```powershell
zrok2 version
```

## Enable an environment with the CLI

You need an account token from [zrok.io](https://zrok.io) or your self-hosted controller.

```powershell
zrok2 enable <your-account-token>
```

This enrolls **this machine** as a zrok environment. It registers an OpenZiti identity and writes it under
`~/.zrok2`. The daemon loads that identity at startup, so your account token never appears in its config.

An environment enabled this way is the **system** one. A daemon that finds it enabled and has no stored choice
adopts it and records that, so the daemon keeps working with nothing more from you.

Check it took:

```powershell
zrok2 overview
```

## Namespaces

A **namespace** is a zone that owns names, and it usually matches a DNS zone. Names in it can be ephemeral or
reserved.

Most environments have a default namespace, and the daemon uses it when `exposer.zrok2.namespace` is blank. Find
yours:

```powershell
zrok2 list namespaces
```

If there is no default, set one or name it explicitly:

```yaml
exposer:
  kind: zrok2
  zrok2:
    namespace: my-namespace
```

## What `nfpreview` does with names

A **name is an object with its own quota**, separate from a share, and the daemon manages its whole lifecycle:

1. **It registers the name** before creating the share. v2 does not create a name for a named share on its own. It
   answers `409` and says it can't find the name in the namespace, so this step is required.
2. **It creates a public share** bound to `{namespace, name}` and attaches an overlay listener to it.
3. **It keeps the share and the name** through every rebuild, supersede and restart. A rebuild swaps what the share
   serves, and a restart adopts the share instead of creating one. That keeps the URL in the pull request comment
   stable across pushes, so one comment can be edited forever instead of replaced.
4. **It releases the name when the preview is torn down.** It removes the reservation first and then deletes the
   name, so a crash between the two leaves an unreserved name that the controller collects on its own.

By default that is one name per pull request, for the branch. With `exposer.build_shares: true`, each build also gets
a share and a name of its own, the branch name with the short commit appended, for the **Open build** link.

You don't need to create names for branches ahead of time. You **do** want a reserved name for the webhook ingress
and another for the public dashboard (see the [webhook tunnel guide](./webhook-tunnel.md)), because those URLs live
in someone else's settings and you don't want to edit them after every restart. You create those two by hand, and
the daemon never releases them.

### Watch the counts

```powershell
zrok2 list names
```

With the default of one name per pull request, names pile up slowly. With `exposer.build_shares` on, every build
adds one, and [`preview.keep_builds`](../reference/configuration.md#preview) limits how many are kept. When the
account runs out, a build's share fails, the branch URL is unaffected, and the failure appears only in the daemon's
log.

Share *creations* are limited too. Every process on one zrok environment (the daemon, `webhook-only` and
`dashboard-only`) goes through one gate of 15 creations per 10 minutes. Two 429 answers in a row from the
controller pause creations for 10 minutes. The dashboard footer shows `zrok shares N/15`, and `/status` reports it
as `zrok_shares`. A restart does not count against it, because previews and both tunnels adopt their existing
shares.

## Verify the daemon can use it

```powershell
nfpreview doctor
```

The zrok check does two things. It confirms the environment is enabled locally, and it makes a round trip to the
controller. The round trip matters because an environment can be enabled on disk while its account token has been
revoked on the server. Without the check, that shows up as an unexplained 401 on the first pull request of the day.

## Failure modes

**`no zrok environment is enrolled here; sign up and enrol from the dashboard at /secrets, or run 'nfpreview zrok invite <email>'`**
Enrol with [the short way](#the-short-way-let-netfoundry-preview-do-it). If you already did, `nfpreview zrok
status` shows which directory this installation is reading.

**`zrok controller rejected this environment`**
The environment exists locally but the controller disagrees. The token was revoked, or the environment was deleted
from the web console. Run `zrok2 disable`, then `zrok2 enable <token>` again.

**`no zrok namespace configured and the environment has no default`**
Set `exposer.zrok2.namespace` in the config, or give the environment a default namespace.

**`creating zrok share "..." in namespace "...": <controller error>`**
Another share holds that name. The daemon tries once to reclaim a name held by one of its own orphaned shares and
retries. If that fails, the name belongs to something it did not create. Check `zrok2 list shares`.

**`registering zrok name "...": this zrok account holds the maximum number of reserved names`**
The account is out of reserved names. Run `zrok2 list names`, then close pull requests, or lower
`preview.keep_builds` if `exposer.build_shares` is on. The same `409` status also carries "failed profanity or DNS
check" for a name zrok won't accept. The message tells them apart and the status does not.

**`could not release an exposer name; it stays counted against the account's limit until it is deleted by hand`**
A teardown could not release a name. The preview is gone and the name is not. Run `zrok2 delete name <name>`.

## Restricting who can see previews

By default previews are open, and anyone with the link can read them. For an unreleased site:

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

For a WAF and MFA on top of that, see [Frontdoor](./frontdoor.md).

## Sources

- [Introducing zrok v2.0](https://blog.openziti.io/introducing-zrok-v2-0)
- [zrok getting started](https://netfoundry.io/docs/zrok/get-started/install-zrok2/)
- [Public shares](https://netfoundry.io/docs/zrok/concepts/sharing-public/)
