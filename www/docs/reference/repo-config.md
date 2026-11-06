---
id: repo-config
title: Repository configuration
sidebar_position: 2
---

# Repository configuration

`.docpreview.yml` goes in the root of the repository being previewed. It's optional. The defaults handle a stock
Docusaurus site.

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

Anyone who can open a pull request controls what this file says. NetFoundry Preview validates every field and limits
what it can do:

- No path may escape the repository root. Absolute paths, `..` traversal and Windows drive-qualified paths are all
  refused.
- `build.command` is honored under **both** drivers. Under `docker` the damage stays in a container. **Under `local`
  it is a shell command on the build host.** On a repository where anyone can open a pull request, use the Docker
  driver, or set the command on a [project](./projects.md).
- `build.env` can't set `DOCUSAURUS_BASE_URL` or any other reserved variable.
- `detect.script` is resolved through symlinks and checked again, because a symlink in an attacker's working tree can
  point anywhere.

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

Default `npm run build`. It runs in `dir` after the dependency install, as one shell command.

:::danger This is arbitrary shell, from the pull request

Under the **local** driver the command runs on the build host as the daemon's user. Under **docker** it runs in the
container. Either way, whoever opens the pull request chooses it.

That's a smaller step than it sounds. `npm run build` already runs `package.json` scripts from the same branch, so
the local driver gives a contributor code execution anyway. That's why forks are refused at the webhook and why the
Docker driver exists. Still, nothing limits this field.

To take the choice away from the branch, set the command on a [project](./projects.md). The project wins, and the
project is yours.

:::

### Dependency install

There's no setting for this. The lockfile decides.

| Committed file | What runs |
|---|---|
| `yarn.lock` | `yarn install --frozen-lockfile --non-interactive --mutex file:${YARN_CACHE_FOLDER:-/tmp}/.yarn-mutex`, then `yarn install --immutable`, then `yarn install` |
| `pnpm-lock.yaml` | `pnpm install --frozen-lockfile`, then `pnpm install` |
| `package-lock.json` | `npm ci --no-audit --no-fund` |
| none of the above | `npm install --no-audit --no-fund` |

The wrong package manager doesn't just run slower. `npm ci` on a tree with only a `yarn.lock` fails outright, and
`npm install` there resolves a different dependency graph than the one the author tested. The lockfile says which
manager owns the tree.

yarn and pnpm fall back to a looser install when the strict one fails. Their flags differ between major versions, and
building from a drifted lockfile beats not building. `npm ci` has no fallback. The package manager has to be on
`PATH` under the local driver, or in the image under the Docker driver.

### `output`

The build output directory, relative to `dir`. Default `build`, which is what Docusaurus writes. Any static site
works: use `out` for a Next.js export, `public` for Hugo, `_site` for Jekyll.

### `base_url`

The path prefix the preview is served under. Default `/`.

It's passed into the build as `DOCUSAURUS_BASE_URL` **and** used as the mount point when serving, so the two can't
drift apart. Any of these work:

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

Missing leading and trailing slashes are added, so `zrok`, `/zrok` and `/zrok/` are the same.

The site is mounted at that prefix, and a request to the bare hostname is redirected there. A reviewer who opens the
hostname alone still lands on the site.

**Which value do you need?** If `docusaurus.config.ts` reads `process.env.DOCUSAURUS_BASE_URL`, any value works and
`/` is the nicest. If it hardcodes a value, `base_url` must match it, or the build is refused. See
[Troubleshooting](../troubleshooting.md).

### `env`

Extra environment for the build. Literal values only. There's no vault lookup, because a pull request author must not
be able to name a secret and have it handed to a script they wrote.

Names NetFoundry Preview reserves are ignored here: every variable in the table below except `CI`, every name in the
server's `build.secrets`, and every one of the project's own variables.

Variables NetFoundry Preview always sets:

| Variable | Value |
|---|---|
| `DOCPREVIEW` | `1` |
| `DOCPREVIEW_BASE_URL` | the resolved `base_url` |
| `DOCUSAURUS_BASE_URL` | the resolved `base_url` |
| `DOCPREVIEW_COMMIT` | the commit being built |
| `DOCPREVIEW_BRANCH` | the branch being built |
| `DOCPREVIEW_REPO` | the platform and repository, such as `github:acme/docs` |
| `DOCPREVIEW_PR` | the pull request number, `0` for the default branch |
| `CI` | `true` |

## `detect`

How NetFoundry Preview decides a pull request changes the site. This is what keeps every backend commit from starting
a two-minute site build. A pull request that matches nothing is not built. Its build shows as **skipped**, with the
reason. A skip never replaces a preview that published, but it does replace a failed build that published nothing.

### `paths`

Globs matched against the pull request's changed files, with `**` semantics. Any match means "build".

Defaults:

```yaml
detect:
  paths:
    - "**/docs/**"
    - "**/blog/**"
    - "**/src/**"
    - "**/static/**"
    - "**/*.md"
    - "**/*.mdx"
    - "**/docusaurus.config.*"
    - "**/sidebars.*"
    - "**/package.json"
    - "**/package-lock.json"
    - ".docpreview.yml"
```

**Every default starts with `**/`, so the site doesn't have to be at the repository root.** A monorepo, or a
repository that assembles several sources into one `unified-doc/`, matches them as written. `**/` matches zero or
more segments, so a root-level `docs/intro.md` still matches `**/docs/**`.

**A build directory other than `.` replaces these defaults.** When `build.dir` (or the project's **Directory**) is
`docusaurus`, the list becomes `docusaurus/**` and `.docpreview.yml`. The site lives in that directory, so a change
outside it can't change the preview. A `paths` list you write here is used as written and never narrowed.

An **empty** list is treated as a mistake and builds everything. A misconfigured repository should be noisy rather
than quietly never building.

### `script`

A path in the repository to a script that replaces the globs. The repository knows its own layout better than any
default.

The script gets the changed paths on stdin, one per line, and runs with the repository root as its working directory.

| Exit code | Meaning |
|---|---|
| `0` | Build. |
| `78` | Skip. (`EX_CONFIG` from `sysexits.h`.) |
| anything else | Error, reported as a build failure. |

Only `78` means skip, so a broken script shows up. If any nonzero code meant skip, a script with a typo that exits
127 would turn off previews for the repository and nothing would say so.

Whatever the script prints, on stdout or stderr, becomes the reason line in the pull request comment.

```bash
#!/usr/bin/env bash
# .docpreview/detect: build only when published pages change.
set -euo pipefail

changed=$(cat)

# A changelog entry isn't worth a two-minute build.
filtered=$(grep -v '^CHANGELOG.md$' <<<"$changed" || true)

if grep -qE '^(docs/|sidebars\.|docusaurus\.config\.)' <<<"$filtered"; then
  echo "Published documentation changed."
  exit 0
fi

echo "Only code and changelog entries changed."
exit 78
```

Make it executable and point at it:

```yaml
detect:
  script: .docpreview/detect
```

The script has a 60-second timeout and a minimal environment. It doesn't get the host's environment or the build's
injected variables, because both hold things a pull request author shouldn't have.

**Under the `docker` driver the script runs in a container** from the project's build image, with only
`DOCPREVIEW=1` and `HOME=/tmp` set. The checkout is mounted read-only at `/workspace`, which is the working directory,
and the container has no network. A script that needs `git` needs an image with git in it.

Under `local` the script runs on the host with `PATH`, `DOCPREVIEW=1` and `HOME` set to the workspace. Only a script
the project names runs there. A script named in this file is ignored.

### The default script: `docpreview-detect`

Under `docker`, when neither the project nor this file names a script, NetFoundry Preview looks for
`docpreview-detect` in the build directory, for example `docusaurus/docpreview-detect`. If it's there, it decides,
with the rules above. If not, the daemon logs that and the globs decide. There's no extension: the shebang picks the
interpreter. The default isn't used under `local`, because any pull request could add the file.

## A project overrides all of this

If the repository has a [project](./projects.md), every build field the project sets wins over this file: driver,
image, `dir`, `command`, `output`, `base_url` and the detect script. A blank field on the project means "use the
repository's value", and a project that names only a repository is the usual case.

That's what projects are for. This file comes in with the pull request, so on a repository where anyone can open one,
the author decides what it says. The project is yours.

`build.env` here is applied first and can't set a reserved variable, one named in the server's `build.secrets`, or
one of the project's own. See [precedence, end to end](./projects.md#precedence-end-to-end).

## Related

- [Server configuration](./configuration.md)
- [Projects](./projects.md)
- [Troubleshooting](../troubleshooting.md)
