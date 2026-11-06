---
id: vercel
title: How Vercel previews work
sidebar_position: 1
---

# How Vercel previews work

Vercel showed how good the preview loop can be: push a branch, get a link, review the real site. NetFoundry Preview
copies that loop. If Vercel works for you, keep using it. If you need the loop on your own machines, this page shows
what was copied and what was changed. The description of Vercel comes from its own documentation.

## The Git integration

**Vercel for GitHub** is a GitHub App you install on a repository. Bitbucket, GitLab and Azure DevOps have their own
versions. The App lets Vercel read the repository and sends it a webhook for everything that happens there.

From then on, every branch push and every pull request produces a **preview deployment**. Vercel clones the commit,
detects the framework, runs its build, and uploads the static output to its CDN under a unique generated URL. Preview
deployments get their own environment variables, separate from production.

## The write-back

Vercel reports back to the pull request in two ways.

**The Deployments API.** Vercel creates a GitHub deployment and updates its status as the build progresses. That is
what puts the "View deployment" box in the pull request timeline and the entry in the repository's Environments tab.
Because it is structured data, other tools can consume it. A Slack integration can post it, and a required status
check can gate on it.

**The bot comment.** Within about a minute of the push, the Vercel bot comments on the pull request with the preview
link and the build status. On later pushes it **edits that comment** instead of posting a new one.

That edit is the detail worth copying. A bot that posts a fresh comment on every push buries the review conversation
under identical status updates by the fifth commit. One comment that stays current is what keeps people from turning
the bot off.

## The lifecycle

- **Push to a branch** → a new preview deployment at a new immutable URL.
- **Open a pull request** → the preview is linked from the PR.
- **Merge** → the merge commit triggers a production deployment.
- **Close or merge** → the preview is cleaned up.

## What NetFoundry Preview copies, and what it changes

| Vercel | NetFoundry Preview |
|---|---|
| GitHub App / Bitbucket app for auth and events | Same |
| Clone the commit, run the framework build | Same, for any build that produces static files |
| Upload static output to a global CDN | Serve it in-process from disk |
| A new immutable URL per deployment, plus a branch URL | One **stable URL per branch**, reused across rebuilds |
| Bot comment edited in place | Same, found by a hidden marker in the body |
| GitHub Deployments API | GitHub Check Runs, Bitbucket build statuses |
| Preview environment variables | `build.env` in `.docpreview.yml`, plus secrets from the operator's vault |
| Merge promotes to production | The default branch keeps a preview of its own. Production is your pipeline's job. |

Two of those need an explanation.

**One URL per branch.** Vercel keeps every deployment as an immutable artifact at its own URL, which is handy for
comparing two commits. NetFoundry Preview gives each branch one zrok share and keeps it. A rebuild swaps the files
behind the share, so the link a reviewer bookmarked on Monday still works on Thursday. A share pinned to each commit
is available too. It is off by default because every share creation counts against zrok's rate limit. See
[One share per branch, unless you ask for more](../exposers.md#one-share-per-branch-unless-you-ask-for-more).

**Check Runs, not the Deployments API.** A GitHub deployment claims that code is running somewhere, and the
Environments tab files it under the repository's release history. A preview is not a release. A check run says "this
commit was examined and here is the result", which is what happened.

## Sources

- [Deploying Git Repositories with Vercel](https://vercel.com/docs/git)
- [Deploying GitHub Projects with Vercel](https://vercel.com/docs/git/vercel-for-github)
- [Preview Deployments](https://vercel.com/academy/svelte-on-vercel/preview-deployments)
