# Running this in production

Everything in this project so far has assumed a developer laptop: a terminal open, a person present, `Ctrl-C`
when they are done. The requirement now is *"a long running, restart surviving, ha-type install. eventually
we'll have to plan for when this is actually ha/redundant"*.

That is two tiers, and they are very far apart.

**Tier 1 — one instance, supervised, survives a reboot.** Close. The code already does the hard part: `recover`
republishes every ready preview from artifacts on disk without re-cloning
(`internal/daemon/daemon.go:367`), the job queue is in sqlite so a push landing during a restart is not lost
(`internal/store/store.go:119`), and `vault.key_source` means the vault can open without a person
(`internal/vault/keysource.go:154`). What is missing is a service definition, a backup, and enough of a
readiness signal that somebody on call can tell "up" from "up and inert".

**Tier 2 — two instances serving at once.** Not close, and not one change. Five separate assumptions in the
code say "there is exactly one docpreview", and two of them are in interface contracts rather than in
implementations, which is what makes the ordering below matter: they are cheap to change while there are four
exposers and expensive once there are eight.

The host is not chosen. This document covers the realistic options and recommends one, which is: **a small
Linux VM, systemd, the `local` build driver, the master key delivered as a systemd credential, and the webhook
endpoint the only thing reachable from outside.** The reasoning is in each section.

---

## Tier 1: one instance that survives a restart

### What already works

A restart is deliberately cheap, and that shapes everything below it.

`recover` reaps every remote object the exposer owns, then republishes each `ready` row from
`data_dir/artifacts/<preview id>` (`internal/daemon/daemon.go:375`, `:391`). No clone, no `npm install`, no
rebuild — a box with forty open documentation pull requests is serving all forty again a second or two after
the process comes up. `base_url` is stored rather than recomputed (`internal/store/store.go:53`) precisely so
this works after a config change, because a site built for `/docs/` and served at `/` returns its HTML and
404s every asset in it.

Queued work survives too. `Enqueue` is an upsert keyed by preview ID, and the one-second worker poll exists
specifically to pick up jobs left behind by a previous process (`internal/daemon/daemon.go:41`).

**An in-flight build does not survive.** `Claim` is `DELETE ... RETURNING`, so claiming a job removes it, and
a process that dies mid-build loses that build with nothing marked "running" to clean up
(`internal/store/store.go:140`–`:147`). This is deliberate and documented, and the operational consequence is
worth stating plainly: **a restart is cheap, not invisible.** One reviewer sees a comment stuck on "building"
until they push again. Restarting during a quiet hour costs nothing; restarting under load costs one comment
per build in flight, times `workers`.

Shutdown is already graceful on the signal path: `signal.NotifyContext` on `os.Interrupt` and `SIGTERM`, then
`srv.Shutdown` with a 30-second budget, with `BaseContext` wired so the dashboard's server-sent-event handlers
unblock instead of holding shutdown open for the full timeout (`cmd/docpreview/main.go`, `cmdServe`).

### What is missing

Supervision, a key that survives a reboot, a backup of the two precious files, and a readiness signal. In that
order of importance.

### The master key is the only genuinely new decision

Everything else in tier 1 is packaging. This one is a security trade with no default answer, which is why
`vault.key_source` is a setting and not a policy (`docs/design/05-secrets.md`, "The master key").

Unset is the default and it is a *state*, not a failure: the daemon boots with a locked vault, serves the
setup page, and waits. On a laptop that is right. On a production box it means **every reboot leaves the
daemon inert until a human arrives** — and worse, inert in a way that looks healthy: `/healthz` answers `ok`,
the dashboard renders, and `/webhook/github` answers 501 because no GitHub client could be built without the
App private key (`internal/daemon/ingress.go:172`). GitHub records the failed deliveries and nothing pages
anybody.

So a production box configures one of three sources.

| Source | What it costs | When it is right |
|---|---|---|
| `exec:` a credential helper | the key exists in this process and nowhere else on the machine | there is already a secret manager on the box |
| `file:` a key file | anyone who can read that path can read every secret | there is not |
| `$DOCPREVIEW_MASTER_KEY` | visible in the unit file, in `ps`, and in crash dumps | never, on a box you did not build by hand |

Two constraints bind the choice. A key file may not live inside `data_dir` — config load refuses it
(`internal/config/config.go:568`), because one directory read would otherwise yield both `vault.age` and the
key that opens it. And on Unix a key file readable by group or other is refused at read time
(`internal/vault/keysource.go:184`); on Windows it is not, because `os.Stat` reports 0666 for every ordinary
file regardless of its ACL, so the check would reject every key file for a reason that is not true.

**systemd credentials are the recommended delivery mechanism on Linux**, and they land in the `file:` form
rather than needing a new one. `LoadCredential=` copies the key into a tmpfs directory that only the unit's
user can read, mode 0400, unlinked when the service stops — which satisfies the permission check above and
keeps the material off the service's own filesystem. The one sharp edge: **the config file has no environment
expansion**, so `$CREDENTIALS_DIRECTORY` cannot appear in `key_source`. The literal path has to be written
out. It is stable, being derived from the unit name.

```ini
# /etc/systemd/system/docpreview.service
[Unit]
Description=docpreview — documentation previews for pull requests
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=docpreview
Group=docpreview
ExecStart=/usr/local/bin/docpreview serve -config /etc/docpreview/config.yml

Restart=always
RestartSec=5s
# Longer than the 30s srv.Shutdown budget in cmdServe, or systemd SIGKILLs a
# shutdown that was going to finish, and every in-flight build dies with it.
TimeoutStopSec=45s

# The key, in tmpfs, mode 0400, owned by this unit's user, gone when it stops.
LoadCredential=master.key:/etc/docpreview/master.key

StateDirectory=docpreview
WorkingDirectory=/var/lib/docpreview
# npm caches under $HOME. Without this it lands wherever /etc/passwd says, which
# under ProtectHome=yes is a directory the build cannot write.
Environment=HOME=/var/lib/docpreview

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/docpreview

[Install]
WantedBy=multi-user.target
```

```yaml
# /etc/docpreview/config.yml
data_dir: /var/lib/docpreview
workers: 2
listeners:
  - tcp: "127.0.0.1:8471"
vault:
  key_source: "file:/run/credentials/docpreview.service/master.key"
```

The hardening directives are not decoration. Under the `local` driver a build runs `package.json` scripts as
the daemon's user (`docs/design/10-security.md`), so `ProtectSystem=strict` plus a single `ReadWritePaths` is
the difference between "a contributor's postinstall script can write to `/var/lib/docpreview`" and "…can write
anywhere that user can". It does not make an untrusted repository safe — fork pull requests are refused at the
webhook layer for that, and the `docker` driver exists for repositories whose contributors are not fully
trusted — but it bounds the trusted-contributor accident.

`Restart=always` is correct here rather than `on-failure`, because the failure this guards against is the
process exiting for a reason nobody predicted. A restart loop is visible in `systemctl status` and costs at
most the in-flight builds, which the design already treats as expendable.

### Windows

**docpreview is not a Windows service.** There is no `golang.org/x/sys/windows/svc` handler anywhere in the
tree, so `sc.exe create` pointed straight at the binary produces a service that the SCM kills for not
responding to its start request. It needs a wrapper — WinSW, NSSM, or `shawl` — or Task Scheduler with an
at-startup trigger running as a service account.

Two things are worse on Windows and both are worth knowing before choosing it.

Graceful stop is unproven. `TODO.md` lists it explicitly: `docpreview serve` graceful SIGINT has never been
exercised interactively on Windows, and a background process there is killed hard. A wrapper's "stop" is
therefore likely to be a hard kill, which loses in-flight builds — cheap, per the `Claim` design — and leaves
an orphaned workspace directory behind, which the next `recover` does not sweep. See the disk section.

The key file is protected by ACLs the daemon cannot verify (`internal/vault/keysource.go:184`). On Windows the
guarantee is whatever the operator set with `icacls`, and nothing at startup will tell them they got it wrong.
An `exec:` source pointing at a credential helper is a better answer there than on Linux, precisely because
the file check is absent.

### The container option

A container changes four things, and none of them are hard, but all four have to be answered before the first
build runs.

**The vault.** `data_dir` must be a named volume or the vault and the database are destroyed on the next image
pull. The master key must *not* be baked into the image and must not be an environment variable in the compose
file — that is the demoted path for exactly this reason. A secret mount (`/run/secrets/...`) referenced as
`file:` is the equivalent of the systemd credential, and it satisfies the not-inside-`data_dir` rule as long
as the mount is not under `data_dir`.

**Node.** The `local` driver shells out to `npm` through `sh -c` (`internal/pipeline/build.go:382`), so the
image needs Node in it. The published `docpreview` binary is static — pure-Go sqlite, no cgo
(`internal/store/store.go:11`) — which makes `FROM node:24-bookworm-slim` plus one `COPY` the whole
Dockerfile. That is a real advantage of the container option: the build toolchain becomes an image tag rather
than something you `apt install` on a host and then forget you did.

**The npm cache.** Nothing in the pipeline configures a cache location, so it is `$HOME/.npm` for the daemon's
user. In a container that is inside the writable layer and is discarded on every recreate, which turns every
first build after a deploy into a cold `npm ci`. Mount a volume for it.

**The docker driver, inside a container, needs the host's docker socket.** `buildDocker` shells out to the
`docker` CLI (`internal/pipeline/build.go:349`), so the isolation the driver exists to provide requires
mounting `/var/run/docker.sock` into the container — which hands the container root on the host. If the reason
for the container was isolation, this undoes it. Use the `local` driver inside a container, or the `docker`
driver on a host, not the cross.

### The recommendation

**A small Linux VM with the systemd unit above, the `local` driver, and the master key as a systemd
credential.** It is the shape with the fewest moving parts that satisfies "restart surviving": one static
binary, one state directory, one unit file, `journalctl -u docpreview` for logs. The container option is
better only if the deployment target is already a container platform — in which case the argument for it is
not isolation but that Node's version becomes part of the artifact.

---

## The filesystem

```
<data_dir>/
  vault.age              PRECIOUS   every credential, age-encrypted
  docpreview.db          PRECIOUS   previews, jobs, local-platform comments
  artifacts/<id>/        reproducible, but only by rebuilding
  workspaces/<id>/       scratch, deleted when the build output is copied out
  logs/<id>/<build>.log  reproducible not at all, and worth nothing durable
```

Two files matter. Everything else is derived.

**`vault.age` is irreplaceable.** Lose it and you re-mint a GitHub App private key, regenerate the webhook
secret in two places, and re-enter every build secret. **And it is worthless without the master key, which is
deliberately not in `data_dir`** — so a backup of `data_dir` alone is not a restorable backup. The key belongs
in whatever holds the rest of the organisation's secrets; if the answer is "the key file on that box", then
the backup of that box's `/etc/docpreview` is part of the backup of docpreview.

**`docpreview.db` is what makes a restart cheap rather than a rebuild.** It holds the mapping from preview ID
to artifact directory, name, URL and `base_url`. Without it, `recover` finds no rows, republishes nothing, the
artifacts on disk are unreferenced, and every existing pull request comment points at a URL that no longer
answers — with nothing left that knows those comments exist to retract them.

**The artifacts are not disposable in practice.** In principle every one is reproducible by pushing again; in
practice "reproducible" means N × `npm install` and N reviewers noticing. `recover` drops any row whose
artifact directory is missing rather than republishing it (`internal/daemon/daemon.go:384`), so restoring a
database without the artifacts beside it silently shrinks the preview set.

### Backing up

The database is open with WAL and a 5-second busy timeout (`internal/store/store.go:91`). **A `cp` of
`docpreview.db` while the daemon is running is not a backup** — WAL means recent commits live in
`docpreview.db-wal`, and copying the three files individually can capture them at different instants. There is
no `docpreview backup` command and no `VACUUM INTO` exposed, so the two honest options are a filesystem or
volume snapshot (atomic across all three files), or `systemctl stop`, copy, `systemctl start` — which costs
the in-flight builds and a few seconds of preview downtime, and is entirely acceptable nightly.

`vault.age` is written whole by `Vault.Save` and is only written when a secret changes, so copying it is safe
at any time.

`artifacts/` is worth including if the backup target is cheap, and worth skipping if it is not — it is the
large, boring, regenerable part.

### What restoring does to live previews and their comments

This is the part that is easy to get wrong, because a restore is not neutral.

Restore a database **older** than the artifacts on disk and every preview created since the snapshot vanishes
from docpreview's view. Its artifacts sit unreferenced, its share is reaped at the next startup as an orphan
(`internal/daemon/daemon.go:375`), and its pull request comment still says `ready` and links to a URL that now
answers connection-refused. Nothing will ever fix that comment, because `Retract` is only called from
`teardown` (`internal/daemon/daemon.go:536`) and teardown only ever runs for previews the database knows
about. **The remedy is to push to those pull requests, not to wait.**

Restore a database **newer** than the artifacts and `recover` deletes the rows whose directories are missing
and republishes the rest. Quieter, but the same comments are stranded.

Restore `vault.age` from a snapshot taken before a webhook-secret rotation and every GitHub delivery starts
failing signature verification with a 401 that reveals nothing about why
(`internal/daemon/ingress.go:189`–`:195`). This is the failure most likely to be misread as a network problem.

The general rule worth writing into a runbook: **after any restore, walk the open documentation pull requests
and push an empty commit, or accept that some comments link to nothing.**

---

## Upgrades and rollback

The upgrade is `systemctl stop`, replace the binary, `systemctl start`. What it costs is bounded and known.

**What a restart costs.** Ready previews are republished from `data_dir/artifacts` with no clone and no build
(`internal/daemon/daemon.go:391`), so the outage is however long `recover` takes — dominated by one
`exposer.Publish` per preview, which under `local` is a map insert and under zrok is a share creation and an
overlay listener. **Under the `local` and `ziti` exposers the URL does not move**, because the path or
hostname is derived from the name. Under an exposer whose address is not derived from the name, republishing
can yield a different URL, in which case the row is rewritten and a fresh `ready` comment is published
(`internal/daemon/daemon.go:436`) — correct, and visible to every reviewer as a comment edit.

**What an in-flight build costs.** It is lost, per `Claim`. `workers` is the bound, so at most `workers`
reviewers see a stuck comment. To upgrade with zero lost builds: watch `GET /status` until `running` is 0 and
`pending` is 0, then stop. There is no drain mode and no `docpreview drain`; polling `/status` is the whole
mechanism.

**Rolling back the binary is safe for the data.** Artifacts are static files plus a stored `base_url`, so an
older binary serves a newer binary's artifacts unchanged.

**The schema is not versioned and there are no migrations.** `store.Open` executes one `CREATE TABLE IF NOT
EXISTS` block and nothing else (`internal/store/store.go:36`–`:104`). There is no `PRAGMA user_version`, no
migrations table, and no version check. What that buys today: additive columns work by accident, because every
existing column carries a `DEFAULT` and both `INSERT` and `SELECT` name their columns explicitly — so an old
binary reading a database that a newer binary added a column to simply ignores it, and a new binary reading an old
database gets the column added by the `CREATE TABLE IF NOT EXISTS` on first open. That is a real property and
it makes rollback across an additive change work.

What it does not survive is the first change that is not additive: a renamed column, a changed type, a
backfill. **Adopt `PRAGMA user_version` before that change, not during it** — a schema version added while
also needing to migrate has to guess what version the existing rows are, and every deployed database is
already version 0 whether or not anybody wrote it down.

---

## Observability

### What exists

`GET /healthz` returns the four bytes `ok\n` (`internal/daemon/ingress.go:278`). That is a liveness probe and
nothing more. Note that `expose.Exposer.Kind`'s own doc comment claims it is "for logs and for the `/healthz`
payload" (`internal/expose/expose.go:102`) — there is no payload; the kind appears in `/status`.

`GET /status` is genuinely useful (`internal/daemon/daemon.go:1042`): the exposer kind, the pending count, the
running count, a row per preview with its state, URL and reason, and the last 60 state-change events. Its
states are composed at read time from the store, the job queue and the in-flight map, so it does not lie about
a build that is running — which is the failure mode it was written to fix.

`docpreview doctor` prints the config path, data dir, exposer, driver, every listener, the vault path and
secret count, the key source, and the SCM wiring, then runs every component's `Validate`
(`cmd/docpreview/main.go`, `cmdDoctor`). **Do not run it against a live daemon under the `ziti` exposer.**
`Ziti.Validate` binds the service (`internal/expose/ziti.go:110`), binding creates a terminator, and a second
terminator makes the router load-balance previews between the daemon and the doctor process
(`internal/config/config.go:120`–`:130`). Roughly half the preview requests fail for the duration of the
check. `doctor` is a pre-flight tool, not a health check.

### What is missing for somebody on call

**A readiness signal distinct from liveness.** The listeners start serving before `d.Run` — and therefore
before `recover` — is called in `cmdServe`, so `/healthz` answers `ok` while zero previews are republished. A
load balancer or a monitoring check cannot currently distinguish "up and serving" from "up and still
restoring" or, worse, "up and permanently unable to accept webhooks".

**The vault's state.** The single most important fact about a restarted daemon is whether the vault opened,
because a locked vault means every GitHub webhook gets a 501 and the only symptom is on GitHub's deliveries
page. It appears in one startup log line and in `doctor`, and in neither of the two endpoints. `TODO.md`
already carries the dashboard half of this ("Show the key source on the dashboard"); the monitoring half is
the same field.

**Rates and ages.** `/status` reports queue *depth* but not queue *age*, so a wedged worker and a busy one
look identical. There are no build durations, no failure counts, no last-successful-webhook timestamp, and no
uptime. The four alerts worth having are: process down; vault locked; oldest pending job older than
`build.timeout`; free space on `data_dir` below a build's worth.

The cheapest path to all of it is to extend the existing `/status` payload rather than to add a metrics
endpoint — every field named above is already in memory or one sqlite read away, and a JSON scrape is enough
for a service with one instance.

### Log volume, and where a secret could still leak

Logs are `slog` text to stderr (`newLogger` in `cmd/docpreview/main.go`), so under systemd journald owns
rotation and there is nothing to configure. Volume is low by design: a handful of lines per build plus one per
state change. **Build output does not go to stderr** — it goes to `data_dir/logs/<id>/<build>.log` and to live
subscribers, and it is bounded by `build.keep_logs`.

Redaction is thorough where it applies and it is worth being exact about the edges, because an unredacted log
looks precisely like a log with no secrets in it.

*Covered.* Everything a build prints, in six encodings, buffered by line so a secret split across two `Write`
calls is still caught, scrubbed in `buildlog.Writer.emit` before anything reaches disk or a subscriber. Every
failure reason and log excerpt on its way to a pull request comment, scrubbed again at
`internal/daemon/daemon.go:657`. Clone URLs, which carry installation tokens and never reach a log or a
persisted git remote.

*Not covered, and worth knowing.*

- **The daemon's own `slog` lines are not scrubbed.** Nothing wraps the handler in a redactor. Today no call
  site logs a secret value, but that is a property of the current call sites and not of the logger.
- **A credential helper's stderr goes straight to the journal.** `readExec` sets `cmd.Stderr = os.Stderr` on
  purpose, so an `op` prompt reaches the operator (`internal/vault/keysource.go:211`). A helper that prints the
  key to stderr instead of stdout puts the master key in the journal.
- **`KeySource.Describe` prints the `exec:` argv**, deliberately, on the reasoning that a command is a locator
  and not a secret (`internal/vault/keysource.go:137`). A command with the credential *in* it — anything of the
  form `exec:echo <key>` — is logged verbatim at startup.
- **A secret shorter than four characters is refused and never registered**, with a count logged and not the
  name. It will appear in build logs, because the redactor was never armed with it.
- **A transformed secret survives.** Six encodings, and nothing else.

---

## Resource sizing and blast radius

Every build is an `npm install` and then a static-site build. That single sentence sets the sizing.

**CPU and memory.** `workers` bounds concurrency and defaults to 2 (`internal/config/config.go:445`); the
worker pool exists precisely because letting every webhook build at once makes each build slower than running
two at a time (`internal/daemon/daemon.go:1`–`:8`). The `docker` driver caps each build at 4 GB and 2 CPUs
(`internal/pipeline/build.go:336`). **The `local` driver caps nothing** — a build that wants 4 GB gets it, and
that is listed as deliberately not defended (`docs/design/10-security.md`). With `workers: 2` under the local
driver, budget for two simultaneous Node builds: 2 vCPU and 4 GB is workable, 4 vCPU and 8 GB is comfortable.

**Time.** `build.timeout` defaults to 15 minutes and is applied per build
(`internal/pipeline/build.go:123`). A build that hangs occupies a worker for that long; two hung builds at
`workers: 2` stop the queue entirely for fifteen minutes. That is the argument for alerting on queue age
rather than queue depth.

**Disk, in order of how much it grows.**

- `artifacts/<id>/` — one built documentation site per open pull request, retained until `preview.ttl`
  (default 72 hours) or the pull request closes. Tens of megabytes each for a Docusaurus site.
- `workspaces/<id>/` — a depth-1 clone plus `node_modules`, which is the largest thing on the disk while a
  build runs and is removed as soon as the output is copied out (`internal/daemon/daemon.go:709`). **A process
  killed mid-build leaves one behind and nothing sweeps it**: `recover` does not look at `workspaces/`, and it
  is only cleared by the next clone of the same pull request (`internal/pipeline/clone.go:75`) or by teardown
  (`internal/daemon/daemon.go:529`). A pull request that is killed mid-build and never pushed to again leaks
  its `node_modules` until the TTL tears the preview down. Worth a periodic check on a box that is restarted
  often.
- `logs/<id>/` — swept by `build.keep_logs`, 7 days by default, and skipped for previews with a live writer
  (`internal/buildlog/store.go:240`).
- The npm cache under `$HOME`, which grows monotonically and is nobody's responsibility.

The hourly reaper does all three sweeps in one tick (`internal/daemon/daemon.go:971`). Hourly is right because
TTLs are in days, but it means **disk pressure is relieved on the hour and not on demand**: a burst of twenty
pull requests can fill a disk that the reaper would have tidied. Size for the peak, not the steady state — 20
to 40 GB for a documentation repository of ordinary size.

**Blast radius.** A full disk degrades rather than stops: `buildlog.NewStore` failing is not fatal and the
daemon builds without capturing output (`internal/daemon/daemon.go:113`), and a log that cannot be opened
produces a warning and an untailed build (`internal/daemon/daemon.go:756`). What does fail is the commit
phase, and it fails safely — `replaceDir` errors before anything is published, and a `SavePreview` that fails
after a publish withdraws the publication rather than leaving a live share nothing records
(`internal/daemon/daemon.go:866`).

---

## Exposure

The daemon binds `127.0.0.1:8471` by default and the right production shape keeps it there.

**Only the webhook endpoint must be reachable from the internet.** `POST /webhook/github` is internet-facing
by design and defended accordingly: the body is size-capped before it is read, HMAC-SHA256 verified with
`hmac.Equal`, and rejected with a 401 that says nothing (`docs/design/10-security.md`, "The webhook
endpoint").

**Nothing else needs to be.** The dashboard and `/status` enumerate every open documentation pull request and
have no authentication at all — that is deliberate and load-bearing, on the assumption that they are reachable
only on loopback or over an overlay that authorizes the dialer
(`internal/daemon/ingress.go:283`–`:289`). Previews themselves reach reviewers through the exposer, not
through the ingress — except under the `local` exposer, where they are paths on the ingress listener
(`internal/expose/local.go:111`), which is exactly why `local` is for trying the pipeline and not for sharing
a link off-box.

So the tunnel in front of the daemon should forward **one path**, not the whole listener. A reverse proxy
with a single `location /webhook/github`, or a zrok share or ziti service scoped to it. Reach the dashboard by
SSH port-forward or over an overlay.

### The secrets surface refuses to serve unless every listener is loopback

`SecretsAdmin.Available` walks `cfg.Listeners` and returns false if any of them is a ziti listener or a TCP
listener whose host is not loopback (`internal/daemon/secrets.go:86`–`:105`). Every mutating route is gated on
it (`:302`); the read route is not, so the page can explain itself.

The reasoning holds: on a loopback-only daemon the boundary is the same one that already protects
`docpreview vault set` — anyone who can reach `127.0.0.1` can run the binary — and the surface adds no
reachability a shell did not already have. The moment a listener is not loopback, it does.

The production consequence, stated plainly: **a box that binds anything other than loopback has no unlock
button and no way to enter a credential from the browser.** On such a box `vault.key_source` is not a
convenience, it is mandatory, and every credential has to be placed with `docpreview vault set` over SSH.
That is a coherent configuration. What is not coherent is discovering it after a reboot.

This is the strongest argument for the recommended shape: keep every listener on loopback, put the tunnel in
front, and the setup page keeps working over an SSH forward — the tunnel terminates outside the process, so
`Available` still sees a loopback-only config, which is true.

---

## Tier 2: actual HA

Five assumptions in the code say "there is exactly one docpreview". Ordered by *when the decision has to be
made*, not by difficulty: the first two are interface contracts, and changing an interface with four
implementations is a morning's work while changing one with eight is a project.

### 1. Startup reaping deletes the other instance's live previews — interface, decide first

`recover` now calls `exposer.Reap(ctx, keep)` with what this instance's database claims, rather than the `nil` it
used to pass — see **Adoption** in `docs/design/02-exposers.md`. That was done for startup latency, not for HA,
and it narrows this blocker without closing it: instance B no longer deletes everything, but it still deletes
whatever *its own* database does not claim, which includes every preview belonging to instance A.

**A second instance starting reaps the first instance's live shares.** How badly depends on the exposer, and
the differences are instructive:

- `frontdoor` lists every share in the **tenant** and deletes any carrying the `docpreview:` tag prefix that
  is not in `keep` (`internal/expose/frontdoor.go:284`–`:314`). Worst case: instance B's boot deletes all of
  instance A's previews.
- `zrok` scopes both `Reap` and `reapName` to the environment's own ziti identity
  (`internal/expose/zrok.go:300`, `:352`), so two instances with *separate* `zrok enable` environments do not
  reap each other — accidentally, not by design. They still share a namespace of names, so the second
  instance's `Publish` fails on a name the first holds. And `reapName` reclaims a name held by "a share we do
  not know about" and retries once (`internal/expose/zrok.go:177`); with two instances, a share we do not know
  about is the other instance's live preview.
- `ziti.Reap` is a no-op (`internal/expose/ziti.go:278`), so this blocker does not apply — but blocker 5 does,
  harder.
- `local.Reap` prunes an in-process map, so it is per-instance by construction.

**The change:** an instance identity that reaches the remote object. Concretely, a configured `instance` string
folded into the target tag alongside the preview ID, and a `Reap` contract of "delete what *this instance*
owns and `keep` does not name", with a separate, explicitly-invoked sweep for "delete anything docpreview owns
that no instance claims". That second mode is the audit command `TODO.md` already wants ("No audit command").
This is a small change to `Spec`, one line in each implementation's tag construction, and a rewrite of one
sentence of contract — today. It is worth doing **before the fifth exposer exists**, whether or not HA
follows, because it also fixes the single-instance case of two daemons sharing one zrok account.

### 2. The commit lock is in-process, and publishing is destructive — interface, decide second

`commitLock` returns a `*sync.Mutex` from a map keyed by preview ID (`internal/daemon/daemon.go:311`). Its own
comment explains why the lock exists: publishing a name is *destructive* to whoever holds it, because the
exposer withdraws the existing share for that name first, so a build that reaches `Publish` without holding
the lock does not merely waste work — it tears down the newer preview and replaces it with older content
(`internal/daemon/daemon.go:301`–`:310`).

A mutex in one process does not serialize two processes. **Two instances building the same pull request tear
each other down**, alternately, for as long as both keep receiving the webhook. This is not a rare race: it is
the ordinary case, because both instances would receive the same delivery.

**The change:** the commit phase needs a lease, not a mutex — an owner plus an expiry, acquired before the
liveness check and released after `SavePreview`, with the same "am I still current?" semantics extended from
"is this pointer still in my map" to "do I still hold the lease". The natural home is the store, which is
already the coordination point and already has an atomic primitive in `Claim`. That works only if the store is
shared, which is blocker 3. The reason this is an interface decision rather than an implementation one is that
`Daemon` currently holds a concrete `*store.Store` and a concrete `expose.Exposer`; whether the lease lives
behind the store interface or beside it is the kind of choice that is free before it is written and awkward
afterwards.

### 3. sqlite is on local disk — operational

`Claim` is already correct across processes: `DELETE ... RETURNING` is atomic in sqlite, and the whole point
of the primary key on `jobs` is that two workers cannot take the same job
(`internal/store/store.go:148`–`:153`). Two processes on the *same filesystem* would coordinate correctly
today, WAL and the 5-second busy timeout included (`internal/store/store.go:91`).

**Not over NFS or SMB.** sqlite's locking depends on the filesystem honouring the locking primitives it uses,
and network filesystems are the documented case where they do not. A shared `data_dir` on NFS is the tempting
answer to blockers 3 and 4 at once and it is the one that corrupts the database.

**The change:** either a shared block device with exactly one writer — which is the fast-failover option
below, not HA — or a database that is a server. The latter is a real port: `internal/store` is a concrete
struct with SQL inline, so a second backend means extracting an interface. That is cheap now and it is the
same extraction blocker 2 wants.

### 4. Artifacts, workspaces and logs are local disk — operational, and the deepest

`recover` republishes from `p.ArtifactDir`, a path on this machine
(`internal/daemon/daemon.go:384`–`:391`), and `runPipeline` moves build output into
`data_dir/artifacts/<preview id>` (`internal/daemon/daemon.go:812`). **An instance that did not run the build
has nothing to serve.** A second instance restoring from a shared database would find every artifact directory
missing and, following the existing logic, delete the rows
(`internal/daemon/daemon.go:384`) — turning a failover into a mass teardown.

Three options, in increasing order of how much they change:

- **Shared filesystem for `artifacts/` only**, keeping the database elsewhere. This is the least invasive and
  it avoids putting sqlite on NFS. It does mean two instances writing `artifacts/<id>` for the same preview,
  which `replaceDir` handles by removing the destination first (`internal/daemon/daemon.go:1165`) — non-atomic
  across a shared mount, and serialized by the lease from blocker 2 if that exists.
- **Object storage**, with `preview.Site` reading from a bucket rather than a directory. A clean seam
  (`preview` already owns "serve a built directory as an `http.Handler`") and a real amount of work.
- **Route to the builder.** Each instance serves what it built, and the layer in front knows which. This is
  the honest answer, because it is what an HA exposer has to know anyway — see blocker 5 — but it means
  preview URLs stop being location-independent.

### 5. Preview URLs are per-instance by construction — operational, and it constrains everything above

The `local` exposer serves previews as paths on the daemon's own listener
(`internal/expose/local.go:111`, `internal/daemon/ingress.go:131`), so its URLs *are* one instance's address.
The `ziti` exposer binds one wildcard service, and **exactly one process may bind a given service**: binding
creates a terminator, a second binding creates a second one, and the router load-balances between them under
the default strategy — so two docpreviews sharing a service each answer about half the requests
(`internal/config/config.go:120`–`:130`, restated in `TODO.md` under "Known limits").

So HA is impossible under `local` and requires either a second service or per-preview routing under `ziti`.
zrok and Frontdoor are the two that could work, both because the URL is a remote object rather than a local
address — which is why blocker 1 is the one to fix first: on those two exposers, startup reaping is the
immediate blocker and the URL is not.

### The option that is not HA and is usually enough

**One active instance, a standby, and a shared or replicated data volume.**

Only one process runs at a time, so all five blockers stay dormant: nothing reaps a live share, the in-process
mutex is the only mutex, sqlite has one writer, the artifacts are local to whoever is serving, and exactly one
process binds the ziti service. Failover is: stop A (or observe that it is gone), attach the volume to B,
start B, and `recover` republishes every ready preview from the artifacts on the volume in a second or two.

The cost is a failover window of tens of seconds to a couple of minutes, during which previews 404 and
webhook deliveries fail — and GitHub retries deliveries, so the queue is not necessarily lost. Against that:
**the thing being protected is a documentation preview.** A two-minute gap in preview availability is not the
same class of event as a two-minute gap in a payments API, and the five changes above are a genuine project.

The one hard requirement is a fence: two instances mounting the same volume read-write is worse than an
outage, because it is the sqlite-corruption case *and* the mutual-reaping case at once. Whatever provides the
fence — a cloud volume that can only attach to one instance, a cluster manager with a lease, or a human
runbook — is the part that has to be right.

**Recommendation: build this, and fix blocker 1 anyway.** Fast failover satisfies "long running, restart
surviving" and most of "ha-type install" for a fraction of the work, and the instance-identity change in
blocker 1 is worth making on its own merits while there are four exposers rather than eight.

---

## The order to build it in

1. **Write the systemd unit and the config beside it**, master key as a systemd credential named by its
   literal `/run/credentials/docpreview.service/` path. Confirm a reboot brings the daemon back with an
   unlocked vault and a GitHub client, by checking that `/webhook/github` no longer answers 501.
2. **Set up the backup**: a volume snapshot, or a nightly stop-copy-start of `vault.age` and `docpreview.db`,
   plus wherever the master key lives. Then *restore it once* into a scratch box, and write down what it did
   to the comments — that paragraph is the runbook.
3. **Extend `/status` with the four facts on-call needs**: vault locked or open, recovery complete, oldest
   pending job age, free space on `data_dir`. Alert on those four and on process-down.
4. **Add `PRAGMA user_version`** and a version check that refuses to start on a database from the future.
   Before the first non-additive schema change, not during it.
5. **Sweep orphaned workspaces at startup** — any `workspaces/<id>` with no in-flight build is dead by
   definition, exactly as the artifacts are handled. Cheap, and it removes the only unbounded disk leak.
6. **Add an instance identity to `Spec` and rewrite the `Reap` contract** to "what I own, minus `keep`", with
   the cross-instance sweep as an explicit audit command. This is blocker 1, it is worth doing whether or not
   HA follows, and it is cheapest today.
7. **Build the fast-failover pair**: shared or replicated volume, a fence, a documented promote procedure.
   Measure the actual failover window rather than estimating it.
8. **Extract a store interface**, which blockers 2 and 3 both need and neither can start without.
9. **Then, and only if the failover window turns out to be unacceptable**: the commit lease, a server
   database, and shared artifact storage — in that order, because each is useless without the one before it.

---

## Not verified

Inferred from reading rather than confirmed by running:

- **No systemd unit has been run.** The unit file above is written from the code's requirements — the 30-second
  `srv.Shutdown` budget, `$HOME` for the npm cache, `ReadWritePaths` for `data_dir` — and has not been started
  on a box. `Type=exec` in particular assumes the process does not fork, which is true of the code but
  untested under systemd.
- **The systemd credential path and permissions.** That `LoadCredential=` produces mode 0400 owned by the
  unit's user at `/run/credentials/<unit>/<name>`, and that this satisfies the `0o077` check at
  `internal/vault/keysource.go:184`, is from systemd's documented behaviour and not from a run.
- **The Windows service wrapper.** That `sc.exe create` against the bare binary fails is inferred from the
  absence of any `windows/svc` handler in the tree, not observed. Which wrapper to use is a recommendation with
  no testing behind it.
- **Container specifics.** The npm cache location (`$HOME/.npm`) is inferred from npm's defaults, since nothing
  in `internal/pipeline` sets `npm_config_cache`. No image has been built.
- **sqlite over NFS.** Asserted from sqlite's documented locking caveats, not from an attempt.
- **Backup consistency.** That copying `docpreview.db` alone under WAL can miss recent commits is from sqlite's
  documented WAL behaviour. No corrupted restore has been produced deliberately.
- **The frontdoor tenant-wide reap** follows from `internal/expose/frontdoor.go:284` listing `/shares` with no
  instance filter, but Frontdoor's wire format has never been exercised against a live tenant at all — see
  `TODO.md`, "Verification gaps". The blast radius is real if the endpoint behaves as modelled.
- **Resource sizing.** The 2 vCPU / 4 GB / 20–40 GB figures are reasoned from "two concurrent Docusaurus builds
  plus one artifact directory per open pull request", not measured on a real repository.
- **Failover timing.** "Tens of seconds to a couple of minutes" is an estimate. `recover`'s own cost is one
  `Publish` per ready preview and has not been timed at scale.
