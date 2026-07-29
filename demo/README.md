# docpreview demo harness

PowerShell. Every script defaults its root to its own directory, so everything lands here under `demo\`.
`data\`, `repos\`, `work-*\`, `template\` and `docpreview.exe` are all generated and all disposable.

Each of the four demo projects is a **real Docusaurus site** built with `yarn build`, so a build is a genuine
`yarn install` plus a webpack run — a couple of hundred lines of log to watch stream, twenty-odd seconds with
a warm yarn cache and a few minutes the first time.

## First run

```powershell
cd D:\worktrees\tangents\vercel-replacement
go build -o build.claude\ .\cmd\docpreview\   # the harness picks this up automatically
cd demo

.\Build-Template.ps1      # scaffold + install the Docusaurus template all projects are cut from (once)
.\New-Projects.ps1        # recreate the four bare repos and working copies from that template
.\Start-Demo.ps1          # serve on http://127.0.0.1:8493/ and open the dashboard (Ctrl-C to stop)
```

`Start-Demo.ps1 -Background` detaches instead, writing to `daemon.log`.

## Making things happen

```powershell
.\Push-Change.ps1 mydocs new-install-guide "rewrote the install steps"   # one build
.\Send-Burst.ps1                                                         # 12 builds at once
.\Start-KeepBusy.ps1                                                     # a wave every 90s, forever
```

`Start-KeepBusy.ps1` keeps `api-docs/broken-build` permanently failing — an MDX file importing a component
that does not exist. That is the branch to click when you want to see what a failure looks like in the log
viewer.

## Cleaning up

```powershell
.\Stop-Demo.ps1           # daemon plus any node processes a killed build left behind
.\Clear-Branches.ps1      # delete the feature branches so the next wave cuts them from current main
```

Reseeding from scratch is `.\Stop-Demo.ps1`, `Remove-Item -Recurse -Force .\data`, `.\New-Projects.ps1`.
`Build-Template.ps1` does not need repeating — it is the slow part.

## Proving redaction

```powershell
.\Test-Redaction.ps1
```

Stands up a separate daemon on port 8495 with its own vault, runs a build that prints a real secret seven
ways — raw, URL-encoded, JSON, base64, inside a URL, as an `Authorization` header, and split across two writes
with no newline between them — then greps the whole data directory for it. Non-zero exit if it finds anything.

## Files

| Script | What it does |
|---|---|
| `Build-Template.ps1` | Scaffolds `template\` with create-docusaurus and warms the yarn cache. Slow, once. |
| `New-Projects.ps1` | Recreates the four bare repos and working copies. Destructive. |
| `Start-Demo.ps1` / `Stop-Demo.ps1` | The daemon. |
| `Push-Change.ps1` | One edit, one branch, one push. |
| `Send-Burst.ps1` | Every branch on every project, pushed concurrently. |
| `Start-KeepBusy.ps1` | Waves forever, including one branch that always fails. |
| `Clear-Branches.ps1` | Deletes the feature branches both sides. |
| `Test-Redaction.ps1` | End-to-end secret-redaction proof, isolated daemon. |
| `Test-PreviewUrls.ps1` | Fetches every advertised preview URL; non-zero exit if a `ready` one is dead. |
| `Resolve-Docpreview.ps1` | Finds the binary — `..\build.claude\` first, then `.\`. Dot-called by the rest. |

## `ziti-trial\`

The OpenZiti quickstart trial. Its `home\` and `data\` were left behind: a quickstart bakes absolute paths
into its generated PKI and router config, so it cannot be relocated — it has to be re-bootstrapped in place
with `bootstrap.sh`.
