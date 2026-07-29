---
id: vercel
title: How Vercel previews work
sidebar_position: 1
---

# How Vercel previews work

docpreview is a deliberate imitation, so it is worth being precise about what is being imitated. This is the
mechanism, as documented by Vercel, stripped of the parts that only matter at their scale.

## The Git integration

You install **Vercel for GitHub** — a GitHub App — on a repository, or the Bitbucket / GitLab / Azure DevOps
equivalent. The App grants Vercel two things: permission to read the repository, and a webhook feed of what
happens in it.

From then on, every branch push and every pull request produces a **preview deployment**. Vercel clones the
commit, detects the framework, runs its build, and uploads the static output to their CDN under a unique
generated URL. Preview deployments get their own environment variables, separate from production.

## The write-back

Two channels, and they do different jobs.

**The Deployments API.** Vercel creates a GitHub deployment and updates its status as the build progresses.
This is what produces the "View deployment" box in the pull request timeline and the entry in the repository's
Environments tab. It is structured data, so other tooling — a Slack integration, a required status check — can
consume it.

**The bot comment.** Within about a minute of the push, the Vercel bot leaves a comment on the pull request
containing the preview link and the build status. On subsequent pushes it **edits that comment** rather than
posting a new one.

That editing behavior is the detail worth stealing. A repository with a chatty CI bot that posts a fresh
comment per push becomes unusable after the fifth commit — the actual review conversation is buried under
identical status updates. One comment that stays current is the difference between a tool people tolerate and
one they turn off.

## The lifecycle

- **Push to a branch** → new preview deployment at a new immutable URL.
- **Open a pull request** → the preview is linked from the PR.
- **Merge** → the merge commit triggers a production deployment.
- **Close or merge** → the preview is cleaned up.

## What docpreview copies, and what it changes

| Vercel | docpreview |
|---|---|
| GitHub App / Bitbucket app for auth and events | Same |
| Clone the commit, run the framework build | Same, but only static-site builds |
| Upload static output to a global CDN | Serve it in-process from disk |
| A new immutable URL per deployment | A **stable URL per branch**, reused across rebuilds |
| Bot comment edited in place | Same, found by a hidden marker in the body |
| GitHub Deployments API | GitHub Check Runs |
| Preview environment variables | Repo-level `.docpreview.yml`, no secrets |
| Merge promotes to production | Nothing. Production is your pipeline's job. |

Two of those deserve an explanation.

**Stable URL per branch, not per deployment.** Vercel mints a new URL for every deployment because their
previews are immutable artifacts you might want to compare. docpreview instead attaches the branch name to a
persistent zrok name and rebinds it on each rebuild, so the link a reviewer bookmarked on Monday still works
on Thursday. That is only possible because zrok v2 decoupled names from shares — see
[Exposers](../exposers.md).

**Check Runs, not the Deployments API.** A GitHub deployment is a claim that code is running somewhere; the
Environments UI treats it as part of the repository's release history. A documentation preview is not a
release. A check run says "this commit was examined and here is the result", which is what actually happened.

## Sources

- [Deploying Git Repositories with Vercel](https://vercel.com/docs/git)
- [Deploying GitHub Projects with Vercel](https://vercel.com/docs/git/vercel-for-github)
- [Preview Deployments](https://vercel.com/academy/svelte-on-vercel/preview-deployments)
