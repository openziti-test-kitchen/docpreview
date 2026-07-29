---
id: webhook-tunnel
title: Runbook — expose the webhook
sidebar_position: 4
---

# Runbook: expose the webhook

The daemon binds loopback. GitHub is on the internet. Something has to bridge those two, and the shape of that
bridge is the whole security story of this deployment — so it is a separate process publishing exactly one route,
not a tunnel pointed at the daemon.

Work through this **before** the [GitHub App runbook](./github-app.md) asks you for a Webhook URL. At the end you
will have a URL that survives restarts and a `PASS` that proves a signed delivery is accepted end to end.

## Before you start

- zrok v2 installed and this machine enrolled — see [the zrok v2 runbook](./zrok2.md).
- `github.webhook_secret` already in the vault. `webhook-check` reads it from there, so store it first:

  ```powershell
  "paste-the-secret-here" | docpreview vault set github.webhook_secret
  ```

  If you have not generated one yet, [step 2 of the GitHub App runbook](./github-app.md#step-2--create-the-app)
  has the command. The same value goes into the App later.

## Step 1 — reserve a name

An ephemeral share mints a new hostname every time it starts, which means editing the App's Webhook URL after
every restart. Reserve a name once instead:

```powershell
zrok2 create name docpreview
```

```bash
zrok2 create name docpreview
```

That yields `docpreview.shares.zrok.io`. If your environment has no default namespace, name one:

```powershell
zrok2 create name -n <namespace> docpreview
```

Confirm it, and note that `SHARE TOKEN` is empty until a share binds to it:

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

This is also how you check the state after a crash: a `SHARE TOKEN` present with nothing running means an
abandoned share still holds the name. Step 2 reclaims that automatically.

Two things about zrok v2 that are worth knowing before you go looking for them:

**There is no `zrok2 reserve`.** v1 made durability a property of a *share*; v2 decoupled the two. Durability is
now a **name** in a namespace, carrying a reserved flag, and a share binds to it. So the thing you create ahead of
time is a name, and shares come and go underneath it.

**`zrok2 share public` cannot bind that name.** The command takes one positional argument — the target — and its
`-n` / `--name-selection` flag sets only `NameSelection.NamespaceToken`, despite what the flag is called and
despite its help text mentioning frontends. There is no CLI path from a reserved name to a public share. The SDK
has one: `ShareRequest.NameSelections` carries `{Name, NamespaceToken}` pairs. That is why the next step creates
the share itself rather than shelling out to `zrok2`.

## Step 2 — serve only the webhook

```powershell
docpreview webhook-only -zrok-name docpreview -upstream http://127.0.0.1:8471
```

```bash
docpreview webhook-only -zrok-name docpreview -upstream http://127.0.0.1:8471
```

Leave it running in its own terminal. It logs the URL to hand GitHub:

```text
level=INFO msg="webhook-only serving over zrok" url=https://docpreview.shares.zrok.io forwards="POST /webhook/github" to=http://127.0.0.1:8471
level=INFO msg="this is the webhook URL to give GitHub" webhook=https://docpreview.shares.zrok.io/webhook/github
```

### Why this exists rather than sharing the daemon

`zrok2 share public http://127.0.0.1:8471` shares an *origin*, not a path. Every route the daemon serves goes with
it — the dashboard, the previews, and `/api/secrets`. The write endpoints on that API are gated on one test:
whether the daemon's listeners are loopback. They are, always, by design. So the gate answers yes while the
surface is on the internet, and with an unlocked vault `PUT`, `DELETE` and generate all succeed for anyone holding
the share URL.

No check inside the daemon can close that hole. In proxy mode the daemon sees the connection from the local zrok
process, so `RemoteAddr` is loopback too, and `Host` is whatever the client sent. The distinction does not exist at
that layer. The fix is not a smarter check, it is not putting the admin surface and the public route on one origin.

So `webhook-only` forwards `POST /webhook/github` and nothing else. It does **not** verify the signature — that is
the daemon's job, with the secret from the vault, and duplicating it here would mean a second copy of the secret in
a second process. This is a router; the guard is one hop further in.

With `-zrok-name` it binds **no local TCP port at all**. The listener is the zrok share, so there is nothing on
`127.0.0.1` for anything else on the machine to find. Omit the flag and it listens on `-listen`
(`127.0.0.1:8481` by default), which is useful for testing the filter locally.

### Restarts reclaim the name

A process serving a tunnel is normally ended by a kill, and a kill runs no deferred cleanup — so the share from the
previous run outlives it and still holds the name. `webhook-only` handles that: on a name-in-use error it lists the
shares belonging to this environment, deletes any whose leftmost DNS label matches the name, and retries. It logs
it when it happens:

```text
level=WARN msg="reclaimed an abandoned zrok share that still held the name" token=abc123xyz name=docpreview
```

Matching is on the label rather than a substring, so a name of `docs` will not reclaim a share published at
`docs-internal.shares.zrok.io`. Reclaiming is only safe because the name is reserved to your account: the only
thing that can be holding it is your own abandoned share.

Cleanup deletes the share and leaves the name. The name is the stable URL, which is the entire point.

## Step 3 — prove it before configuring GitHub

Start the daemon in another terminal, then:

```powershell
docpreview serve
```

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

This is the step that replaces "paste it into GitHub and hope" with "already known to work". It signs a `ping` with
the secret read from the vault — never printed, never an argument, never in your shell history — and posts it.
Signature verification runs before the event type is examined, so a signed `ping` returning 2xx has already proven
the signature passed. A fabricated `pull_request` would prove the same thing and additionally queue a build for a
repository that does not exist.

Confirm it from the other end too. The daemon logs the ping with the same delivery id:

```text
level=INFO msg="received github ping" delivery=check-3f9a1c04b7e2d518
```

Two ends agreeing on one delivery id is the whole path verified.

When it fails, it says which hop:

| Status | What it means |
|---|---|
| connection error | Nothing answers the URL. Is `webhook-only` running? |
| `401` | The request arrived and the signature was rejected. The vault this command read is not the one the daemon read — `-config` points elsewhere, or the secret was rotated after the daemon started. |
| `404` | The tunnel is up and the path is wrong. It must match what `webhook-only` was given, `/webhook/github` unless you passed `-path`. |
| `501` | The daemon has no GitHub client: `github.app_id` is unset, or the vault is still locked. Run `docpreview doctor`. |
| `502` | zrok has the share and cannot reach the backend. Covered below. |

## Step 4 — give GitHub the URL

Now go to the [GitHub App runbook](./github-app.md). Two fields take what you built here:

- **Webhook URL** — `https://docpreview.shares.zrok.io/webhook/github`, the exact URL `webhook-check` passed on.
- **Webhook secret** — the same value that is in the vault under `github.webhook_secret`. A mismatch here is the
  `401` in the table above, arriving from GitHub's side instead of yours.

Everything else — permissions, events, the private key, installation — is unchanged; follow that runbook from
[step 2](./github-app.md#step-2--create-the-app).

## `GET` on the webhook URL answers 405. That is correct.

This is the single most likely thing to look like a failure, so: **pasting the webhook URL into a browser address
bar cannot work and never could.** A browser sends `GET`. GitHub sends `POST`. The route is registered as
`POST /webhook/github`, so a `GET` gets a 405 and a sentence:

```text
this endpoint accepts signed POST deliveries only; there is nothing here to open in a browser
```

A 405 with that body is proof the tunnel is up and the path is right — it is the best news a browser can give you.
Every other path, including `/`, answers a bare 404 with no hint that a dashboard exists behind it. The 405 leaks
nothing: a `POST` to the real path answers 401 and a `POST` to anything else answers 404, so the path is
distinguishable to anyone probing properly regardless.

## What a 502 means

zrok has the share, and the backend is not attached to it yet. The share exists at the frontend the moment the
controller creates it; the overlay listener takes a few seconds to attach after `webhook-only` starts. A 502 in
that window resolves itself — wait and re-run `webhook-check`.

A 502 that persists means `webhook-only` is not running, or it is running and the daemon behind it is not.
`webhook-only` returns 502 rather than 500 when forwarding fails, deliberately: GitHub retries 5xx deliveries, and
"the daemon is briefly down" is exactly the case where a retry helps.

## Restarting

Restart either process in any order. The URL does not change, so **the App never needs reconfiguring**. That is
what the reserved name bought:

1. Kill `webhook-only`. The share is orphaned; the name is not.
2. Start it again. It reclaims the name, creates a new share, binds the same hostname.
3. `docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github` to confirm.

If step 2 fails with a name-in-use error that reclaiming did not resolve, something you did not create holds the
name. `zrok2 list shares` will show what.

## Where this is going

The end state removes the public URL entirely: a GitHub Action in the documentation repository dials the daemon's
webhook endpoint over an OpenZiti service instead of GitHub's webhook delivery reaching a public hostname. The
daemon already hosts previews from an overlay identity, so the ingress is the last thing left needing a public
frontend, and the Action's own runner is the only client that has to reach it. **This is not built.** Until it is,
the reserved name and this filtering proxy are how deliveries arrive.

## Related

- [zrok v2 runbook](./zrok2.md)
- [Create the GitHub App](./github-app.md)
- [CLI reference](../reference/cli.md)
- [Security model](../reference/security.md)
- [Ziti-native previews](../future/ziti-native-previews.md)
