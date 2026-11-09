# Moving this installation off the laptop

The specific version of [the move runbook](../www/docs/guides/move-an-installation.md), with this
installation's actual paths in it. The runbook is the general procedure; this is the shopping list.

Delete this file once the move is done.

## The target

`docprev`, as surveyed on 2 August 2026:

| | |
|---|---|
| Amazon Linux 2023, `x86_64` | fine |
| 2 vCPU | fine |
| 3.7 GiB RAM, no swap | fine — it was 912 MiB and was resized |
| 100 GB disk | fine |
| No docker, node, go, zrok | expected; the install runbook adds what is needed |
| Outbound HTTPS to zrok and GitHub | confirmed working |
| Nothing listening but SSH | good |

:::note Why the memory mattered

At 912 MiB this could not have worked. A Docusaurus build peaks in prerendering, and `cdzrok` —
1.2 GiB — already fails `docusaurus-shared` there with `ENOMEM`, two minutes into a build that had
looked fine. Adding swap is not a substitute: Node's heap limit is not a memory-pressure decision,
and a build that swaps takes minutes instead of seconds.

:::

Nothing on this box has been changed. Everything below is still to do.

## What this installation actually is

| | |
|---|---|
| Data directory | `D:\worktrees\tangents\vercel-replacement\.nfpreview\data` |
| Config | `D:\worktrees\tangents\vercel-replacement\.nfpreview\config.yml` |
| Master key | `C:\Users\claude\AppData\Local\nfpreview\master.key` |
| zrok environment | **`C:\Users\claude\.zrok2`** — system-scoped, so it is *not* under the data directory |
| Hostname prefix | `a` |
| Exposer | `zrok2`, selected on the dashboard rather than in the config file |
| Share names | `nfpreview` (webhook), `nfpreview-dash` (dashboard) |
| Source control | GitHub App 4420399, plus a Bitbucket access token |
| Previews | 7 serving, 16 publications, as of 4 August 2026 — check with `shares list` before copying, and expect the same numbers on the far side |

The zrok scope is the one that makes this move more than a file copy.

## The steps

### 1. Prepare the VM

One command. The package creates the service account, both directories and the three units, brings
docker with it, and starts nothing:

```bash
sudo dnf install -y \
  https://github.com/openziti-test-kitchen/docpreview/releases/download/v0.5.0/nfpreview-0.5.0-1.x86_64.rpm
```

**Stop there.** The next steps of
[the install guide](../www/docs/guides/linux-service.md) write a config, a master key and a vault,
and this installation brings all three with it.

Confirm what you have before copying anything onto it:

```bash
nfpreview version
docker run --rm hello-world
```

Already done on `docprev` as of 4 August 2026 — v0.2.0, docker 25.0.16 — with a throwaway config and
a one-entry vault from testing the package. **Delete both before step 5**, or the copy lands beside
them and `doctor` reports a vault that is neither the old one nor empty:

```bash
sudo systemctl stop nfpreview nfpreview-webhook nfpreview-dashboard
sudo rm -rf /var/lib/nfpreview/vault.age /var/lib/nfpreview/nfpreview.db* /etc/nfpreview/config.yml /etc/nfpreview/master.key
```

### 2. Stop everything on the laptop

All three processes. **Nothing may overlap**: two daemons on one zrok account fight over the same
names, and the second to publish takes them.

```powershell
Get-Process nfpreview | Stop-Process -Force
```

Confirm: `curl.exe -s -o NUL -w "%{http_code}" http://127.0.0.1:8471/healthz` should fail.

### 3. Copy the database, its WAL, and the artifacts

```powershell
$data = "D:\worktrees\tangents\vercel-replacement\.nfpreview\data"
Copy-Item "$data\nfpreview.db","$data\nfpreview.db-wal","$data\nfpreview.db-shm" $env:TEMP\out\ -EA SilentlyContinue
Copy-Item "$data\vault.age" $env:TEMP\out\
Copy-Item "C:\Users\claude\AppData\Local\nfpreview\master.key" $env:TEMP\out\
```

The `-wal` file matters. Copying the `.db` alone loses whatever has not been checkpointed, which on
a daemon stopped a moment ago is the most recent thing it did.

**`artifacts/` matters as much, and is the easy one to leave behind.** Startup republishes each
recorded preview *from the built site already on disk*, so without it the database describes previews
whose output is absent — `recovered previews_restored=0` — and every one is rebuilt from source
instead. On this installation that is 600 MB and the difference between URLs answering in seconds
and a rebuild queue that takes an hour:

```powershell
tar -czf $env:TEMP\out\artifacts.tar.gz -C $data artifacts
```

`logs/` and `workspaces/` are not worth copying. Build logs are history nothing needs to run, and a
workspace is scratch — worse, a workspace left by a killed build contains root-owned files that the
service account on the far side cannot delete, which fails every later build for that preview.

### 4. Copy the zrok environment — the part that is easy to get wrong

This installation is **system-scoped**, so the enrolment is in `C:\Users\claude\.zrok2` and not in
the data directory.

```powershell
Copy-Item -Recurse "C:\Users\claude\.zrok2" "$env:TEMP\out\zrok2"
```

On the VM it goes to **`/var/lib/nfpreview/zrok2`**, and then the installation is told to use it:

```bash
sudo -u nfpreview /usr/local/bin/nfpreview zrok use project -config /etc/nfpreview/config.yml
```

Doing it this way rather than dropping it in the service account's `~/.zrok2` means the whole
installation is one directory from now on, and the next move is a straight copy.

:::danger Do not enrol a fresh environment instead

The reserved names — every preview URL anybody has been sent — are objects owned by *this*
environment and counted against the account's quota. A new environment cannot reclaim them, cannot
delete them, and cannot publish under them. The URLs would all change and the old ones would sit
there, unusable, forever.

:::

### 5. Place the files

```bash
sudo install -o nfpreview -g nfpreview -m 0600 nfpreview.db* vault.age /var/lib/nfpreview/
sudo cp -r zrok2 /var/lib/nfpreview/zrok2
sudo chown -R nfpreview:nfpreview /var/lib/nfpreview
sudo install -o nfpreview -g nfpreview -m 0600 master.key /etc/nfpreview/master.key
```

### 6. Write the config

Copy `config.yml` across and change the values that were only true on the laptop:

```yaml
data_dir: "/var/lib/nfpreview"
log_file: "/var/lib/nfpreview/daemon.log"

workers: 1

build:
  cpus: 2
  memory: 3g

vault:
  key_source: "file:/etc/nfpreview/master.key"
```

The three paths are obvious. **The three sizing values are the ones that bite**, because the laptop's
are larger than the VM and nothing warns you:

- `cpus: 4` on a 2-core box makes docker refuse every build —
  `Range of CPUs is from 0.01 to 2.00, as there are only 2 CPUs available`, visible only in the build
  log.
- `workers: 2` with `memory: 3g` permits 6 GB on a 4 GB machine. Both builds start and the *host*
  runs out, which takes ssh down rather than failing a build.
- Add swap before the first build. See
  [sizing the build](../www/docs/guides/linux-service.md#sizing-the-build-for-the-box).

Everything else travels. The exposer, the prefix `a`, the console passwords, the project rows and
their credentials are all in the database.

### 7. Check before starting

```bash
sudo -u nfpreview /usr/local/bin/nfpreview doctor -config /etc/nfpreview/config.yml
sudo -u nfpreview /usr/local/bin/nfpreview zrok status -config /etc/nfpreview/config.yml
```

Expect `exposer: zrok2 (selected on the dashboard, not from the config file)`, `login: required`,
both source controls listed, and `zrok status` marking **this installation** as in use and enrolled.

If `zrok status` says the project directory is empty, the zrok copy landed in the wrong place — fix it
before starting, or the daemon will reap nothing, publish nothing, and look broken.

### 8. Start

```bash
sudo systemctl enable --now nfpreview
sudo systemctl enable --now nfpreview-webhook
sudo systemctl enable --now nfpreview-dashboard
journalctl -u nfpreview -f
```

Both tunnel units already carry `-zrok-home /var/lib/nfpreview/zrok2`, which is required now that
the environment is project-scoped. Without it they would look in the service account's home, find
nothing, and claim the names from a different account.

Startup republishes every preview. Under zrok the names are reserved and the URLs are derived from
them, so **the URLs come back unchanged** and nothing in any pull request comment moves.

### 9. Confirm, then decommission the laptop

- Every preview `ready` on the dashboard, and one URL opened to be sure.
- `nfpreview webhook-check -url https://nfpreview.shares.zrok.io/webhook/github` from the VM.
- A test pull request, or a push to a default branch, and watch it build.

Then make sure the laptop cannot start it again. It still holds a copy of the vault, the master key
and the zrok enrolment — which is a second machine able to claim the same names, and a copy of
every credential.

## Replacing the binary afterwards

The three units run `/usr/local/bin/nfpreview`, so that is the only path a deploy may write:

```bash
scp build.claude/nfpreview-linux-amd64 docprev:/tmp/nfpreview-new
ssh docprev 'sudo systemctl stop nfpreview nfpreview-webhook nfpreview-dashboard \
  && sudo install -m 0755 /tmp/nfpreview-new /usr/local/bin/nfpreview \
  && sudo systemctl start nfpreview nfpreview-webhook nfpreview-dashboard \
  && nfpreview version'
```

Stop first: a running daemon holds the file, and on Linux replacing it under a live process leaves the
old code running with the new binary on disk — which reads as a deploy that did nothing.

**A copy at `/usr/bin/nfpreview` is not it, and writing there is silent.** `/usr/local/bin` comes
first on `PATH`, so the stray copy is shadowed: the install succeeds, the services restart, and
`nfpreview version` answers with the version that was already there — the same answer a deploy that
never happened gives. `which -a nfpreview` lists every copy, and the units' `ExecStart` is the
authority on which one runs.

## After the move

Three things become possible that are not possible today, and they are the reason to do it:

1. **The demo links stop dying.** A pull request comment and the preview it points at survive being
   clicked next week. See [the demo notes](./demo-and-branding.md).
2. **The `main` branch preview becomes the published docs site** at a stable URL — the tool hosting
   its own documentation.
3. **Push-triggered rebuilds start mattering.** Subscribe the `push` event on one repository and
   the `main` preview keeps itself current.
