---
id: projects
title: Projects
sidebar_position: 3
---

# Projects

A **project** is a repository NetFoundry Preview is allowed to build, plus your answers about how to build it. Projects
live in the daemon's database and you manage them at `http://127.0.0.1:8471/projects`.

**A repository with no project is never built.** A webhook delivery for one is verified, answered with a `202` and
dropped. Nothing is cloned, queued or written back to the pull request. That is what makes it safe to run an App
anybody can install. The App has one webhook secret, so GitHub signs every installation's deliveries with it, and a
valid signature only proves GitHub sent the delivery. The project is where you say the repository is yours.

Every build field on a project can be blank. Blank means "use the repository's own
[`.nfpreview.yml`](./repo-config.md)". A project that names only a repository is valid and is the usual case. It
allows the repository without restating its config.

So installing the App on a repository takes two steps: install it, then add the project. The order doesn't matter. A
delivery that arrives before the project exists is dropped, and the next push builds.

## Why a project wins over the repository

`.nfpreview.yml` comes in with the pull request. On a repository where anyone can open one, the author of the pull
request decides what the file says. The project is yours, so when both set a field, the project wins.

## The page

There is one card per project. The card only shows what the project sets. A field the project leaves blank is not
shown as a default, because the card is there to show your decisions.

| Control | |
|---|---|
| **Edit** | Expands the build settings for that project, in place |
| **Settings** | Expands its environment variables, with a count when it has any |
| **Delete** | Removes the project. Existing previews are left alone |
| **Disable** | Inside **Edit**. A disabled project stops building. The project and its settings stay |
| **Open ↗** | On the branch strip. Goes to this project's default-branch preview |
| **Rebuild** | On the branch strip. Builds the default branch again, at its current tip |
| **Link an unlinked PR…** | Appears only when the project is skipping something. See below |

One panel is open at a time. **New project** opens the same form as **Edit**. A save is an upsert, and one form means
the two can't drift apart.

## The default branch is always previewed

Adding a project builds its default branch right away, and that preview is permanent. It sits first on the card
because it's the link people want: every other preview belongs to a pull request that will be merged and forgotten.
This one is the current state of the site.

The branch name comes from the platform, so a repository on `master` gets `master`.

It differs from a pull request's preview in four ways:

- **It never comments.** There is no pull request to comment on.
- **Closing a pull request can't remove it**, even one whose head is that same branch.
- **It doesn't expire.** The preview TTL is for pull requests that are done. `main` is still `main` after two quiet
  weeks.
- **Rebuild takes the branch's current tip.** A pull request's rebuild repeats the commit under review. This one
  builds whatever the branch points at now.

A push to the default branch rebuilds the preview. Pushes to other branches are ignored: a branch with a pull request
already builds from the pull request event, and a branch without one is someone's work in progress. A push only
refreshes a branch preview that already exists. It never creates one.

A project with no branch preview yet says so and offers to build one. That happens when the project was added while
the platform was unreachable, or when a private repository was added before it had a token. In that case, saving the
project's first token builds the default branch and scans for open pull requests. At startup the daemon also builds
any missing branch preview and retries any that last failed.

## Pull requests you do not want built

**Unlink** on a preview removes it and records that you don't want it. After that, pushes, webhook deliveries and
project scans all skip that pull request. Without the record, the next delivery would rebuild the preview and it would
look like the removal failed.

The project's card then says how many pull requests it is skipping, and **Link an unlinked PR…** opens a picker. It
has one row per unlinked pull request, with its branch and when it was unlinked. Linking one builds it right away and
stops skipping it, in the same request, so there's nothing to undo later.

A relinked pull request gets the same URL and a fresh comment. Its build history does not come back. The logs were
deleted with the preview, so the first build after linking is a cold one.

:::note Who can change a project

Anyone who can reach the dashboard can read this page. A change goes through only from a signed-in admin, or with
no session, from the machine running NetFoundry Preview: a loopback address and no forwarding header. A project
decides what command runs on the build host, which is a more direct route to running code there than a credential.

**Rebuild** is looser. Any signed-in user can press it, viewers too, because it runs the command the project or the
repository already set. See [Security](./security.md).

:::

## Environment variables scoped to a project

A site that pulls content from several private repositories needs a credential for each source, and those credentials
differ from project to project. So they belong to the project.

A variable added here is:

- **injected into every build of that repository**, under the name you gave it.
- **redacted from every build log** and from the text of every error. npm prints its environment when it fails, and
  so does any script run under `set -x`. Both print asterisks instead of the value.
- **stored in the vault**, under a namespace for the project, so two projects can use the same name with different
  values.
- **never readable back**. Nothing in the API or the UI returns a value. If you lose it, you get the token again from
  wherever it came from.

The name must be upper-case letters, digits and underscores, can't start with a digit, and is at most 128 characters.
That is what a shell can read. Go's process API will set a name with a dot or a dash in it, but `sh` can't read it,
so the build would never see the value.

### What this is for

A build script that clones other repositories picks a variable for each source:

```js
if (url.includes("customer-connect-docs")) {
  if (process.env.BB_REPO_TOKEN_CUSTOMER_CONNECT) {
    return `https://x-token-auth:${process.env.BB_REPO_TOKEN_CUSTOMER_CONNECT}@bitbucket.org/…`;
  }
  return "git@bitbucket.org:netfoundry/customer-connect-docs.git";  // falls back to SSH
}
```

The daemon has no SSH key, and it can only reach the private repository the webhook came from. Without the variable,
that clone falls back to SSH and the build fails. With it, the build clones over HTTPS with a token you control and
can revoke.

### Server-wide variables

`build.secrets` in [the server config](./configuration.md) names variables every project gets. The Secrets panel shows
them greyed out as inherited. Without that, "this project has no variables" and "this project has none of its own"
would look the same, and only the first one means a build is about to fail.

If a project sets the same variable as the server config, the project wins, like every other field.

:::danger A variable is a credential inside a build

Under the **local** driver, a build runs on the host as the daemon's user and these variables are in its environment.
Under **docker** they are passed into the container. Either way, anything the build runs can read them, including a
`postinstall` script from a dependency. Scope each token to the one repository it needs: a Bitbucket repository
access token rather than an account token, and read-only where the platform offers it.

:::

## Precedence, end to end

For any one build:

1. The **project** decides driver, image, build directory, command, output, base URL and the ignore-build script, for
   every field it sets.
2. **`.nfpreview.yml`** in the pull request decides the rest.
3. For the environment, the repository's `build.env` goes first. It **can't set** a variable NetFoundry Preview
   reserves, a variable from `build.secrets`, or one of the project's own. Then the server-wide variables go on top,
   then the project's, which win on a shared name.

A repository can't shadow any of these, for two reasons. It can't see what the build does with a value it supplied.
More importantly, it can't swap a value the redactor knows about for one it doesn't, which would print the swapped-in
value in the log. That second reason is why the rule is enforced in code rather than just written down.
