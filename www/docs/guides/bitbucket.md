---
id: bitbucket
title: Connect Bitbucket Cloud
sidebar_position: 2
---

# Connect Bitbucket Cloud

You need a token, a webhook secret, the webhook itself, a line of config and a project row. Bitbucket has no App to
install, so everything here is per repository or per workspace, and there is no install screen to click through.

:::tip Adding another repository to a daemon that already serves Bitbucket

Follow [Add a private Bitbucket repository](./bitbucket-add-repository.md) instead. It reuses the existing webhook
secret, which Step 3 below would replace.

:::

## Step 1: Create an access token, and name it carefully

Repository settings → **Access tokens** → **Create Repository Access Token**.

:::danger The token's name is the comment author

Bitbucket posts the preview comment as the token and shows the token's **name** beside it on every pull request. A
token called `test` or `mytoken2` puts that name on every review your team does. Renaming it means creating a new
token and storing it again.

Name it `NetFoundry Preview`.

:::

It needs two scopes, and no more:

| Scope | For |
|---|---|
| **Repositories: Read** | Cloning the branch, and reading which files a pull request changed |
| **Pull requests: Write** | Posting and editing the one preview comment |

A **project** or **workspace** access token also works, and covers every repository under it. Once you have more
than a few repositories that is worth it. It is also a wider credential in a process that runs builds (see
[Security](../reference/security.md)), so limit it to the repositories that need it.

Copy the token now. Bitbucket shows it once.

## Step 2: Store the token

To share it across the workspace, so every Bitbucket project can use it:

```powershell
docpreview vault set bitbucket.access_token
```

A repository-scoped token belongs to one project instead. Paste it on that project's page after you add the project
in Step 6. A project with its own token ignores the workspace one, a project with none falls back to it, and the
panel says which one is in use.

## Step 3: Generate a webhook secret

Generate the secret in NetFoundry Preview and paste it into Bitbucket, not the other way round. A value you make up
is a value you typed somewhere first.

On the **Settings** page, find `bitbucket.webhook_secret` and press **Generate**. The value appears once, in the
response to the call that created it, and you can't ask for it again. Copy it before you leave the page.

Or from a terminal:

```powershell
docpreview vault set bitbucket.webhook_secret     # reads the value from stdin
```

## Step 4: Turn Bitbucket on

The Bitbucket client only starts when the config says so. Storing a credential never switches it on. Add this to the
server config and restart the daemon:

```yaml
bitbucket:
  enabled: true
  auth: access_token
```

Without `enabled: true`, `/webhook/bitbucket` answers 501.

## Step 5: Add the webhook

The webhook tunnel forwards only `/webhook/github` unless you tell it otherwise. Pass both paths:

```powershell
docpreview webhook-only -zrok-name docpreview -upstream http://127.0.0.1:8471 `
  -path /webhook/github -path /webhook/bitbucket
```

The packaged `docpreview-webhook` service already passes both. The tunnel logs one
`this is the webhook URL to configure` line per path, and the `platform=bitbucket` one is the URL for this step. See
[Expose the webhook](./webhook-tunnel.md).

Repository settings → **Webhooks** → **Add webhook**.

| Field | Value |
|---|---|
| Title | `NetFoundry Preview` |
| URL | The Bitbucket URL your webhook tunnel printed, such as `https://docpreview.shares.zrok.io/webhook/bitbucket` |
| Secret | The value from Step 3 |
| Status | Active |

Triggers:

- ✅ **Pull request → Created**
- ✅ **Pull request → Updated**
- ✅ **Pull request → Merged**
- ✅ **Pull request → Declined**
- ✅ **Repository → Push**, only if you want the main branch's preview kept current

Nothing else. Extra triggers only send deliveries that get ignored.

:::note What `Updated` means here

Bitbucket fires it for a title change, a description edit, a reviewer change and a retarget, as well as for new
commits. So fixing a typo in a description rebuilds the site. A newer build cancels the one in flight, so the cost is
a wasted `npm install`, not a wrong preview.

:::

## Step 6: Add the project

On the **Projects** page, press **New project** and paste the repository URL. If a credential can reach the
repository, adding it builds the default branch and queues every open pull request, so there is something to look at
before anyone pushes. For a private repository with only a per-project token, both happen when you save that token
on the project's page.

## Check it

```powershell
docpreview doctor
```

`scm: bitbucket (access_token)` means Bitbucket is enabled, and a `not wired up` suffix means the vault is locked.
`doctor` checks neither the token nor the secret. If the secret is missing, the daemon log warns
`bitbucket is enabled but its credentials are not complete yet`. To test the token, use **Test credential** on the
project's page. Then open a pull request, or push to one, and watch:

```powershell
journalctl -u docpreview -f     # or the daemon's own output
```

| What you see | What it means |
|---|---|
| `401` on the delivery | The secret in Bitbucket and the one in the vault differ |
| `404` on the delivery | The webhook tunnel is not forwarding `/webhook/bitbucket`. Add `-path /webhook/bitbucket` |
| `501` on the delivery | `bitbucket.enabled` is not set, or the vault is locked |
| `ignoring bitbucket event` | A trigger that is not acted on. Check the list above. This line only appears with `-log-level debug` |
| `refusing to build a fork pull request` | Working as intended |
| Nothing at all | The delivery never arrived. Bitbucket's webhook page has a **View requests** tab |

## What is different from GitHub

| | |
|---|---|
| The commit hash | Deliveries carry an abbreviated 12-character hash, which the daemon resolves to the full one. A short hash checks out fine and then fails every comparison |
| The credential | A token you create and paste, not an App installation. Nothing proves it reaches a repository until something tries, which is why the project form has a **Test credential** button |
| Fork detection | The source repository's `full_name` differs from the destination's. Same rule, different field |
