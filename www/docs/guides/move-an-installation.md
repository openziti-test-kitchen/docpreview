---
id: move-an-installation
title: Move an installation
sidebar_position: 8
---

# Move an installation

Moving NetFoundry Preview to another machine, or into a container, means copying four things and editing one file.
There is no export command. A file copy already gives you everything an export would.

:::danger Stop the old instance before starting the new one

Once you copy the zrok environment, both machines hold the same one. At startup a daemon adopts the shares its
database claims and deletes every other share it published in that environment. Two daemons running at once
delete each other's newer previews and bind listeners to the same shares, and every log looks healthy.

A move is stop, copy, start. Never an overlap.

:::

## What travels

| What | Where it is | |
|---|---|---|
| The database | `<data_dir>/nfpreview.db` | **and its `-wal` file**, see Step 2 |
| The credential store | `<data_dir>/vault.age` | encrypted, useless without the key |
| The master key | wherever `vault.key_source` points | deliberately outside `data_dir` |
| The zrok environment | `~/.zrok2` **or** `<data_dir>/zrok2` | the identity that owns every reserved name, see Step 4 |
| A ziti identity, if you use that exposer | `<data_dir>/ziti/*.json` | inside `data_dir`, so it travels with it |

**The database carries more than you might expect.** Anything you set on the dashboard or with the CLI lives in
its settings table and overrides `config.yml`: the console passwords, the hostname prefix, the selected exposer,
which zrok environment directory this installation uses, and the ziti service and identity path. You don't
re-enter any of it, and the login on the new machine is the one you already use.

You edit one file instead of copying it as is: `config.yml`, which holds absolute paths such as `data_dir` and
`vault.key_source`.

Three directories stay behind on purpose. `workspaces/` holds one clone per commit and the next build recreates
it. `logs/` is build history that nothing needs to run. `artifacts/` is the built sites, covered in Step 3.

## Step 1: Stop everything on the old machine

Stop all three processes: the daemon and both shares.

```powershell
Get-Process nfpreview | Stop-Process
```

```bash
pkill nfpreview
```

Make sure nothing answers before you continue. A daemon still running is the one mistake in this guide that
destroys something:

```powershell
curl.exe -s -o NUL -w "%{http_code}" http://127.0.0.1:8471/healthz
```

You want `000` or a connection error. `200` means something is still up.

## Step 2: Take the database as one consistent file

The daemon opens sqlite in WAL mode. A write is appended to `nfpreview.db-wal` and folded into `nfpreview.db`
later, at a checkpoint. That keeps the reaper's reads from blocking a worker's writes.

:::warning `nfpreview.db` on its own is not the current state

It holds the state as of the last checkpoint. Everything since then is in the `-wal` file, which is often larger
than the database. On a live instance, a 77 KB database beside a 519 KB WAL is normal. Copy the `.db` alone and the
new instance starts without some of its projects, previews and settings, and reports no error, because what you
copied is a valid older database.

:::

`VACUUM INTO` writes a single file with the WAL already folded in:

```powershell
sqlite3 "$data\nfpreview.db" "VACUUM INTO '$out\nfpreview.db'"
```

Copying all three files together also works, as long as the daemon is stopped:

```powershell
Copy-Item "$data\nfpreview.db*" -Destination $out
```

Prefer `VACUUM INTO` when the destination is a container volume or another host. A single file can't arrive with a
stale WAL beside it.

## Step 3: Decide about artifacts

Artifacts are the built sites the previews serve. At startup the daemon drops every preview whose artifact
directory is missing (`dropping preview with missing artifacts` in the log) and deletes its share. Those previews
disappear from the dashboard until their pull request is pushed to or rebuilt.

Copy them if the preview URLs have to keep working through the move:

```powershell
Copy-Item -Recurse "$data\artifacts" "$out\artifacts"
```

Skip them if a gap is fine. They are usually the bulk of the data directory, which tends to decide it.

:::warning Keep the same `data_dir` path

Each preview row stores its artifact directory as an absolute path under the old `data_dir`. If the new machine
uses a different `data_dir` path, every preview is dropped at startup even when you copied `artifacts/`. The stored
ziti identity path goes stale the same way. Keep the path the same if you can. If you can't, expect every preview
to rebuild, and set the ziti identity path again.

:::

## Step 4: Copy the credentials and the zrok identity

```powershell
Copy-Item "$data\vault.age" $out
Copy-Item "$env:LOCALAPPDATA\nfpreview\master.key" $out
```

The key is not inside the encrypted vault. Bring one without the other and the daemon starts, serves its setup page,
and can't decrypt anything it holds.

### Which zrok directory to copy

There are two possible ones, and which is in use decides how much work this step is:

```powershell
nfpreview zrok status -config .nfpreview\config.yml
```

| In use | Copy | |
|---|---|---|
| `project` | nothing extra | It is `<data_dir>/zrok2` and came with `data_dir` |
| `system` | `~/.zrok2` → the new machine's `~/.zrok2` | It is outside `data_dir`, under the account the daemon runs as |

`~/.zrok2` is **per user**. On the new machine it must land in the home directory of the account that runs the
daemon, which is a service account's home, not yours. Get that wrong and the daemon reports
`no zrok environment is enrolled here` while `zrok2 overview` in your own shell shows a working environment. Both are
telling the truth.

:::tip Move to the project directory while you are here

Run `nfpreview zrok use project` on the new machine and move the contents of `~/.zrok2` into `<data_dir>/zrok2`.
The next move is then a straight copy of one directory. A container needs this anyway, because a home directory in
a container does not last.

:::

:::danger You can't re-create the zrok environment

Enabling a fresh environment on the new machine loses more than the preview URLs. The reserved names are objects
owned by the old environment and counted against the account's quota, and nothing on the new machine can reclaim
or delete them. See [Publish previews with zrok v2](./zrok2.md).

:::

## Step 5: Place the files and edit the config

On the new machine, put the files where they belong, then check the absolute paths in `config.yml`:

```yaml
data_dir: "/srv/nfpreview/data"

vault:
  key_source: "file:/etc/nfpreview/master.key"
```

`data_dir` and `key_source` are the usual two. Also check any other absolute path you set, such as `log_file` or
`exposer.ziti.identity_file`. Everything else in the file is portable.

## Step 6: Check before starting

```powershell
nfpreview doctor -config /srv/nfpreview/config.yml
```

`doctor` reads the config, the vault and the exposer's settings and reports what it can't reach. It takes a second
and answers the question a failed build would answer ten minutes later. Read these four lines:

```text
exposer: zrok2 (selected on the dashboard, not from the config file)
key:     file /etc/nfpreview/master.key
login:   required — either password reaches the dashboard
scm:     github (app 4420399), bitbucket (access_token)
```

If `exposer` names something other than the config file, the choice came from the database and travelled with it.
That is correct, and you don't need to edit the file to match.

Then start the daemon, and start the two shares once it answers. Until then the shares log errors.

**If the zrok environment is the project one, both shares need `-zrok-home`.** They read no config file, so they
can't work it out. Without the flag they look in `~/.zrok2`, which is a different account or nothing at all, and
they fail unless that account happens to hold a name of the same spelling:

```powershell
nfpreview webhook-only   -zrok-name nfpreview      -zrok-home /srv/nfpreview/data/zrok2
nfpreview dashboard-only -zrok-name nfpreview-dash -zrok-home /srv/nfpreview/data/zrok2
```

## Step 7: Confirm the previews came back

At startup the daemon lists the shares the environment already has, adopts each one the database claims, deletes
the rest, and creates shares only for previews it could not adopt. Watch it finish:

```powershell
.\demo\Wait-Nfpreview.ps1
```

Then open the dashboard. Every restored preview should be `ready` with a URL that answers. A preview you expected
and don't see is one whose artifacts did not come across. Push to its pull request or rebuild it.

The console passwords move with the database, so you log in on the new machine as before. If you did not bring the
database, set them again with `nfpreview console password -role admin` and `-role viewer`.

Preview URLs come back **unchanged**. Under zrok the name is reserved on the account and the URL comes from it, so
the adopted share is the one the comments already point at. Nothing is rewritten and no link breaks.

## Afterwards

The old machine still has a copy of the vault and the master key. Whether that matters depends on the credentials
inside, not on NetFoundry Preview.

**Consider changing the master key on the new machine.** After a move `vault.age` sits on two machines. If the key
is short, say a passphrase typed during a hurried first setup, someone could guess it for the copy you left behind
without the key file. With the daemon stopped:

```bash
nfpreview vault rekey -generate -yes
```

Every secret is kept, so you need none of the plaintexts. See [`vault rekey`](../reference/cli.md#vault-rekey) for
the order of operations and what the backup is for.

Make sure the old machine can't start again. It holds the same zrok environment, so an old daemon that comes back
adopts the shares its database still claims and deletes the rest, including previews the new instance created.
Uninstall or disable the service, and delete its copy of the zrok environment, or the whole old `data_dir`.

## Moving into a container

The same list applies, plus one rule of its own: **mount the data directory at the same path inside and outside the
container.** The daemon asks the host's docker daemon to bind-mount each workspace into a build container, so the
host resolves that path. Mount the data elsewhere internally and every build gets an empty workspace. It fails as a
missing `package.json`, which looks nothing like a mount problem.

See [Run in a container](./container.md).
