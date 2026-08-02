---
id: bitbucket
title: Connect Bitbucket Cloud
sidebar_position: 2
---

# Connect Bitbucket Cloud

Four steps: a token, a webhook secret, the webhook, and a project row. Bitbucket has no App to install, so
everything here is per repository or per workspace and there is no click-through to sit out.

## Step 1 — Create an access token, and name it carefully

Repository settings → **Access tokens** → **Create Repository Access Token**.

:::danger The token's name is the comment author

Bitbucket posts the preview comment as the token, and shows the token's **name** beside it on every pull request.
A token called `test` or `mytoken2` puts that on every documentation review your team does, and renaming it means
creating a new one and re-storing it.

Call it `docpreview`.

:::

Scopes — two, and no more:

| Scope | For |
|---|---|
| **Repositories: Read** | Cloning the branch, and reading which files a pull request changed |
| **Pull requests: Write** | Posting and editing the one preview comment |

A **project** or **workspace** access token works too and covers every repository under it, which is worth it once
you have more than a few. It is also a wider credential in a process that runs a build — see
[Security](../reference/security.md) — so scope it to the repositories that need it.

Copy the token now. Bitbucket shows it once.

## Step 2 — Store the token

Workspace-wide, so every Bitbucket project can use it:

```powershell
docpreview vault set bitbucket.access_token
```

Or per project, on the **Projects** page, which is what a repository-scoped token wants. A project with its own
token ignores the workspace one. A project with none falls back to it, and the panel says which is happening
rather than leaving you to guess.

## Step 3 — Generate a webhook secret

Generate it in docpreview and paste it into Bitbucket, not the other way round: a value you invent is a value you
typed somewhere first.

On the **Settings** page, find `bitbucket.webhook_secret` and press **Generate**. It is shown exactly once, in the
response to the call that minted it, and there is no way to ask again — copy it before you leave the page.

From a terminal instead, if you would rather:

```powershell
docpreview vault set bitbucket.webhook_secret     # reads the value from stdin
```

## Step 4 — Add the webhook

Repository settings → **Webhooks** → **Add webhook**.

| Field | Value |
|---|---|
| Title | `docpreview` |
| URL | `https://docpreview.shares.zrok.io/webhook/bitbucket` — the URL your webhook tunnel printed |
| Secret | The value from Step 3 |
| Status | Active |

Triggers:

- ✅ **Pull request → Created**
- ✅ **Pull request → Updated**
- ✅ **Pull request → Merged**
- ✅ **Pull request → Declined**
- ✅ **Repository → Push** — only if you want the preview of the main branch kept current

Nothing else. Over-selecting costs deliveries docpreview ignores.

:::note What `Updated` means here

Bitbucket fires it for a title change, a description edit, a reviewer change and a retarget, not only for new
commits — so a typo fix in a description rebuilds the site. That is accepted rather than filtered: suppressing it
means remembering the last commit built per pull request, and the machinery to absorb the churn already exists —
a newer build cancels the one in flight. The cost is a wasted `npm install`, not a wrong answer.

:::

## Step 5 — Add the project

On the **Projects** page, **New project**, and paste the repository URL. Adding it queues every open pull request,
so there is something to look at without waiting for somebody to push.

## Check it

```powershell
docpreview doctor -config .docpreview\config.yml
```

`scm: bitbucket (access_token)` means the credential is loaded. Then open a pull request, or push to one, and
watch:

```powershell
journalctl -u docpreview -f     # or the daemon's own output
```

| What you see | What it means |
|---|---|
| `401` on the delivery | The secret in Bitbucket and the one in the vault differ |
| `ignoring bitbucket event` | A trigger docpreview does not act on — check the list above |
| `refusing to build a fork pull request` | Working as intended |
| Nothing at all | The delivery never arrived. Bitbucket's webhook page has a **View requests** tab |

## What is different from GitHub

| | |
|---|---|
| The comment marker | A link reference definition, `[docpreview]: #<id>`, because Bitbucket renders raw HTML comments as visible text |
| The commit hash | Deliveries carry an abbreviated 12-character hash, which docpreview resolves to the full one — a short hash checks out fine and then fails every comparison |
| The credential | A token you create and paste, not an App installation. Nothing has verified it reaches a repository until something tries, which is why the project form has a **Test credential** button |
| Fork detection | The source repository's `full_name` differing from the destination's. Same rule, different field |
