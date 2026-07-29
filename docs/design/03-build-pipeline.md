# Build pipeline

`clone → detect → build → verify`, in `internal/pipeline`. Everything here is reversible: a clone in a scratch
directory and a build tree nobody can see. The irreversible half is in [04-concurrency.md](04-concurrency.md).

## Clone

```go
func (c *Cloner) Clone(ctx, pr, cloneURL) (*Workspace, error)
```

Shallow, fetched by SHA rather than by branch name, into `data/workspaces/<preview>/<sha12>`.

### One workspace directory per build, not per preview

Two builds of one pull request overlap **by design**: a newer push cancels the older build but does not wait for
it, so the loser is still unwinding while the winner clones. Sharing `workspaces/<preview>` let them corrupt each
other two ways (`internal/pipeline/clone.go:69-98`):

- The loser's cleanup deleted files under the winner mid-build.
- On Windows a locked file made that cleanup *fail*, so a `.git` survived — and the winner's `git remote add
  origin` then failed with "remote origin already exists", killing a build that had nothing wrong with it. This is
  the failure that was never reachable from the local git simulator, where nothing holds a file open.

Now `workspaces/<preview>/<sha12>` (`buildDirName`, `clone.go:139`). Nested under the preview ID so teardown's
`RemoveAll` of the preview directory keeps working unchanged. Twelve characters of SHA: enough that a collision
needs deliberate effort, short enough to keep paths clear of Windows' limit — the workspace holds `node_modules`,
whose paths are the deepest thing this program creates.

Siblings from earlier commits are pruned **best-effort**, and the failure is the point rather than a nuisance
(`pruneSiblings`, `clone.go:155`). A sibling that will not delete is a superseded build still shutting down with a
file open, and that directory must survive until it does. Deleting it is the original bug.

### No named remote

`git init`, `git fetch <url> <sha>`, `git checkout FETCH_HEAD` (`clone.go:116-120`). The remote is gone entirely.

`git remote add origin` fails outright on a repository that already has an origin, so any `.git` surviving an
interrupted build turned the next attempt into a failed build — which is exactly what a supersede produced.
Fetching the URL directly is idempotent, and nothing here needs a remote to persist: the URL carries a short-lived
token that would be stale by the next build anyway.

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

#### Spelling the host path (Windows)

The mount source has to be a path the *daemon* can resolve, and on Windows that is never the path the host uses.
The daemon is Linux and has no drive letters: `--volume` splits its fields on a colon and reports
`invalid mode: /workspace`, `--mount` reports the path is not absolute. `hostMountPath`
(`internal/pipeline/dockermount.go`) rewrites `D:\ws` as `/mnt/d/ws`, which is where a dockerd running inside
WSL2 sees the Windows disks, and the driver passes `--mount` rather than `--volume` so the remaining colon in
`type=bind,source=…` is the only one.

Two daemons this does not handle, both deliberately an error rather than a guess: Docker Desktop's own engine
exposes the host at `/run/desktop/mnt/host/<letter>`, and a daemon on another machine cannot see the host's disk
at all. A wrong path that the daemon *accepts* mounts an empty directory, and the build then fails on a missing
`package.json` — which sends whoever is debugging it into the repository instead of at the mount. Refusing up
front costs an hour less.

#### Symlinks in the output are refused

A mount is the one place the docker driver's containment leaks in the wrong direction. The preview server hands
the output directory to `http.Dir`, which refuses a URL that climbs out of the root but follows a symlink that
leaves it — so a build could publish a host file by writing `build/leak -> /etc/passwd`. The container cannot
read the host, but the server serving its output can. `rejectSymlinks` walks the output directory and fails the
build, naming the file. A static site has no use for a symlink, so one appearing is either an attempt or a bug.

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

### 3. The check runs again at recovery, not only after a build

`verifyBaseURL` is exported as `pipeline.VerifyBaseURL` (`internal/pipeline/build.go:454`) because it is needed
twice. A build knows the base URL it just used and can check its own output. A **republish** takes the base URL out
of a stored row (`Daemon.republish`, `internal/daemon/daemon.go:542`), and that row disagrees with the artifacts
sitting beside it whenever `exposer.kind` has changed since the build: a path-mounting exposer folds its mount
prefix into the base URL and a host-per-preview one does not.

Serving anyway produces the failure at the top of this section, on startup, for every preview at once — index.html
loads, every asset 404s, and nothing in any log explains it. So the artifacts are checked against the stored value
before they are published, and a mismatch is `errArtifactsUnusable` (`daemon.go:466`), which drops the row exactly
as a missing artifact directory does. The next push rebuilds against the current exposer.

Dropping rather than keeping: leaving the row means this error on every startup forever, and a comment advertising
a URL that serves a broken page. See [08-storage.md](08-storage.md).

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
6. The base URL used to build, to verify, and to serve is one value — including on a republish, where the value
   comes from the database and is verified against the artifacts before anything is served.
7. Every returned error and log is scrubbed.
8. Two builds of one pull request never share a workspace directory.
9. Nothing the clone does requires a clean `.git`, so an interrupted build cannot poison the next one.
