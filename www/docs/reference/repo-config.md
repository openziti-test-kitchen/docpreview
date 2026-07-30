---
id: repo-config
title: Repository configuration
sidebar_position: 2
---

# Repository configuration

`.docpreview.yml`, in the root of the repository being previewed. Entirely optional — the defaults handle a
stock Docusaurus site.

```yaml
build:
  dir: .
  command: npm run build
  output: build
  base_url: /
  env:
    ALGOLIA_APP_ID: "XYZ123"

detect:
  paths:
    - "docs/**"
    - "**/*.md"
    - "docusaurus.config.*"
  script: ""
```

:::danger This file comes from the pull request

Anyone who can open a pull request controls this file's contents. docpreview validates every field and
constrains what it can do:

- No path may escape the repository root — checked for absolute paths, `..` traversal, and Windows
  drive-qualified paths.
- `build.command` is honored under **both** drivers. Under `docker` the blast radius is a container. **Under
  `local` it is a shell command on the build host**, so on a repository where opening a pull request is not a
  privilege, run the Docker driver or put the repository behind a [project](./projects.md) that states the command.
- `build.env` cannot set `DOCUSAURUS_BASE_URL` or any other reserved variable.
- `detect.script` is resolved through symlinks and re-checked, because a symlink in an attacker-controlled
  working tree can point anywhere.

:::

## `build`

### `dir`

Where `package.json` lives, relative to the repository root. Default `.`.

For a repository whose site is in a subdirectory:

```yaml
build:
  dir: website
```

### `command`

Default `npm run build`. It runs after the dependency install, in `dir`, as one shell command.

:::danger This is arbitrary shell, from the pull request

Under the **local** driver the command runs on the build host as the daemon's user. Under **docker** it runs in the
container. Either way, whoever can open a pull request chooses it.

That is not as large a step as it sounds — `npm run build` already executes `package.json` scripts from the same
branch, so the local driver hands a contributor code execution regardless. It is why forks are refused at the webhook
and why the Docker driver exists. But do not read this field as constrained: it is not.

To take the decision out of the branch, set the command on a [project](./projects.md). A project's answer wins, and
a project row is the operator's.

:::

### Dependency install

There is no setting for this: the lockfile decides.

| Committed file | What runs |
|---|---|
| `yarn.lock` | `yarn install --frozen-lockfile --non-interactive` |
| `pnpm-lock.yaml` | `pnpm install --frozen-lockfile` |
| `package-lock.json` | `npm ci --no-audit --no-fund` |
| none of the above | `npm install --no-audit --no-fund` |

Running the wrong package manager is not merely slower. `npm ci` against a tree with only a `yarn.lock` fails
outright, and `npm install` there resolves a different dependency graph than the one the author tested. The
lockfile is the author's statement of which manager owns the tree, so it is the thing consulted.

Each of the first three falls back to a looser install if the strict flag is rejected — the flag differs by
major version, and refusing to build at all is worse than building from a drifted lockfile. The package
manager itself has to be on `PATH` under the local driver, or in the image under the Docker driver.

### `output`

The build output directory, relative to `dir`. Default `build`, which is what Docusaurus emits. Use `out` for
Next.js export, `public` for Hugo, `_site` for Jekyll.

### `base_url`

The path prefix the preview is served under. Default `/`.

This is passed into the build as `DOCUSAURUS_BASE_URL` **and** used as the mount point when serving, so the
two cannot drift apart. Any of these work:

```yaml
build:
  base_url: /          # the site is at the root of its hostname
```

```yaml
build:
  base_url: /docs/     # served at https://<name>.shares.zrok.io/docs/
```

```yaml
build:
  base_url: /zrok      # normalized to /zrok/
```

Leading and trailing slashes are added if missing, so `zrok`, `/zrok`, and `/zrok/` are the same thing.

The site is mounted at that prefix and a request to the bare origin is redirected there, so a reviewer who
clicks the hostname alone still lands on the site.

**Which value do you need?** If `docusaurus.config.ts` reads `process.env.DOCUSAURUS_BASE_URL`, any value
works and `/` is the nicest. If it hardcodes something, `base_url` must match it — or the build is refused.
See [Troubleshooting](../troubleshooting.md).

### `env`

Extra environment for the build. Literal values only; there is no vault lookup, because a pull request author
must not be able to name a secret and have it handed to a script they wrote.

Reserved names (`DOCUSAURUS_BASE_URL`, `DOCPREVIEW_BASE_URL`, `DOCPREVIEW`) are silently ignored here.

Variables docpreview always sets:

| Variable | Value |
|---|---|
| `DOCPREVIEW` | `1` |
| `DOCPREVIEW_BASE_URL` | the resolved `base_url` |
| `DOCUSAURUS_BASE_URL` | the resolved `base_url` |
| `CI` | `true` |

## `detect`

How docpreview decides a change is documentation-related. This is what stops every backend commit from
triggering a two-minute site build.

### `paths`

Globs matched against the pull request's changed files, using `**` semantics. Any match means "build".

Defaults:

```yaml
detect:
  paths:
    - "docs/**"
    - "blog/**"
    - "src/**"
    - "static/**"
    - "**/*.md"
    - "**/*.mdx"
    - "docusaurus.config.*"
    - "sidebars.*"
    - "package.json"
    - "package-lock.json"
    - ".docpreview.yml"
```

An **empty** list is treated as a mistake and builds everything rather than nothing. A misconfigured
repository should be noisy, not silently dead.

### `script`

A path in the repository that overrides the globs entirely. The repository knows its own layout better than
any default.

The script gets the changed paths on stdin, one per line, and runs with the repository root as its working
directory.

| Exit code | Meaning |
|---|---|
| `0` | Build. |
| `78` | Skip. (`EX_CONFIG` from `sysexits.h`.) |
| anything else | Error — reported as a build failure. |

Reserving one specific code for "skip" is what makes a broken script visible. If any nonzero code meant skip,
a script with a typo in it — exiting 127 — would silently disable previews for the repository and nothing
would say so.

Whatever the script writes to stdout becomes the reason line in the pull request comment.

```bash
#!/usr/bin/env bash
# .docpreview/detect — build only when published pages actually change.
set -euo pipefail

changed=$(cat)

# A changelog entry is not a documentation change worth a two-minute build.
filtered=$(grep -v '^CHANGELOG.md$' <<<"$changed" || true)

if grep -qE '^(docs/|sidebars\.|docusaurus\.config\.)' <<<"$filtered"; then
  echo "Published documentation changed."
  exit 0
fi

echo "Only code and changelog entries changed."
exit 78
```

Make it executable, and point at it:

```yaml
detect:
  script: .docpreview/detect
```

The script has a 60-second timeout and a **three-variable** environment — `PATH`, `HOME` pointed at the workspace,
and `DOCPREVIEW=1`. It does not inherit the host's environment, because a build server's environment contains things
a pull request author should not be handed.

## A project row overrides all of this

If the repository has a [project](./projects.md) on this daemon, every build field the project states wins over what
this file says: driver, image, `dir`, `command`, `output`, `base_url` and the detect script. Blank on the project
means "defer to the repository", and a project row that names only a repository is the common case.

That is the point of projects. This file arrives in the pull request, so on any repository where opening one is not
a privilege, its author chooses what it says. A project row is the operator's.

`build.env` here is applied first and cannot set a variable docpreview reserves, one named in the server's
`build.secrets`, or one of the project's own — see [precedence, end to end](./projects.md#precedence-end-to-end).

## Related

- [Server configuration](./configuration.md)
- [Projects](./projects.md)
- [Troubleshooting](../troubleshooting.md)
