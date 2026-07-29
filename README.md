# docpreview

Documentation previews for pull requests. One Go binary, runs anywhere.

Push a commit → a bot comment appears on the pull request with a link → click it and there is your
documentation site, built from that branch. Push again and the same comment updates in place. Close the pull
request and the preview disappears.

That is what Vercel does for documentation repositories. The difference is where it runs: this is a single
binary you can start on a laptop, and the public URL comes from [zrok](https://docs.zrok.io/) or
[NetFoundry Frontdoor](https://netfoundry.io/docs/frontdoor/intro/) rather than somebody else's cloud.

## Why

- **Docs you cannot send to a SaaS.** Unreleased, internal, embargoed. The build runs on your hardware.
- **Bitbucket, or Bitbucket and GitHub.** Preview tooling clusters around GitHub.
- **It should not be complicated.** A documentation site is a directory of static files.

## Quick look

Try it with no accounts and no GitHub App:

```powershell
docpreview init                    # one question: which exposer
docpreview preview -build ./www    # publishes a real URL, holds it until Ctrl-C
```

Then, when you want it on pull requests:

```powershell
docpreview vault keygen -out C:\ProgramData\docpreview\master.key   # then: vault.key_source in the config
docpreview vault set github.private_key -file .\app-key.pem        # from the GitHub App
"webhook-secret" | docpreview vault set github.webhook_secret

docpreview doctor                                                  # real calls, not a config lint
docpreview serve
```

The "vault" is one file — `~/.docpreview/vault.age` — holding every credential, encrypted with
[age](https://age-encryption.org). Not a service, nothing to install; the age library is linked into the binary.

Where its master key comes from is a choice, not a default. `vault.key_source` reads it from a file or from a
command like `op read`, which is what lets the daemon come back from a reboot on its own. Configure nothing and it
boots with a locked vault and waits for a person — the only arrangement with no key at rest anywhere. Background:
[`www/docs/background/age.md`](www/docs/background/age.md).

Then expose the webhook — [`www/docs/runbooks/webhook-tunnel.md`](www/docs/runbooks/webhook-tunnel.md) — and open
a pull request touching `docs/`.

> **Documentation preview**
>
> | | |
> |---|---|
> | **Status** | ✅ Ready |
> | **Preview** | https://feature-new-guide.share.zrok.io/ |
> | **Name** | `feature-new-guide` |
> | **Commit** | `a1b2c3d` |
> | **Built in** | 41s |

## How it fits together

```
  GitHub App ─┐                     ┌─ zrok2 ─────────┐
              ├─ webhook ─ ingress ─┤  frontdoor      ├─ public URL ─┐
  Bitbucket ──┘      │              └─ local ─────────┘              │
                     v                                               v
              sqlite queue ─ worker ─ clone ─ detect ─ build ─ serve │
                                                                     │
                       PR comment, edited in place ◄─────────────────┘
```

Two interfaces carry the design:

**`Exposer`** takes an `http.Handler`, not a port. zrok's Go SDK hands back a listener on the OpenZiti
overlay, so a zrok-published preview binds **no local TCP port at all**. Frontdoor binds one internally
because its agent connects back over the network. Switching between them is one line of config.

**`scm.Client`** normalizes GitHub and Bitbucket down to: verify a webhook, get a clone URL, list changed
files, publish a report, retract it.

## The baseUrl thing

Docusaurus bakes `baseUrl` into every emitted `href` and `src` at build time. Build for `/my-project/`, serve
at `/`, and `index.html` loads while every stylesheet 404s — an unstyled wall of text with nothing in the
build log to explain it.

docpreview passes the configured base URL into the build **and** uses it as the serve mount point, then
inspects the built `index.html` and refuses to publish a preview whose asset URLs disagree with where it is
about to be mounted. `/`, `/docs/`, `/zrok/`, anything — configurable per repository, verified before it
reaches a reviewer.

## Repository layout

```
cmd/docpreview/         CLI: serve, doctor, vault, sim, configure
internal/
  buildlog/             build output: redacted, stored, fanned out live
  config/               server config + attacker-influenced repo config
  daemon/               orchestration, worker pool, webhook ingress, dashboard
  expose/               Exposer interface + local, zrok2, frontdoor, ziti
  model/                pull requests, preview IDs, name sanitization
  pipeline/             clone, detect, build, baseUrl verification
  preview/              static file server
  redact/               scrubbing known secrets in six encodings
  scm/                  Client interface + GitHub App + local simulator
  store/                sqlite queue and preview table
  vault/                age-encrypted credentials
  zitiadmin/            provisioning an OpenZiti network
demo/                   PowerShell harness: four Docusaurus projects, real builds
docs/design/            how it works and why — read before changing it
www/                    the documentation site — and the test subject
```

## The docs

Everything is in `www/`, which docpreview builds and previews itself. If the documentation renders, the tool
works.

```bash
cd www
yarn install     # or: npm install
yarn start       # or: npm start
```

Then, at `http://localhost:3000`:

| Page | Path | Source |
|---|---|---|
| **What this is** | `/docs/intro` | `www/docs/intro.md` |
| **Quickstart** | `/docs/quickstart` | `www/docs/quickstart.md` |
| How Vercel previews work | `/docs/background/vercel` | `www/docs/background/vercel.md` |
| Jamstack | `/docs/background/jamstack` | `www/docs/background/jamstack.md` |
| Docusaurus | `/docs/background/docusaurus` | `www/docs/background/docusaurus.md` |
| age — the credential vault | `/docs/background/age` | `www/docs/background/age.md` |
| **Architecture** | `/docs/architecture` | `www/docs/architecture.md` |
| **Exposers** — zrok2 / Frontdoor | `/docs/exposers` | `www/docs/exposers.md` |
| **GitHub App runbook** | `/docs/runbooks/github-app` | `www/docs/runbooks/github-app.md` |
| **Expose the webhook** | `/docs/runbooks/webhook-tunnel` | `www/docs/runbooks/webhook-tunnel.md` |
| zrok v2 runbook | `/docs/runbooks/zrok2` | `www/docs/runbooks/zrok2.md` |
| Frontdoor runbook | `/docs/runbooks/frontdoor` | `www/docs/runbooks/frontdoor.md` |
| Server configuration | `/docs/reference/configuration` | `www/docs/reference/configuration.md` |
| Repository configuration | `/docs/reference/repo-config` | `www/docs/reference/repo-config.md` |
| CLI reference | `/docs/reference/cli` | `www/docs/reference/cli.md` |
| Security model | `/docs/reference/security` | `www/docs/reference/security.md` |
| Troubleshooting | `/docs/troubleshooting` | `www/docs/troubleshooting.md` |

See [`www/README.md`](www/README.md) for the documentation workflow, including how to prove the `baseUrl`
override works.

### Design documents

`www/docs` answers "how do I run this". [`docs/design`](docs/design/README.md) answers "what breaks if I change
this" — the invariants, the reasoning, and the failures that produced each rule. Read it before changing the
commit phase, an exposer, or redaction.

| | |
|---|---|
| [Architecture](docs/design/01-architecture.md) | The pipeline, the two seams, package ownership |
| [Exposers](docs/design/02-exposers.md) | The interface, four implementations, naming, reaping |
| [Build pipeline](docs/design/03-build-pipeline.md) | Clone, detect, build, the base URL problem |
| [Concurrency](docs/design/04-concurrency.md) | Supersede, the commit phase, composed status |
| [Secrets](docs/design/05-secrets.md) | The vault, injection, redaction and its limits |
| [Build logs](docs/design/06-build-logs.md) | Line buffering, fan-out, server-sent events |
| [Dashboard](docs/design/07-dashboard.md) | The operator UI and the rules it renders under |
| [Storage](docs/design/08-storage.md) | Schema, the job queue, recovery |
| [Source control](docs/design/09-scm.md) | Webhooks, the single-comment protocol, the simulator |
| [Trust boundaries](docs/design/10-security.md) | What is defended, and what deliberately is not |

01–10 describe code that exists. 11–18 are plans for code that does not, each ending with the order to build it
in — the GitHub roadmap and its test strategy, production deployment and what blocks real HA, Bitbucket, and one
per exposer. See [`docs/design/README.md`](docs/design/README.md).

## Building

```powershell
go build -o build.claude/ ./cmd/... ./internal/...
go test ./cmd/... ./internal/...
```

The explicit package list rather than `./...`: the demo's `node_modules` contains Go files that are not part of
this module. A running daemon holds the built binary open on Windows, so stop it before rebuilding.

Requires Go 1.26 (the zrok v2 SDK does). Node 20+ to run documentation builds, unless you use the Docker
driver.

## Status

| | |
|---|---|
| GitHub App | Authenticates against real GitHub. Has not yet processed a real pull request |
| Webhook over zrok | Working end to end — a signed delivery is accepted through a public URL |
| zrok v2 exposer | Working, via the Go SDK. Preview URLs not yet exercised by a reviewer |
| local exposer | Working |
| Docusaurus builds | Working, local and Docker drivers |
| Frontdoor exposer | Written; wire format unverified against a live tenant |
| ziti exposer | Works, but separates previews by a client-supplied `Host` header — so any reader-attributed identity can reach any preview. Not suitable for reviewers you do not trust equally. [18-exposer-ziti.md](docs/design/18-exposer-ziti.md) |
| Bitbucket | Interface in place, client not written |
| Real HA | Not supported. Five specific blockers, see [14-production-deployment.md](docs/design/14-production-deployment.md) |
