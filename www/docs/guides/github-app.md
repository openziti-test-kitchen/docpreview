---
id: github-app
title: Create the GitHub App
sidebar_position: 1
---

# Create the GitHub App

This is the one part nobody can automate for you: creating a GitHub App requires a human in a browser. Budget
ten minutes. At the end you will have three values in the vault and a working webhook.

## Before you start

- Admin rights on the organization (or a personal account, for a personal repository).
- `docpreview` built and on your PATH.
- A vault master key. The vault is one [age](../background/age.md)-encrypted file at
  `~/.docpreview/vault.age`. If you have not made a key:

  ```powershell
  docpreview vault keygen -out C:\ProgramData\docpreview\master.key
  ```

  ```bash
  docpreview vault keygen -out /etc/docpreview/master.key
  ```

  Then point the daemon at it, which is what lets it unlock itself after a restart:

  ```yaml
  vault:
    key_source: "file:/etc/docpreview/master.key"
  ```

  The key never reaches your clipboard or your shell history either way. **Back it up somewhere that is not
  this machine** — it is the only thing that decrypts the vault. If you would rather no key sat on disk at
  all, skip this: `serve` starts with a locked vault and the dashboard unlocks it, at the cost of a person
  being needed after every restart. See [the master key](../reference/security.md#the-master-key).

## Step 1 — get a public webhook URL first

GitHub validates the webhook URL when you save the App, so have it working before you start clicking. That is a
guide of its own: **[expose the webhook](./webhook-tunnel.md)**. Work through it now and come back with two
things.

1. A URL like `https://docpreview.shares.zrok.io/webhook/github`, stable across restarts because it is bound to a
   reserved zrok name.
2. A `PASS` from `docpreview webhook-check -url <that URL>`, which signs a `ping` with the secret from the vault
   and proves the whole path already accepts a signed delivery.

Do not point a share at the daemon directly. `zrok2 share public http://127.0.0.1:8471` publishes every route
including `/api/secrets`, putting the credential API on the internet. `docpreview webhook-only` publishes one
route and nothing else. The reasoning is in
[that guide](./webhook-tunnel.md#why-this-exists-rather-than-sharing-the-daemon).

That guide also has you store `github.webhook_secret` in the vault first, since `webhook-check` reads it from
there. Use that same value in step 2 below.

Leave `webhook-only` running in its own terminal.

## Step 2 — create the App

Go to **Settings → Developer settings → GitHub Apps → New GitHub App**.

- Organization: `https://github.com/organizations/<ORG>/settings/apps/new`
- Personal: `https://github.com/settings/apps/new`

Fill in:

| Field | Value |
|---|---|
| **GitHub App name** | `docpreview` (must be globally unique — add your org if taken) |
| **Description** | What it does, in one line. This is on the install screen, which is where somebody decides whether to trust it |
| **Homepage URL** | Your docs site, not the repository. Somebody clicking it wants to know what this is |
| **Webhook** | ✅ Active |
| **Webhook URL** | The URL from step 1, e.g. `https://docpreview.shares.zrok.io/webhook/github` |
| **Webhook secret** | The value already in the vault under `github.webhook_secret`. It must match exactly. |

:::tip Own it with the organization, and give it an avatar

**Owner.** An App owned by a person cannot be moved to an organization without the org accepting a transfer, and
the account that owns it is the account that can rotate its private key. Create it under the organization from the
start.

**Avatar.** The App's name and picture are the author of every comment it writes, on every pull request anybody
reviews. The default identicon reads as somebody's experiment. Upload a square image, 200×200 or larger, with no
transparency — GitHub composites it onto white in some places and dark in others. It has to be legible at **20
pixels**, which is the size on a comment and the only size most people ever see it at, so anything with a word in
it is a grey smudge.

:::

If you have not generated a secret yet, do it now, paste it into this field, and store the same value in the vault
before running `webhook-check`:

```powershell
[Convert]::ToBase64String((1..32 | ForEach-Object { Get-Random -Max 256 }))
```

```bash
openssl rand -base64 32
```

:::danger Do not leave the webhook secret blank

Without it, `X-Hub-Signature-256` is absent and anyone who finds your URL can post a forged `pull_request`
payload — which is a request to clone and build a repository of their choosing. docpreview rejects unsigned
deliveries outright, so a blank secret means nothing works — that is deliberate.

:::

## Step 3 — permissions

Under **Repository permissions**, set exactly these and nothing else:

| Permission | Access | Why |
|---|---|---|
| **Contents** | Read-only | Clone the branch |
| **Pull requests** | Read and write | Post and edit the preview comment |
| **Checks** | Read and write | Write the check run |
| **Metadata** | Read-only | Mandatory — GitHub adds it automatically |

Leave every other permission at **No access**. Anything more is scope you did not need and now have to
justify.

## Step 4 — events and installation scope

Under **Subscribe to events**, tick:

- ✅ **Pull request**
- ✅ **Push** — only if you want the permanent preview of the default branch kept current

Nothing else. docpreview ignores every other event, and over-subscribing wastes deliveries.

**Push** is optional and does one thing: when the default branch moves, docpreview rebuilds its
branch preview. Without it that preview is refreshed only when the daemon restarts or somebody presses
Rebuild, so it shows whatever `main` looked like the last time one of those happened.

A push to any *other* branch is ignored — a branch with an open pull request already arrives as
`synchronize`, and one without is somebody's work in progress. A push to a repository that has no
branch preview is also ignored: pushes refresh a preview, they never create one.

Under **Where can this GitHub App be installed?** choose **Only on this account** unless you have a reason
not to.

Click **Create GitHub App**.

## Step 5 — collect the three secrets

You land on the App's settings page.

**a. App ID.** Near the top, a number like `1234567`. Put it in your server config:

```yaml
github:
  app_id: 1234567
```

**b. Webhook secret.** Already stored, if you followed step 1. If not:

```powershell
"paste-the-secret-here" | docpreview vault set github.webhook_secret
```

Then re-run `docpreview webhook-check -url <your webhook URL>` — a `401` there is the same mismatch GitHub will
report, found without waiting for a delivery.

**c. Private key.** Scroll to **Private keys** → **Generate a private key**. A `.pem` file downloads. Then:

```powershell
docpreview vault set github.private_key -file "$env:USERPROFILE\Downloads\docpreview.2026-07-27.private-key.pem"
Remove-Item "$env:USERPROFILE\Downloads\docpreview.*.private-key.pem"
```

```bash
docpreview vault set github.private_key -file ~/Downloads/docpreview.2026-07-27.private-key.pem
rm ~/Downloads/docpreview.*.private-key.pem
```

Delete the download. That file is the App's identity: anyone holding it can mint installation tokens for
every repository the App is installed on.

Confirm all three landed:

```powershell
docpreview vault list
```

```text
github.private_key
github.webhook_secret
```

The values are never printed — only the names.

## Step 6 — install it

**Install App** in the left sidebar → pick your account or organization → **Only select repositories** → pick
your documentation repository → **Install**.

Selecting individual repositories rather than "All repositories" is worth the extra click. The App can clone
anything it is installed on.

## Step 7 — verify

```powershell
docpreview doctor
```

```text
config:  C:\Users\you\.docpreview\config.yml
data:    C:\Users\you\.docpreview
vault:   C:\Users\you\.docpreview\vault.age (2 secrets)
exposer: zrok2
driver:  local

all checks passed
```

`doctor` asks GitHub who the App is and asks zrok whether the environment is still valid, so a pass means both
sets of credentials genuinely work — not merely that they are present.

Then start it:

```powershell
docpreview serve
```

Open a pull request that touches a file under `docs/`. Within a minute or so you should see a comment.

## Verifying the round trip

If nothing appears, GitHub tells you exactly where it stopped. On the App settings page, **Advanced →
Recent Deliveries** lists every webhook with its request and response.

| What you see | What it means |
|---|---|
| No delivery listed | The event never fired. Check the event subscription and that the App is installed on *this* repository. |
| Delivery with a red ❌ and no response | The URL is unreachable. Is `docpreview webhook-only` still running? |
| `401 unauthorized` | Signature mismatch. The webhook secret in the App and in the vault differ. |
| `404 not found` | Wrong path. It must be exactly `/webhook/github`. |
| `502 bad gateway` | The tunnel is up, the daemon behind it is not. See [502](./webhook-tunnel.md#what-a-502-means). |
| `202 accepted`, no comment | docpreview took it. Look at the server log — most likely the change was skipped as non-documentation. |

Every delivery has a **Redeliver** button, so you can iterate without pushing more commits.

Opening the webhook URL in a browser is not a test — it answers `405`, because a browser sends `GET` and GitHub
sends `POST`. A 405 there means the URL is right. See
[GET answers 405](./webhook-tunnel.md#get-on-the-webhook-url-answers-405-that-is-correct).

## Rotating the private key

Rotate on a schedule, and immediately if the `.pem` was ever somewhere it should not have been.

1. App settings → **Private keys** → **Generate a private key**.
2. `docpreview vault set github.private_key -file <new.pem>`
3. Restart `docpreview serve`.
4. Back on GitHub, **delete the old key**.

GitHub allows multiple valid keys at once, so steps 1–3 are zero-downtime. Step 4 is the one that matters and
the one people forget.

## Related

- [Expose the webhook](./webhook-tunnel.md)
- [Configuration reference](../reference/configuration.md)
- [Security model](../reference/security.md)
- [Troubleshooting](../troubleshooting.md)
