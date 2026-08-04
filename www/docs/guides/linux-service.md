---
id: linux-service
title: Install on a Linux VM
sidebar_position: 6
---

# Install on a Linux VM

From a freshly provisioned cloud VM to a preview URL on a pull request. Ten steps, most of them one
command.

This is the arrangement to reach for. It is one binary run by systemd, building in docker
containers on the same host, reaching the internet through a zrok tunnel that needs no inbound
port. There is [a container image](./container.md) as well, and it is the harder path — the daemon
has to talk to the host's docker socket, so the container is not isolation, it is packaging with
extra failure modes.

## Before you start

| | |
|---|---|
| A VM | 2 vCPU, **4 GB RAM**, 20 GB disk. Ubuntu 22.04 or 24.04, Debian 12, Amazon Linux 2023, or anything with systemd |
| Docker | Needed by the default build driver. Step 1 installs it, and the `.deb` and `.rpm` pull it in for you |
| Outbound HTTPS | To your source-control host, to the zrok service, and to the package registries a build downloads from |
| Inbound | **None.** No port is opened, no DNS record is needed, no certificate is issued |
| A source-control account | A GitHub App you can create, or a Bitbucket repository you can add a webhook to |

:::danger 4 GB, not 2

A Docusaurus build peaks in prerendering, and a 2 GB box kills it there. The failure is
`ENOMEM` — or the build being killed with no message at all — after two minutes of successful
`npm ci`, which reads like a broken repository rather than a small machine.

:::

## Step 1 — Install docker

Builds run in containers by default, which is what keeps a pull request author's build script off
the host filesystem.

```bash
curl -fsSL https://get.docker.com | sudo sh
sudo systemctl enable --now docker
docker run --rm hello-world
```

:::tip The package may have done this already

The `.deb` and `.rpm` in Step 2 declare docker as a recommended dependency, so `apt` and `dnf`
install it with them by default. On Amazon Linux 2023, `dnf install ./docpreview*.rpm` brings in
docker, containerd and runc unasked.

That is a *weak* dependency: a minimal image, or `--setopt=install_weak_deps=False`, skips it. Run
`docker run --rm hello-world` and only come back here if it fails.

:::

:::note You can skip docker entirely

Set `build.driver: local` in the config instead and builds run `npm` directly on the VM. That is
fine for repositories only you can push to, and it is not fine for anything where opening a pull
request is not a privilege — the build script arrives *in* the pull request. See
[Security](../reference/security.md).

:::

## Step 2 — Install docpreview

A package, if your distribution takes one. Take the version from the
[release page](https://github.com/openziti-test-kitchen/docpreview/releases) and substitute it
below — these examples use `0.2.0`.

```bash
# RHEL, Fedora, Amazon Linux
sudo dnf install -y \
  https://github.com/openziti-test-kitchen/docpreview/releases/download/v0.2.0/docpreview-0.2.0-1.x86_64.rpm
```

```bash
# Debian, Ubuntu
curl -fsSLO \
  https://github.com/openziti-test-kitchen/docpreview/releases/download/v0.2.0/docpreview_0.2.0_amd64.deb
sudo apt install -y ./docpreview_0.2.0_amd64.deb
```

`aarch64` and `arm64` builds of both are on the release page. Note the filenames: the rpm spells
the architecture `x86_64` and `aarch64`, the deb spells it `amd64` and `arm64`, and neither carries
the leading `v` that the tag does.

The package does everything Step 3 describes — the service account, the directories, the units —
and starts nothing. It prints the next three commands as it finishes.

**Upgrading is the same command with a newer file.** There is no apt or dnf repository to add, so
nothing upgrades on its own. `/var/lib/docpreview` and `/etc/docpreview` survive an upgrade and
survive a removal, because they hold the vault, the database and the config naming the master key's
source. Restart the service afterwards to run the new binary:

```bash
sudo systemctl restart docpreview
```

### No package for your distribution

The installer script works anywhere with systemd. It downloads a release, **verifies it against the
`SHA256SUMS` published with it**, and refuses to install anything that does not match:

```bash
curl -fsSLO https://raw.githubusercontent.com/openziti-test-kitchen/docpreview/v0.2.0/install/install.sh
chmod +x install.sh
sudo ./install.sh --version v0.2.0
```

The installer comes from the repository at that tag, not from inside an archive — one file with one
source. It downloads the right archive for the machine, checks it, and reads the systemd units out
of it.

Releases carry `linux/amd64`, `linux/arm64`, `darwin/arm64` and `windows/amd64`.

### Or build it yourself

From the VM or from your laptop:

```bash
# on the VM, with Go installed
git clone https://github.com/openziti-test-kitchen/docpreview
cd docpreview
go build -o docpreview ./cmd/docpreview
```

```bash
# or cross-compiled from a laptop
GOOS=linux GOARCH=amd64 go build -o docpreview ./cmd/docpreview
scp docpreview install/*.service install/install.sh you@vm:
```

Whichever you used, `docpreview version` says exactly what you have — the tag, the commit, and
whether the tree it was built from was dirty:

```text
docpreview v0.2.0  a162fad0ffe3  2026-08-01T16:30:48-04:00  linux/amd64  go1.26.0
```

## Step 3 — Run the installer

Skip this entirely if you installed the `.deb` or `.rpm`, or if `--version` already did it.
Otherwise, pointing at the binary you built:

```bash
sudo ./install.sh --binary ./docpreview
```

However you got here, the result is the same host: the `docpreview` service account,
`/var/lib/docpreview` (0700, owned by it) and `/etc/docpreview` (0770, `root:docpreview`), the
binary at `/usr/local/bin/docpreview`, and three systemd units. Nothing is started, and no config,
key or credential is written — those are decisions, and the next steps take them one at a time.

`/etc/docpreview` is group-writable because the service account writes into it: Step 4 creates
`config.yml` there and Step 5 puts the master key beside it, both as `sudo -u docpreview`.

### The service account

Everything after this point runs as `docpreview`, and every CLI command below is `sudo -u
docpreview` for that reason: a file written as root in the data directory is a file the daemon
cannot read at three in the morning.

| | |
|---|---|
| Name | `docpreview`, a system account — no login, no password |
| Home | `/var/lib/docpreview`, the same as `data_dir` |
| Shell | `/usr/sbin/nologin` |
| Groups | `docker`, so builds can run containers |

The home directory is not decoration. `zrok` keeps its environment under the *user's* home by
default, so a service account without one produces an enrolment nobody can find — and the daemon
reporting "no zrok environment is enrolled here" while `zrok2 overview` in your own shell shows a
working one. Pointing zrok at the data directory, which Step 6 does, avoids the question entirely.

:::caution The docker group is root-equivalent

The service account is added to it, because that is how a build runs a container. Anyone who can
reach the docker socket can start a container that mounts `/`, so this account is effectively root
on this VM. That is the cost of the docker build driver, and it is the reason this VM should do
nothing else.

:::

:::note Why the uid matters

A build container runs as **root**, so everything it writes through the bind mount would be
root-owned on the host — and the daemon, running as `docpreview`, could not read its own build
output, copy it into `artifacts/`, or remove the workspace afterwards.

docpreview appends a `chown` to the build's own command for exactly this, using the uid and gid the
daemon is running as. It needs nothing from you, and it is the reason changing `User=` in the unit
file to an account that does not own `/var/lib/docpreview` produces a daemon that builds
successfully and then fails to publish.

:::

## Step 4 — Write the config

```bash
sudo -u docpreview /usr/local/bin/docpreview init -config /etc/docpreview/config.yml
```

One question — which exposer — then it writes commented YAML and prints everything it defaulted.
Answer `zrok2`.

Then set the data directory. `init` defaults it to `.docpreview` *inside* the account's home, so on
this layout it writes `data_dir: "/var/lib/docpreview/.docpreview"` — a working directory, one level
below the one the package created and the one the units and every command in this guide name:

```yaml
data_dir: "/var/lib/docpreview"
```

Left alone, nothing fails. The daemon builds and publishes out of the nested directory, and it is
the directory a backup, a migration and `shares list` all miss.

## Step 5 — Mint a master key

Without one the daemon boots **locked**: it starts, serves its dashboard, and can decrypt nothing
until a human unlocks it. That is the right default for a laptop and the wrong one for a VM you
expect to reboot unattended.

```bash
sudo -u docpreview /usr/local/bin/docpreview vault keygen -out /etc/docpreview/master.key
```

```yaml
vault:
  key_source: "file:/etc/docpreview/master.key"
```

:::danger Do not widen the key's permissions

`keygen` writes it `0600` and owned by `docpreview`, which is what the daemon requires. **A key file
readable by group or other is refused at startup**, so a `chmod 0640` to "let the group read it"
produces a daemon that cannot open its own vault — reported as `it must not be readable by group or
other`, which sounds like a bug in the check rather than a change somebody made on purpose.

The file decrypts every secret in the vault. One account reads it, and that account is the one
running the daemon.

:::

The key is outside `data_dir` on purpose, and the daemon refuses a key file inside it: that
directory holds the vault, so one directory read must not yield both halves.

:::tip Better than a file

`key_source: "exec:op read op://ops/docpreview/master-key"` runs a command and keeps the key out of
every file on the machine. Any secrets manager with a CLI works. The command runs as the service
account and must not prompt.

:::

## Step 6 — Sign up for zrok and enrol this host

**As the service account.** zrok keeps its environment in a directory, and `docpreview zrok use
project` puts it in `/var/lib/docpreview/zrok2` rather than in a home directory — which is what
makes the whole installation one directory to back up, and what stops an enrolment made by the
wrong user being invisible to the daemon.

```bash
D=/etc/docpreview/config.yml
sudo -u docpreview /usr/local/bin/docpreview zrok use project    -config $D
sudo -u docpreview /usr/local/bin/docpreview zrok invite you@example.com -config $D
# open the email, then:
sudo -u docpreview /usr/local/bin/docpreview zrok register '<the link from that email>' -config $D
```

`register` asks for a password on stdin. That is the password for the **zrok account**, not for
docpreview. It is not stored here, and it is how that account is recovered.

Already have a zrok account? `docpreview zrok enable -token-stdin -config $D`.

Check it:

```bash
sudo -u docpreview /usr/local/bin/docpreview zrok status -config $D
```

## Step 7 — Set the dashboard passwords

Two roles. Until the **viewer** password exists, nothing is gated.

```bash
sudo -u docpreview /usr/local/bin/docpreview console password -role admin  -config $D
sudo -u docpreview /usr/local/bin/docpreview console password -role viewer -config $D
```

## Step 8 — Check, then start

```bash
sudo -u docpreview /usr/local/bin/docpreview doctor -config $D
```

Read four lines of it:

```text
exposer: zrok2
key:     file /etc/docpreview/master.key
login:   required — either password reaches the dashboard
scm:     none — set local.enabled or github.app_id to receive webhooks
```

`scm: none` is expected at this point — Step 9 fixes it. Anything else wrong is cheaper to fix now
than after a failed build.

```bash
sudo systemctl enable --now docpreview
sudo systemctl enable --now docpreview-webhook
journalctl -u docpreview -f
```

The webhook tunnel prints the public URL it claimed. That is what goes into the App.

## Step 9 — Connect your source control

Follow [the GitHub App guide](./github-app.md), or add a Bitbucket webhook, pointing at the URL
from Step 8:

```text
https://docpreview.shares.zrok.io/webhook/github
```

Then check it end to end without pushing anything, using the secret from the vault — it is never
printed and never reaches your shell history:

```bash
sudo -u docpreview /usr/local/bin/docpreview webhook-check \
  -url https://docpreview.shares.zrok.io/webhook/github -config $D
```

## Step 10 — Reach the dashboard

It binds loopback, so from your laptop:

```bash
ssh -N -L 8471:127.0.0.1:8471 you@vm
```

and open `http://127.0.0.1:8471/`.

To reach it without the tunnel, publish it — **after Step 7**, since this puts the page that lists
every open documentation pull request on the internet:

```bash
sudo systemctl enable --now docpreview-dashboard
```

## Afterwards

| | |
|---|---|
| Logs | `journalctl -u docpreview -f`, and the same for the two tunnels |
| Restart | `sudo systemctl restart docpreview` — previews republish and come back at the same URLs |
| Back up | `/var/lib/docpreview` and `/etc/docpreview/master.key`. That is everything |
| Move it | [Move an installation](./move-an-installation.md) |

:::danger One daemon per zrok account

Startup deletes every share it recognises as its own. Two installations sharing one zrok account
delete each other's previews, each restart wiping the other. Give a second one its own account, or
at minimum its own [hostname prefix](../reference/cli.md) and its own enrolment.

:::

## When something is wrong

| | |
|---|---|
| The service will not start | `journalctl -u docpreview -n 50`. A bad config is named on the first line |
| `502` from the public URL | The tunnel is up and nothing is behind it. `systemctl status docpreview` |
| Builds fail with `ENOMEM` | The VM is too small. See the note at the top |
| Builds fail with a docker permission error | The service account is not in the `docker` group, or was added while the service was running — `systemctl restart docpreview` |
| Everything looks fine, nothing happens | [Troubleshooting](../troubleshooting.md) |
