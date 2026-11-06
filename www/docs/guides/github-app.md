---
id: github-app
title: Create the GitHub App
sidebar_position: 1
---

# Create the GitHub App

Nobody can automate this part for you, because creating a GitHub App takes a person in a browser. Allow ten
minutes. At the end you will have two secrets in the vault, the App ID in your config, and a working webhook.

## Before you start

- Admin rights on the organization (or a personal account, for a personal repository).
- `docpreview` built and on your PATH.
- A vault master key. The vault is one [age](../background/age.md)-encrypted file at `~/.docpreview/vault.age`. If
  you have not made a key:

  ```powershell
  docpreview vault keygen -out C:\ProgramData\docpreview\master.key
  ```

  ```bash
  docpreview vault keygen -out /etc/docpreview/master.key
  ```

  Then point the daemon at it, so it can unlock itself after a restart:

  ```yaml
  vault:
    key_source: "file:/etc/docpreview/master.key"
  ```

  The key never reaches your clipboard or your shell history. **Back it up somewhere other than this machine.** It
  is the only thing that decrypts the vault. If you would rather keep no key on disk at all, skip this. `serve` then
  starts with a locked vault and you unlock it from the dashboard, which means a person is needed after every
  restart. See [the master key](../reference/security.md#the-master-key).

## Step 1: Get a public webhook URL first

GitHub checks the webhook URL when you save the App, so get it working before you start clicking. That has a guide
of its own: **[expose the webhook](./webhook-tunnel.md)**. Work through it now and come back with two things.

1. A URL like `https://docpreview.shares.zrok.io/webhook/github`. It stays the same across restarts because it is
   bound to a reserved zrok name.
2. A `PASS` from `docpreview webhook-check -url <that URL>`. The command signs a `ping` with the secret from the
   vault, which proves the whole path already accepts a signed delivery.

Don't point a share at the daemon directly. `zrok2 share public http://127.0.0.1:8471` publishes every route,
including `/api/secrets`, which puts the credential API on the internet. `docpreview webhook-only` publishes one
route and nothing else. [That guide](./webhook-tunnel.md#why-not-share-the-daemon-directly) explains why.

That guide also has you store `github.webhook_secret` in the vault first, because `webhook-check` reads it from
there. Use the same value in step 2 below.

Leave `webhook-only` running in its own terminal.

## Step 2: Create the App

Go to **Settings → Developer settings → GitHub Apps → New GitHub App**.

- Organization: `https://github.com/organizations/<ORG>/settings/apps/new`
- Personal: `https://github.com/settings/apps/new`

Fill in:

| Field | Value |
|---|---|
| **GitHub App name** | `NetFoundry Preview`. App names are unique across GitHub, so if it is taken, add your org: `NetFoundry Preview (acme)` |
| **Description** | What it does, in one line. It appears on the install screen, where someone decides whether to trust it |
| **Homepage URL** | Your site, not the repository. Someone clicking it wants to know what this is |
| **Webhook** | ✅ Active |
| **Webhook URL** | The URL from step 1, e.g. `https://docpreview.shares.zrok.io/webhook/github` |
| **Webhook secret** | The value already in the vault under `github.webhook_secret`. It must match exactly |

:::tip Own it with the organization, and give it an avatar

**Owner.** An App owned by a person can only move to an organization if the org accepts a transfer, and the account
that owns it is the one that can rotate its private key. Create it under the organization from the start.

**Avatar.** The App's name and picture sit on every comment it writes, on every pull request anyone reviews. The
default identicon looks like somebody's experiment. Upload a square image, 200×200 or larger, with no transparency,
because GitHub puts it on white in some places and on dark in others. It has to read at **20 pixels**, the size on
a comment and the only size most people ever see. Anything with a word in it turns into a grey smudge.

:::

If you have not generated a secret yet, do it now. Paste it into this field, and store the same value in the vault
before running `webhook-check`:

```powershell
[Convert]::ToBase64String((1..32 | ForEach-Object { Get-Random -Max 256 }))
```

```bash
openssl rand -base64 32
```

:::danger Do not leave the webhook secret blank

Without a secret, GitHub sends no `X-Hub-Signature-256`, and anyone who finds your URL can post a forged
`pull_request` payload. That is a request to clone and build a repository of their choosing. The daemon rejects
unsigned deliveries outright, so with a blank secret nothing works, on purpose.

:::

## Step 3: Permissions

Under **Repository permissions**, set exactly these and nothing else:

| Permission | Access | Why |
|---|---|---|
| **Contents** | Read-only | Clone the branch |
| **Pull requests** | Read and write | Post and edit the preview comment |
| **Checks** | Read and write | Write the check run |
| **Metadata** | Read-only | Mandatory. GitHub adds it automatically |

Leave every other permission at **No access**. Anything more is scope you did not need and now have to justify.

## Step 4: Events and installation scope

Under **Subscribe to events**, tick:

- ✅ **Pull request**
- ✅ **Push**, only if you want the default branch's permanent preview kept current

Nothing else. Every other event is ignored, and extra subscriptions only waste deliveries.

**Push** is optional and does one thing: when the default branch moves, the daemon rebuilds that branch's preview.
Without it the preview only refreshes when someone presses Rebuild, so it shows `main` as of its last build. A
restart rebuilds it only if its last build failed.

A push to any *other* branch is ignored. A branch with an open pull request already arrives as `synchronize`, and a
branch without one is somebody's work in progress. A push to a repository with no branch preview is ignored too.
Pushes refresh a preview and never create one.

Under **Where can this GitHub App be installed?** choose **Only on this account** unless you have a reason not to.

Click **Create GitHub App**.

## Step 5: Collect the App ID and the secrets

You land on the App's settings page.

**a. App ID.** A number near the top, like `1234567`. It is not a secret, so it goes in your server config:

```yaml
github:
  app_id: 1234567
```

**b. Webhook secret.** Already stored, if you followed step 1. If not:

```powershell
"paste-the-secret-here" | docpreview vault set github.webhook_secret
```

Then run `docpreview webhook-check -url <your webhook URL>` again. A `401` there is the same mismatch GitHub would
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

Delete the download. That file is the App's identity. Anyone holding it can mint installation tokens for every
repository the App is installed on.

Confirm both secrets landed:

```powershell
docpreview vault list
```

```text
github.private_key
github.webhook_secret
```

It prints only the names, never the values.

## Step 6: Install it

**Install App** in the left sidebar → pick your account or organization → **Only select repositories** → pick the
repository whose site you want previewed → **Install**.

Picking individual repositories instead of "All repositories" is worth the extra click. The App can clone anything
it is installed on.

## Step 6b: Add the project

Installing the App is only half of it. **A repository with no project row is never built.** Its delivery is
verified, answered `202` and dropped, and the server log names the repository. Add the row at
`http://127.0.0.1:8471/projects`: **New project**, then the platform, owner and repository. Leave every build field
blank unless you want to override the repository's own `.docpreview.yml`.

:::info Why this is not automatic

The webhook secret is one value for the whole App, and GitHub signs every installation's deliveries with it. A
valid signature proves GitHub sent the delivery. It says nothing about whether you wanted that repository built.
Without the project row, anyone who could install the App would get a branch of their choosing cloned, and its
`.docpreview.yml` honoured, on the machine holding the App private key and every project's tokens.

The cost is that installing the App is not self-service. Someone with access to the dashboard has to add the row
before the first build.

Once the row exists, the daemon builds the default branch and scans the open pull requests. See
[Projects](../reference/projects.md).

:::

## Step 7: Verify

```powershell
docpreview doctor
```

```text
config:  C:\Users\you\.docpreview\config.yml
data:    C:\Users\you\.docpreview
exposer: zrok2
driver:  docker
listen:  tcp 127.0.0.1:8471
vault:   C:\Users\you\.docpreview\vault.age (2 secrets)
key:     file C:\ProgramData\docpreview\master.key
login:   required — either password reaches the dashboard
scm:     github (app 1234567)

all checks passed
```

`doctor` asks GitHub who the App is and asks zrok whether the environment is still valid. A pass means both sets of
credentials work, not just that they are present.

Then start it:

```powershell
docpreview serve
```

Open a pull request that changes a file inside the project's build directory, such as `docs/`. A pull request that
changes nothing there is skipped. Within a minute or so you should see a comment.

## Verifying the round trip

If nothing appears, GitHub shows you where it stopped. On the App settings page, **Advanced → Recent Deliveries**
lists every webhook with its request and response.

| What you see | What it means |
|---|---|
| No delivery listed | The event never fired. Check the event subscription, and that the App is installed on *this* repository. |
| Delivery with a red ❌ and no response | The URL is unreachable. Is `docpreview webhook-only` still running? |
| `401 unauthorized` | Signature mismatch. The webhook secret in the App and in the vault differ. |
| `404 not found` | Wrong path. It must be exactly `/webhook/github`. |
| `502 bad gateway` | The tunnel is up and the daemon behind it is not. See [502](./webhook-tunnel.md#what-a-502-means). |
| `202 accepted`, no comment | The daemon took it. Read the server log. Usually there is no project row for this repository (the log names it), or the pull request comes from a fork. |

A pull request that changes nothing in the build directory still gets a comment and a neutral check run, both
marked skipped.

Every delivery has a **Redeliver** button, so you can retry without pushing more commits.

Opening the webhook URL in a browser is not a test. It answers `405`, because a browser sends `GET` and GitHub
sends `POST`. A 405 there means the URL is right. See
[GET answers 405](./webhook-tunnel.md#get-on-the-webhook-url-answers-405-that-is-correct).

## Rotating the private key

Rotate it on a schedule, and right away if the `.pem` was ever somewhere it should not have been.

1. App settings → **Private keys** → **Generate a private key**.
2. `docpreview vault set github.private_key -file <new.pem>`
3. Restart `docpreview serve`.
4. Back on GitHub, **delete the old key**.

GitHub allows several valid keys at once, so steps 1–3 cause no downtime. Step 4 is the one that matters, and the one
people forget.

## Related

- [Expose the webhook](./webhook-tunnel.md)
- [Configuration reference](../reference/configuration.md)
- [Security model](../reference/security.md)
- [Troubleshooting](../troubleshooting.md)
