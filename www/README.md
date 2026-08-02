# docpreview documentation

The docpreview documentation site, built with [Docusaurus](https://docusaurus.io/). It is also docpreview's
integration test: the tool builds and previews this directory, so if the docs render, the tool works.

## Read it locally

```bash
cd www
yarn install     # or: npm install
yarn start       # or: npm start
```

Opens `http://localhost:3000/` with hot reload. Edit any `.md` under `docs/` and the browser updates.

Both package managers work. The committed lockfile is `package-lock.json`, which is what docpreview itself
uses (`npm ci`). `yarn install` generates its own `yarn.lock`; do not commit it.

## Where to start reading

Once the dev server is up:

| Page | URL |
|---|---|
| **What this is** | http://localhost:3000/docs/intro |
| **Quickstart** | http://localhost:3000/docs/quickstart |
| How Vercel previews work | http://localhost:3000/docs/background/vercel |
| Jamstack | http://localhost:3000/docs/background/jamstack |
| Docusaurus | http://localhost:3000/docs/background/docusaurus |
| age — the credential vault | http://localhost:3000/docs/background/age |
| **Architecture** | http://localhost:3000/docs/architecture |
| **Exposers** — zrok2 / Frontdoor | http://localhost:3000/docs/exposers |
| **GitHub App guide** | http://localhost:3000/docs/guides/github-app |
| zrok v2 guide | http://localhost:3000/docs/guides/zrok2 |
| Frontdoor guide | http://localhost:3000/docs/guides/frontdoor |
| Server configuration | http://localhost:3000/docs/reference/configuration |
| Repository configuration | http://localhost:3000/docs/reference/repo-config |
| CLI reference | http://localhost:3000/docs/reference/cli |
| Security model | http://localhost:3000/docs/reference/security |
| Troubleshooting | http://localhost:3000/docs/troubleshooting |

## Build it

```bash
yarn build       # -> build/
yarn serve       # serve build/ on :3000
```

## Testing the baseUrl override

`docusaurus.config.ts` reads `baseUrl` from the environment, which is what lets docpreview serve the same
source tree at any mount point. Prove it:

```bash
DOCUSAURUS_BASE_URL=/zrok/ yarn build
grep -o 'href=/zrok/assets[^ ]*' build/index.html
```

Every emitted asset URL now carries the `/zrok/` prefix. That value is baked in at build time and cannot be
changed afterwards, which is why docpreview passes it to the build and uses it as the serve mount from the
same stored value.

> **Git Bash on Windows.** MSYS rewrites a leading-slash argument into a Windows path, so
> `DOCUSAURUS_BASE_URL=/zrok/` becomes `C:/Program Files/Git/zrok/` and the build fails on broken links.
> Prefix with `MSYS_NO_PATHCONV=1`, or use PowerShell. docpreview is unaffected — Go's `exec` sets the
> environment directly and does no path conversion.

The Go tests exercise the same behaviour:

```bash
cd ..
go test ./internal/preview/ -run Real -v      # serves this build, asserts every asset returns 200
go test ./internal/pipeline/ -run BaseURL -v  # the mismatch detector
```

## Layout

```
docs/
  intro.md                    what this is
  quickstart.md               ten minutes to a preview URL
  architecture.md             how the pieces fit
  exposers.md                 zrok2, Frontdoor, local
  troubleshooting.md          the baseUrl trap, and everything else
  background/                 Vercel, Jamstack, Docusaurus, age
  guides/                   GitHub App, zrok2, Frontdoor
  reference/                  configuration, repo-config, CLI, security
sidebars.ts                   hand-written; reading order is deliberate
docusaurus.config.ts          note the baseUrl line
```

## Conventions

- `onBrokenLinks: 'throw'`. A broken cross-reference fails the build, which is the highest-value thing a
  documentation preview does.
- Internal links are relative `.md` paths (`./quickstart.md`, `../exposers.md`) so they resolve at any
  `baseUrl` and stay clickable when browsing the repository on GitHub.
- Every claim about an external product cites a source at the bottom of the page.
