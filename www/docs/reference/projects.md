---
id: projects
title: Projects
sidebar_position: 3
---

# Projects

A **project** is a repository docpreview is willing to build, plus the operator's answers about how. It lives in
the daemon's database and is managed at `http://127.0.0.1:8471/projects`.

Projects are optional. A repository the GitHub App is installed on builds from its own
[`.docpreview.yml`](./repo-config.md) with no project row at all. Adding one is how you take a decision out of the
branch.

## Why a project wins over the repository

`.docpreview.yml` arrives in the pull request, so on any repository where opening one is not a privilege, its
author chooses what the file says. A project row is yours. Where both state a field, the project's answer is used.

Every build field on a project may be left blank, and blank means "defer to the repository". A row that names only
a repository is valid and is the common case: it says the repository is allowed, not that you want to restate its
configuration.

## The page

One card per project. The card shows what the project itself decides — a field it does not state is absent rather
than shown as a default, because the card is for your decisions and not for the full space of settings.

| Control | |
|---|---|
| **Build** | Expands the build settings for that project, in place |
| **Secrets** | Expands its environment variables, with a count when it has any |
| **Delete** | Removes the row. Existing previews are left alone |
| **Disable** | Inside **Build**. A disabled project stops building; the row and its settings stay |

One panel is open at a time. **New project** opens the same form the Build panel uses, because a save is an upsert
and one form means the two cannot drift apart.

:::note Writes are local-only

The page is readable from anywhere the dashboard is, and every button that changes something is refused unless the
request came from the machine running docpreview — loopback address, and no forwarding header. A project decides
what command runs on the build host, which is a more direct route to executing code there than a credential is.
See [Security](./security.md).

:::

## Environment variables scoped to a project

A documentation site that assembles content from several private repositories needs a credential per source, and
those credentials are not the same for every project on the daemon. So they belong to the project.

A variable added here is:

- **injected into every build of that repository**, under the name you gave it;
- **redacted from every build log** and from the text of every error, so a build that prints its own environment —
  which npm does on failure, and any script run under `set -x` does always — produces asterisks;
- **stored in the vault**, under a namespace derived from the project, so two projects can use the same variable
  name with different values;
- **never readable back**. Nothing in the API or the UI returns a value. Removing one means finding the token
  again wherever it came from.

The name must be upper-case letters, digits and underscore, cannot start with a digit, and is capped at 128
characters. That is the form a shell can read: a name with a dot or a dash in it can be set through Go's process API
and never read by `sh`, so you would store a value the build could not see.

### What this is for

A build script that clones other repositories dispatches on a variable per source:

```js
if (url.includes("customer-connect-docs")) {
  if (process.env.BB_REPO_TOKEN_CUSTOMER_CONNECT) {
    return `https://x-token-auth:${process.env.BB_REPO_TOKEN_CUSTOMER_CONNECT}@bitbucket.org/…`;
  }
  return "git@bitbucket.org:netfoundry/customer-connect-docs.git";  // falls back to SSH
}
```

The daemon has no SSH key and no access to a private repository other than the one the webhook came from, so
without the variable that clone falls back to SSH and the build fails. With it, the build clones over HTTPS using a
token you control and can revoke.

### Server-wide variables

`build.secrets` in [the server config](./configuration.md) names variables every project gets. The Secrets panel
shows those greyed, as inherited, because "this project has no variables" and "this project has none of its own"
look identical otherwise and only one of them means a build is about to fail.

A project naming the same variable as the server config wins, which is the same precedence every other field
follows.

:::danger A variable is a credential in a process that runs a build

Under the **local** driver a build runs on the host as the daemon's user, and these variables are in its
environment. Under **docker** they are passed into the container. Either way, anything the build executes can read
them — including a `postinstall` script from a dependency. Scope a token to the repository it needs and nothing
more: a Bitbucket repository access token rather than an account token, and read-only where the platform offers it.

:::

## Precedence, end to end

For any one build:

1. The **project row** decides driver, image, build directory, command, output, base URL and the ignore-build
   script, for every field it states.
2. **`.docpreview.yml`** in the pull request decides the rest.
3. In the environment, `build.env` from the repository is applied first and **cannot set** a variable docpreview
   reserves, a variable from `build.secrets`, or one of the project's own. The rest are applied over it: server-wide
   variables, then the project's, which wins on a name they share.

A repository being unable to shadow one of these matters twice over. It cannot watch what the build does with a
value of its own, and — the reason it is enforced rather than documented — it cannot replace a value the redactor
knows about with one it does not, which would put the substituted string into the log.
