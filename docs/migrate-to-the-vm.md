# Moving this installation off the laptop

The specific version of [the move runbook](../www/docs/runbooks/move-an-installation.md), with this
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
| Data directory | `D:\worktrees\tangents\vercel-replacement\.docpreview\data` |
| Config | `D:\worktrees\tangents\vercel-replacement\.docpreview\config.yml` |
| Master key | `C:\Users\claude\AppData\Local\docpreview\master.key` |
| zrok environment | **`C:\Users\claude\.zrok2`** — system-scoped, so it is *not* under the data directory |
| Hostname prefix | `a` |
| Exposer | `zrok2`, selected on the dashboard rather than in the config file |
| Share names | `docpreview` (webhook), `docpreview-dash` (dashboard) |
| Source control | GitHub App 4420399, plus a Bitbucket access token |
| Previews | 10 serving, 32 publications |

The zrok scope is the one that makes this move more than a file copy.

## The steps

### 1. Prepare the VM

[The install runbook](../www/docs/runbooks/linux-service.md), steps 1 to 3: docker, the binary, and
`install.sh`. Stop before writing a config — this installation brings its own.

Cross-compile from the laptop rather than installing Go on the VM:

```powershell
$env:GOOS='linux'; $env:GOARCH='amd64'
go build -o dist/docpreview ./cmd/docpreview
```

### 2. Stop everything on the laptop

All three processes. **Nothing may overlap**: two daemons on one zrok account fight over the same
names, and the second to publish takes them.

```powershell
Get-Process docpreview | Stop-Process -Force
```

Confirm: `curl.exe -s -o NUL -w "%{http_code}" http://127.0.0.1:8471/healthz` should fail.

### 3. Copy the database with its WAL

```powershell
$data = "D:\worktrees\tangents\vercel-replacement\.docpreview\data"
Copy-Item "$data\docpreview.db","$data\docpreview.db-wal","$data\docpreview.db-shm" $env:TEMP\out\ -EA SilentlyContinue
Copy-Item "$data\vault.age" $env:TEMP\out\
Copy-Item "C:\Users\claude\AppData\Local\docpreview\master.key" $env:TEMP\out\
```

The `-wal` file matters. Copying the `.db` alone loses whatever has not been checkpointed, which on
a daemon stopped a moment ago is the most recent thing it did.

### 4. Copy the zrok environment — the part that is easy to get wrong

This installation is **system-scoped**, so the enrolment is in `C:\Users\claude\.zrok2` and not in
the data directory.

```powershell
Copy-Item -Recurse "C:\Users\claude\.zrok2" "$env:TEMP\out\zrok2"
```

On the VM it goes to **`/var/lib/docpreview/zrok2`**, and then the installation is told to use it:

```bash
sudo -u docpreview /usr/local/bin/docpreview zrok use project -config /etc/docpreview/config.yml
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
sudo install -o docpreview -g docpreview -m 0600 docpreview.db* vault.age /var/lib/docpreview/
sudo cp -r zrok2 /var/lib/docpreview/zrok2
sudo chown -R docpreview:docpreview /var/lib/docpreview
sudo install -o root -g docpreview -m 0640 master.key /etc/docpreview/master.key
```

### 6. Write the config

Copy `config.yml` across and change the two values that were only true on Windows:

```yaml
data_dir: "/var/lib/docpreview"

vault:
  key_source: "file:/etc/docpreview/master.key"
```

Everything else travels. The exposer, the prefix `a`, the console passwords, the project rows and
their credentials are all in the database.

### 7. Check before starting

```bash
sudo -u docpreview /usr/local/bin/docpreview doctor -config /etc/docpreview/config.yml
sudo -u docpreview /usr/local/bin/docpreview zrok status -config /etc/docpreview/config.yml
```

Expect `exposer: zrok2 (selected on the dashboard, not from the config file)`, `login: required`,
both source controls listed, and `zrok status` marking **this installation** as in use and enrolled.

If `zrok status` says the project directory is empty, the zrok copy landed in the wrong place — fix it
before starting, or the daemon will reap nothing, publish nothing, and look broken.

### 8. Start

```bash
sudo systemctl enable --now docpreview
sudo systemctl enable --now docpreview-webhook
sudo systemctl enable --now docpreview-dashboard
journalctl -u docpreview -f
```

Both tunnel units already carry `-zrok-home /var/lib/docpreview/zrok2`, which is required now that
the environment is project-scoped. Without it they would look in the service account's home, find
nothing, and claim the names from a different account.

Startup republishes every preview. Under zrok the names are reserved and the URLs are derived from
them, so **the URLs come back unchanged** and nothing in any pull request comment moves.

### 9. Confirm, then decommission the laptop

- Every preview `ready` on the dashboard, and one URL opened to be sure.
- `docpreview webhook-check -url https://docpreview.shares.zrok.io/webhook/github` from the VM.
- A test pull request, or a push to a default branch, and watch it build.

Then make sure the laptop cannot start it again. It still holds a copy of the vault, the master key
and the zrok enrolment — which is a second machine able to claim the same names, and a copy of
every credential.

## After the move

Three things become possible that are not possible today, and they are the reason to do it:

1. **The demo links stop dying.** A pull request comment and the preview it points at survive being
   clicked next week. See [the demo notes](./demo-and-branding.md).
2. **The `main` branch preview becomes the published docs site** at a stable URL — the tool hosting
   its own documentation.
3. **Push-triggered rebuilds start mattering.** Subscribe the `push` event on one repository and
   the `main` preview keeps itself current.
