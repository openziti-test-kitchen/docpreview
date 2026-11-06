---
id: webhook-tunnel
title: Expose the webhook
sidebar_position: 3
---

# Expose the webhook

The daemon binds loopback. GitHub and Bitbucket are on the internet. Something has to bridge the two, and how you
build that bridge decides the security of the whole deployment. So the bridge is a separate process that publishes
exactly one route. It is not a tunnel pointed at the daemon.

Do this **before** the [GitHub App guide](./github-app.md) asks you for a Webhook URL. You finish with a URL that
survives restarts and a `PASS` that proves a signed delivery is accepted end to end.

## Before you start

- zrok v2 installed and this machine enrolled. See [the zrok v2 guide](./zrok2.md).
- `github.webhook_secret` already in the vault. `webhook-check` reads it from there, so store it first:

  ```powershell
  "paste-the-secret-here" | docpreview vault set github.webhook_secret
  ```

  If you have not generated one yet, [step 2 of the GitHub App guide](./github-app.md#step-2-create-the-app) has
  the command. The same value goes into the App later.

## Step 1: Reserve a name

An ephemeral share gets a new hostname every time it starts, and you would have to edit the App's Webhook URL after
every restart. Reserve a name once instead:

```powershell
zrok2 create name docpreview
```

```bash
zrok2 create name docpreview
```

That gives you `docpreview.shares.zrok.io`. If your environment has no default namespace, name one:

```powershell
zrok2 create name -n <namespace> docpreview
```

Confirm it. `SHARE TOKEN` stays empty until a share binds to the name:

```powershell
zrok2 list names
```

```text
╭───────────────────────────┬────────────┬───────────┬─────────────┬──────────┬─────────────────────╮
│ URL                       │ NAME       │ NAMESPACE │ SHARE TOKEN │ RESERVED │ CREATED             │
├───────────────────────────┼────────────┼───────────┼─────────────┼──────────┼─────────────────────┤
│ docpreview.shares.zrok.io │ docpreview │ public    │             │ true     │ 2026-07-29 07:42:57 │
╰───────────────────────────┴────────────┴───────────┴─────────────┴──────────┴─────────────────────╯
```

**There is no `zrok2 reserve`.** zrok v1 made a *share* durable. v2 splits the two: a **name** in a namespace
carries the reserved flag, and shares bind to it. So you create the name ahead of time, and shares come and go
underneath it.

`webhook-only` creates the name's share itself through the zrok SDK rather than shelling out to `zrok2 share
public`. That is what lets it run with no local port and pick its own share back up after a restart.

## Step 2: Serve only the webhook

```powershell
docpreview webhook-only -zrok-name docpreview -upstream http://127.0.0.1:8471
```

```bash
docpreview webhook-only -zrok-name docpreview -upstream http://127.0.0.1:8471
```

Leave it running in its own terminal. It logs the URL to configure:

```text
level=INFO msg="webhook-only serving over zrok" url=https://docpreview.shares.zrok.io forwards="POST /webhook/github" to=http://127.0.0.1:8471
level=INFO msg="this is the webhook URL to configure" platform=github webhook=https://docpreview.shares.zrok.io/webhook/github
```

For Bitbucket, pass `-path` once per route. Each `-path` replaces the default, so name both if you use both:

```powershell
docpreview webhook-only -zrok-name docpreview -upstream http://127.0.0.1:8471 `
  -path /webhook/github -path /webhook/bitbucket
```

### Why not share the daemon directly

`zrok2 share public http://127.0.0.1:8471` shares an *origin*, not a path. Every route the daemon serves goes with
it: the dashboard, the previews, and `/api/secrets`.

The credential API does guard its write endpoints. A write needs an admin session, or else a loopback listener plus
a loopback request with no forwarding header (`X-Forwarded-For`, `Forwarded` and the like). Behind a proxy, though,
the daemon sees the connection come from the local zrok process, so `RemoteAddr` is loopback too. Only the
forwarding headers give the proxy away, and a tunnel that strips them defeats the check. Treat that check as a
second line of defence. The first is keeping the admin surface and the public route on different origins.

So `webhook-only` forwards `POST /webhook/github` by default, or the paths you pass with `-path`, and nothing
else. It does **not** verify the signature. The daemon does that with the secret from the vault, and checking here
too would put a second copy of the secret in a second process. `webhook-only` is a router, and the guard sits one
hop further in.

With `-zrok-name` it binds **no local TCP port**. The listener is the zrok share, so there is nothing on
`127.0.0.1` for anything else on the machine to find. Without the flag it listens on `-listen` (`127.0.0.1:8481` by
default), which is useful for testing the filter locally.

### Restarts adopt the existing share

Stopping `webhook-only` closes its listener and leaves the share on the zrok controller, still bound to the name.
The next start finds that share and serves on it, so a restart creates no new share:

```text
level=INFO msg="adopted the existing zrok share" token=abc123xyz name=docpreview
```

It only creates a share when there is none to adopt. Creations go through the share gate the daemon uses, 15 per
10 minutes per zrok environment, so a unit stuck in a restart loop waits instead of creating a share every few
seconds. If the create fails because something still holds the name, `webhook-only` deletes the shares in this
environment whose leftmost DNS label matches the name, retries, and logs a
`reclaimed an abandoned zrok share that still held the name` warning.

It matches on the label, not a substring, so a name of `docs` never touches a share published at
`docs-internal.shares.zrok.io`. Reclaiming is safe because the name is reserved to your account, so the only thing
that can hold it is your own share.

## Step 3: Prove it before configuring GitHub

Start the daemon in another terminal:

```powershell
docpreview serve
```

Then check the whole path:

```powershell
docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github
```

```bash
docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github
```

```text
POST https://docpreview.shares.zrok.io/webhook/github
  event     ping
  delivery  check-3f9a1c04b7e2d518
  status    202 Accepted in 412ms
  body      accepted

PASS — a signed delivery is accepted end to end.

That covers the public URL, the tunnel, the webhook-only filter, and the daemon's
signature check against the secret in the vault. Give GitHub this URL and the same
webhook secret and its own ping will land the same way.
```

Now you know it works before you paste anything into GitHub. The command signs a `ping` with the secret from the
vault and posts it. The secret is never printed, never an argument, and never in your shell history. The daemon
checks the signature before it looks at the event type, so a signed `ping` that returns 2xx proves the signature
passed. A forged `pull_request` would prove the same, but a ping is cleaner. For a repository with no project row
it is acknowledged with 202 and dropped.

Check the other end too. The daemon logs the ping with the same delivery id:

```text
level=INFO msg="received github ping" delivery=check-3f9a1c04b7e2d518
```

When both ends agree on one delivery id, the whole path works.

When it fails, it says which hop:

| Status | What it means |
|---|---|
| connection error | Nothing answers the URL. Is `webhook-only` running? |
| `401` | The request arrived and the signature was rejected. This command read a different vault from the daemon's: `-config` points elsewhere, or the secret was rotated after the daemon started. |
| `404` | The tunnel is up and the path is wrong. It must match what `webhook-only` was given, `/webhook/github` unless you passed `-path`. |
| `501` | The daemon has no GitHub client: `github.app_id` is unset, or the vault is still locked. Run `docpreview doctor`. |
| `502` | zrok has the share and can't reach the backend. See below. |

## Step 4: Give GitHub the URL

Now go to the [GitHub App guide](./github-app.md). Two fields take what you built here:

- **Webhook URL**: `https://docpreview.shares.zrok.io/webhook/github`, the exact URL `webhook-check` passed on.
- **Webhook secret**: the value in the vault under `github.webhook_secret`. A mismatch is the `401` in the table
  above, coming from GitHub's side instead of yours.

Permissions, events, the private key and installation are unchanged. Pick that guide up at
[step 2](./github-app.md#step-2-create-the-app).

## `GET` on the webhook URL answers 405. That is correct.

This one looks like a failure more than anything else here. **Pasting the webhook URL into a browser can't work.**
A browser sends `GET` and GitHub sends `POST`. The route is registered as `POST /webhook/github`, so a `GET` gets a
405 and a sentence:

```text
this endpoint accepts signed POST deliveries only; there is nothing here to open in a browser
```

A 405 with that body proves the tunnel is up and the path is right. It is the best answer a browser can give you.
Every other path, including `/`, answers a bare 404 with no hint of a dashboard behind it. The 405 gives nothing
away. A `POST` to the real path answers 401 and a `POST` anywhere else answers 404, so anyone probing properly can
find the path anyway.

## What a 502 means

zrok has the share, and the backend is not attached to it yet. The share exists at the frontend as soon as the
controller has it, and the overlay listener takes a few seconds to attach after `webhook-only` starts. A 502 in that
window clears up on its own. Wait and re-run `webhook-check`.

A 502 that persists means `webhook-only` is not running, or it is and the daemon behind it is not. `webhook-only`
answers 502 rather than 500 when forwarding fails, because GitHub retries 5xx deliveries, and a retry is exactly
what helps when the daemon is briefly down.

## Restarting

Restart either process in any order. The URL does not change, so **the App never needs reconfiguring**. The reserved
name buys you that:

1. Stop `webhook-only`. The share and the name both stay on the controller.
2. Start it again. It adopts the share and serves the same hostname without creating a new one.
3. Run `docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github` to confirm.

If step 2 fails with a name-in-use error that reclaiming did not fix, something you did not create holds the name.
`zrok2 list shares` shows what.

## Where this is going

The goal is no public URL at all. A GitHub Action in the repository would dial the daemon's webhook endpoint over an
OpenZiti service, instead of GitHub delivering to a public hostname. The daemon already hosts previews from an
overlay identity, so the ingress is the last piece that needs a public frontend, and the Action's runner is the only
client that has to reach it. **This is not built.** Until it is, deliveries arrive through the reserved name and this
filtering proxy.

## Related

- [zrok v2 guide](./zrok2.md)
- [Create the GitHub App](./github-app.md)
- [CLI reference](../reference/cli.md)
- [Security model](../reference/security.md)
- [Ziti-native previews](https://github.com/openziti-test-kitchen/docpreview/blob/main/docs/design/ziti-native-previews.md)
