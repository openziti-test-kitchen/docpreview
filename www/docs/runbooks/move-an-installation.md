---
id: move-an-installation
title: Runbook — move an installation
sidebar_position: 5
---

# Runbook: move an installation

Moving docpreview to another machine, or into a container, is a copy of four things and one edit. There is no
export command, because there is nothing to export that a file copy does not already give you.

:::danger Stop the old instance before starting the new one

Two daemons sharing one zrok account **delete each other's live shares**. `Reap` runs at startup on the reasoning
that every share it finds belongs to a process that is gone — true for one daemon per account, false the moment
there are two. Both will tear down each other's previews while every log looks healthy.

A move is stop, copy, start. Never an overlap.

:::

## What travels

| What | Where it is | |
|---|---|---|
| The database | `<data_dir>/docpreview.db` | **and its `-wal` file** — Step 2 |
| The credential store | `<data_dir>/vault.age` | encrypted; useless without the key |
| The master key | wherever `vault.key_source` points | deliberately outside `data_dir` |
| The zrok environment | `~/.zrok2` | the identity that owns every reserved name |

And one thing that is edited rather than copied: `config.yml`, which holds absolute paths in `data_dir` and
`vault.key_source`.

Three directories are deliberately left behind. `workspaces/` is one clone per commit and is recreated by the next
build. `logs/` is build history that nothing reads to work. `artifacts/` is the built sites — see Step 3.

## Step 1 — Stop everything on the old machine

All three processes: the daemon and both shares.

```powershell
Get-Process docpreview | Stop-Process
```

```bash
pkill docpreview
```

Confirm nothing answers before continuing. A daemon still running is the one failure mode in this runbook that
destroys something:

```powershell
curl.exe -s -o NUL -w "%{http_code}" http://127.0.0.1:8471/healthz
```

`000` — or a connection error — is what you want. `200` means something is still up.

## Step 2 — Take the database as one consistent file

docpreview opens sqlite in WAL mode, so a write is appended to `docpreview.db-wal` and folded into
`docpreview.db` later, at a checkpoint. That is what keeps the reaper's reads from blocking a worker's writes.

:::warning `docpreview.db` on its own is not the current state

It is the state as of the last checkpoint. Everything since is in the `-wal` file, which is routinely larger than
the database — on a live instance, a 77 KB database beside a 519 KB WAL is normal. Copy the `.db` alone and the new
instance starts on a database missing projects, previews and settings, with no error, because what you copied is a
perfectly valid older database.

:::

`VACUUM INTO` writes a single file with the WAL already folded in:

```powershell
sqlite3 "$data\docpreview.db" "VACUUM INTO '$out\docpreview.db'"
```

Copying all three files together works too, and only with the daemon stopped:

```powershell
Copy-Item "$data\docpreview.db*" -Destination $out
```

Prefer the first when the destination is a container volume or another host: one file cannot arrive as a database
with a stale WAL beside it.

## Step 3 — Decide about artifacts

Artifacts are the built sites the previews serve. Without them the new instance republishes every preview URL and
**serves 404s until each one rebuilds**, because the share is live and the directory behind it is empty.

Copy them if the URLs have to work the moment the new instance starts:

```powershell
Copy-Item -Recurse "$data\artifacts" "$out\artifacts"
```

Skip them if a gap is acceptable — a rebuild from the dashboard replaces each one. They are usually the bulk of the
data directory, which is what decides it in practice.

## Step 4 — Copy the credentials and the zrok identity

```powershell
Copy-Item "$data\vault.age" $out
Copy-Item "$env:LOCALAPPDATA\docpreview\master.key" $out
Copy-Item -Recurse "$env:USERPROFILE\.zrok2" "$out\zrok2"
```

The vault is encrypted and the key is not in it. Bringing one without the other produces a daemon that starts,
serves its setup page, and cannot decrypt anything it holds.

:::note The zrok environment is not re-creatable

Enabling a fresh environment on the new machine does not merely lose the preview URLs. The reserved names are
objects owned by the old environment and counted against the account's quota, and nothing on the new machine can
reclaim or delete them. See [Runbook — zrok v2](./zrok2.md).

:::

## Step 5 — Place the files and edit the config

On the new machine, put the files where they belong, then change the two absolute paths in `config.yml`:

```yaml
data_dir: "/srv/docpreview/data"

vault:
  key_source: "file:/etc/docpreview/master.key"
```

Everything else in the file is portable. `data_dir` and `key_source` are the two values that were true only on the
old machine.

## Step 6 — Check before starting

```powershell
docpreview doctor -config /srv/docpreview/config.yml
```

`doctor` reads the config, the vault and the exposer's settings and reports what it cannot reach. It costs a second
and answers the question a failed build answers ten minutes later.

Then start the daemon, and the two shares once it answers — the shares forward to the daemon and log errors until
it does.

## Step 7 — Confirm the previews came back

Startup reaps what it owns and republishes from the database. Watch it finish rather than guessing:

```powershell
.\demo\Wait-Docpreview.ps1
```

Then read `/status`. Every restored preview should be `ready` with a URL that answers. A preview whose URL 404s is
one whose artifacts you chose not to bring in Step 3 — rebuild it from the dashboard.

## Afterwards

The old machine still holds a copy of the vault and the master key. Whether that matters is a decision about the
credentials inside it rather than about docpreview.

## Moving into a container

The same list, plus one constraint of its own: **the data directory must be mounted at the same path inside and
outside the container.** The daemon asks the host's docker daemon to bind-mount each workspace into a build
container, so that path is resolved on the host. Mount the data elsewhere internally and every build gets an empty
workspace — which fails as a missing `package.json` and looks nothing like a mount problem.

See [Runbook — the container](./container.md).
