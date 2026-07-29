# Build pipeline

`clone → detect → build → verify`, in `internal/pipeline`. Everything here is reversible: a clone in a scratch
directory and a build tree nobody can see. The irreversible half is in [04-concurrency.md](04-concurrency.md).

## Clone

```go
func (c *Cloner) Clone(ctx, pr, cloneURL) (*Workspace, error)
```

Shallow, single-branch, into a directory named by preview ID under `data/workspaces/`.

**`cloneURL` carries an access token.** It is never logged, never placed in a git remote that persists, and
never appears in an error. `internal/scm/local` has `scrubGitOutput` for the same reason: git puts the userinfo
component of a URL into its own error messages.

`Workspace` carries the `PullRequest` as well as the directory. The builder needs it to tell the build what it
is building — a generated site has no other way to know its own commit.

The workspace is deleted as soon as the output is copied out. It is a scratch directory, not an artifact.

## Detect

Not every push to a documentation repository changes documentation. Building on a CI config change wastes a
build and republishes a preview nobody asked for.

```yaml
detect:
  paths: ["docs/**", "**/*.md", "docusaurus.config.*"]
  script: ""          # optional, overrides paths
```

`Decision{Build bool, Reason string}`. The reason goes into the comment, so a skipped build says why.

`detect.script` is attacker-influenced — it comes from the pull request. It runs with a 60-second cap, five
environment variables, and its path is resolved through symlinks and re-checked, because a symlink in a working
tree the attacker controls can point anywhere.

## Build

```go
func (b *Builder) Build(ctx, ws *Workspace, cfg config.RepoConfig, sink io.Writer) (*Result, error)
```

`sink` receives output as it is produced — that is the live tail. It is separate from `Result.Log`, which is
what goes into the comment: the return value has to be complete, the sink only has to be timely.

Every value this returns is scrubbed of injected secrets — the log on the success path and the error text on
all five failure paths. Done once in a deferred wrapper rather than at each `return`, because a redaction that
must be remembered at every return is one that will be missed the next time somebody adds one.

### Drivers

`local` runs the build on the host with this process's privileges. Fine for a repository whose contributors you
already trust; unacceptable for a public one. `docker` runs it in a throwaway container with the workspace
bind-mounted, 4 GB, 2 CPUs, and nothing else.

`build.command` from `.docpreview.yml` is honored **only under the docker driver**. Under `local` it is ignored,
because it would be arbitrary shell on the host chosen by whoever opened the pull request. To change the command
under `local`, put it behind `npm run build` in `package.json` — which puts the decision in the hands of
whoever can merge rather than whoever can open a pull request.

### The package manager comes from the lockfile

| Committed | Runs |
|---|---|
| `yarn.lock` | `yarn install --frozen-lockfile --non-interactive` |
| `pnpm-lock.yaml` | `pnpm install --frozen-lockfile` |
| `package-lock.json` | `npm ci --no-audit --no-fund` |
| none | `npm install --no-audit --no-fund` |

Not merely a speed choice. `npm ci` against a tree with only a `yarn.lock` fails outright, and `npm install`
there resolves a different dependency graph than the one the author tested. The lockfile is the author's
statement of which manager owns the tree.

Each branch falls back to a looser install if the strict flag is rejected — the flag differs by major version
(`--frozen-lockfile` is yarn 1, `--immutable` is yarn 2+) and refusing to build at all is worse than building
from a drifted lockfile.

### Environment

```
DOCPREVIEW=1
DOCPREVIEW_BASE_URL     the resolved base URL
DOCUSAURUS_BASE_URL     the same, under the name a Docusaurus config expects
DOCPREVIEW_COMMIT       head SHA
DOCPREVIEW_BRANCH
DOCPREVIEW_REPO         platform:owner/name
DOCPREVIEW_PR           number
CI=true
<operator secrets>      from build.secrets, via the vault
<repo env>              from .docpreview.yml build.env
```

Order matters. Repository-supplied entries are applied before operator secrets, and **every name above is
reserved** — a pull request cannot shadow `DOCUSAURUS_BASE_URL` (which would break its own preview in a way
that looks like our bug), cannot shadow `ALGOLIA_WRITE_KEY` (which would let it watch what the build does with a
value the redactor does not know about), and cannot shadow `DOCPREVIEW_COMMIT` (which would let a stale preview
claim to be the current one).

The commit variables exist because a site otherwise cannot say which push produced it, and "am I looking at the
old one or the new one?" is the question a reviewer has while a rebuild runs. Vercel exposes the same three
under `VERCEL_GIT_*`.

## The base URL problem

This is the single most confusing failure mode in static-site previews, and it has caused two separate bugs
here.

**Docusaurus bakes `baseUrl` into the output at build time.** A site built for `/` and served under
`/preview/docs-main/` returns its `index.html` and 404s every stylesheet and script in it. The page loads. It
looks like a broken preview rather than a configuration mismatch.

Two consequences:

### 1. The mount path must be known before the build

Under a `PathExposer` the preview lives beneath a prefix. `Daemon.previewBaseURL(name, repoBase)` folds the
mount path into the repository's own base URL, and `runPipeline` resolves the **name before the build** in order
to call it. That is why `previewName` moved above `builder.Build`.

### 2. `verifyBaseURL` has to ask two different questions

```go
if baseURL != "" && baseURL != "/" {
    // Do a majority of absolute references start with it?
} else {
    // Infer what prefix the site actually wanted, and complain if it wanted one.
}
```

Neither test works for both cases:

- **Prefix matching is vacuous for `/`.** Every absolute path starts with `/`, so the check passes on exactly
  the sites it exists to catch. That was the original bug.
- **Inference cannot handle a multi-segment base.** `inferBaseURL` reports the *first* path segment, so a site
  built correctly for `/preview/handbook-new-install-guide/` infers as `/preview/` and is rejected for a
  mismatch that does not exist. That failed **every build** the moment previews moved onto a path, with an error
  confidently naming two base URLs that were the same one.

A **majority**, not all. A hand-written `href="/"` in a footer is legal and common; failing a correct build over
one would make the check the problem. `dominantShare` is 0.6, and the exact number barely matters — a
root-mounted site scatters references across `/assets`, `/img`, `/docs`, `/blog`, while a prefixed site puts
every one under a single segment. The two distributions are nowhere near each other.

### Parsing the references

```go
var absoluteRef = regexp.MustCompile(`(?:href|src)=(?:"(/[^"]*)"|'(/[^']*)'|(/[^\s>]*))`)
```

The unquoted form is not a curiosity — it is what Docusaurus emits. Its production build minifies the HTML and
drops quotes wherever the value has no spaces:

```html
<link href=/zrok/assets/css/styles.160a45a5.css rel=stylesheet>
```

A pattern matching only `href="..."` found **zero** references in every real build, which meant the check
silently passed on exactly the sites it exists to protect.

Below `minRefsToInfer` (3) references, the check is skipped: a site built with relative URLs works at any
prefix, and too small a sample cannot tell a base path from an ordinary directory.

## Serving the result

`preview.New(dir, baseURL)` returns a `*Site`, an `http.Handler` mounted at `baseURL` that strips its own prefix
and redirects `/` to it. It adds no-store caching headers and serves `404.html` when present.

**Invariant: the `baseURL` passed to `preview.New` is the same one the site was built with.** They are computed
once in `runPipeline` and passed to both, so they cannot drift.

## Invariants

1. `cloneURL` never reaches a log, an error, or a persisted remote.
2. The workspace is removed once output is copied out.
3. Repository config cannot set any reserved environment variable.
4. `build.command` is ignored under the local driver.
5. The install command is chosen by the committed lockfile.
6. The base URL used to build, to verify, and to serve is one value.
7. Every returned error and log is scrubbed.
