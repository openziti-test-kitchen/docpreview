# docpreview design documents

These describe how docpreview works and **why it works that way**. They are for someone changing the code.

They are not the user documentation. That lives in `www/` and answers "how do I run this"; these answer "what will
break if I change this". Where the two overlap, `www/` is the contract and this is the reasoning behind it.

## What is in here

| Document | Covers |
|---|---|
| [01-architecture.md](01-architecture.md) | The pipeline end to end, the seams, what each package owns |
| [02-exposers.md](02-exposers.md) | The Exposer interface, four implementations, naming, collisions, reaping |
| [03-build-pipeline.md](03-build-pipeline.md) | Clone, detect, build, package managers, the base URL problem |
| [04-concurrency.md](04-concurrency.md) | Supersede, the commit phase, identity comparison, the queue |
| [05-secrets.md](05-secrets.md) | The vault, age, injection, redaction and its limits |
| [06-build-logs.md](06-build-logs.md) | Capture, line buffering, fan-out, server-sent events |
| [07-dashboard.md](07-dashboard.md) | The operator UI: what it shows, and the rules it renders under |
| [08-storage.md](08-storage.md) | The sqlite schema, the job queue, recovery |
| [09-scm.md](09-scm.md) | Webhooks, the single-comment protocol, the local simulator |
| [10-security.md](10-security.md) | The trust boundaries, and what is deliberately not defended |
| [20-container.md](20-container.md) | The image and compose file: the host socket over docker-in-docker, and the path rule that breaks builds |

## Plans, not descriptions

01–10 and 20 describe code that exists. These describe work that does not, and each ends with the order to build
it in.

They go stale in a way the others do not: when the work lands, the plan should be folded into the document above
that now describes it, and deleted.

| Document | Covers |
|---|---|
| [11-github-setup-state.md](11-github-setup-state.md) | The GitHub App exercise. The smoke test has passed; what is left is the App's identity, and this should go |
| [12-github-roadmap.md](12-github-roadmap.md) | What GitHub support is missing: auth and rate-limit failure modes, forks, uninstall, opt-in |
| [13-github-testing.md](13-github-testing.md) | Testing GitHub without GitHub: a fake API, the supersede timing test, the restart gap |
| [14-production-deployment.md](14-production-deployment.md) | Running the daemon as a service, backups, observability, and the five things blocking real HA |
| [15-bitbucket.md](15-bitbucket.md) | Adding Bitbucket Cloud, and what to share out of the github package before copying it |
| [16-exposer-zrok.md](16-exposer-zrok.md) | zrok2 in production: names, namespaces, the reap footgun, access grants |
| [17-exposer-frontdoor.md](17-exposer-frontdoor.md) | Frontdoor in production: the bound port, tenant-wide reaping, authorization |
| [18-exposer-ziti.md](18-exposer-ziti.md) | Plain OpenZiti, and the per-preview authorization question that gates it |
| [19-zrok-namespacing.md](19-zrok-namespacing.md) | Deleting a leaked zrok name, what the account limits count, and why an owned subdomain is admin-only |
| [21-multi-exposer.md](21-multi-exposer.md) | Several exposers at once and one per project: the publications table, and the per-exposer reap that must not delete the others' shares |

## CLAUDE.md files

Each substantial directory carries one: the root, `cmd/docpreview/`, `internal/daemon/`, `internal/vault/`,
`internal/expose/`, `internal/scm/`, `demo/` and `www/`. They are the short version — the rules that must not be
relaxed and the traps that have already caught somebody — and they point here for the reasoning. Keep them
consistent with these documents; a CLAUDE.md that contradicts a design doc is worse than one that is missing.

## How to read a rule in these documents

**Reasoning lives here, not in the code.** Comments in the source describe what the code does; where a rule needs
a paragraph of justification, that paragraph belongs in one of these documents and the comment states the rule.

A claim of the form "X must not Y" is normally guarded by a test. Where it is not, it says so.

## Conventions

- **Invariant** — must hold after any change. Breaking one is a correctness bug, not a regression in polish.
- **Deliberate limit** — a known gap that is not worth closing, with the reason.
- **Open** — undecided, and the decision is recorded in `TODO.md` rather than here.
