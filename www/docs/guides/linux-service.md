---
id: linux-service
title: Install on a Linux VM
sidebar_position: 6
---

# Install on a Linux VM

This page takes a freshly provisioned cloud VM to a preview URL on a pull request. Ten steps, most of them one
command.

Start here. It is one binary run by systemd, building in docker containers on the same host, and reaching the
internet through a zrok tunnel that needs no inbound port. There is [a container image](./container.md) too, but
it is the harder path. The daemon has to talk to the host's docker socket, so the container gives you packaging
and extra failure modes, not isolation.

## Before you start

| | |
|---|---|
| A VM | 2 vCPU, **4 GB RAM**, 20 GB disk. Ubuntu 22.04 or 24.04, Debian 12, Amazon Linux 2023, or anything with systemd |
| Swap | **A few GB.** A large docs build peaks above 3 GB, and swap turns that peak into a slower build instead of a killed one. See [sizing the build](#sizing-the-build-for-the-box) |
| git | Required by every build, under both drivers. Step 1 installs it |
| Docker | Needed by the default build driver. Step 1 installs it, and the `.deb` and `.rpm` pull it in for you |
| Outbound HTTPS | To your source-control host, to the zrok service, and to the package registries a build downloads from |
| Inbound | **None.** No port is opened, no DNS record is needed, no certificate is issued |
| A source-control account | A GitHub App you can create, or a Bitbucket repository you can add a webhook to |

:::danger 4 GB, not 2

A Docusaurus build peaks in prerendering, and a 2 GB box kills it there. You see `ENOMEM`, or the build dies with
no message, after two minutes of successful `npm ci`. It looks like a broken repository when the machine is just
too small.

:::

## Step 1: Install git and docker

**Every build needs git, whichever build driver you use.** A build starts by cloning the branch, and the clone
shells out to `git`. Without it every build fails in under a second with
`git init failed: exec: "git": executable file not found in $PATH`, and each failure is reported to the pull
request.

```bash
sudo dnf install -y git      # or: sudo apt install -y git
```

Builds run in containers by default, which keeps a pull request author's build script off the host filesystem.

```bash
curl -fsSL https://get.docker.com | sudo sh
sudo systemctl enable --now docker
docker run --rm hello-world
```

:::caution Installing docker is not the same as starting it

A package manager can install docker and leave it stopped, and Amazon Linux ships the socket unit without enabling
the service. The daemon starts fine, and then a build fails immediately on `docker create`. Run
`systemctl enable --now docker`, and trust `docker run --rm hello-world` as the proof.

:::

:::tip The package may have done this already

The `.deb` and `.rpm` in Step 2 recommend docker, so `apt` and `dnf` install it with them by default. On Amazon
Linux 2023, `dnf install ./nfpreview*.rpm` brings in docker, containerd and runc without being asked.

It is a *weak* dependency, so a minimal image or `--setopt=install_weak_deps=False` skips it. Run
`docker run --rm hello-world`, and come back here only if it fails.

:::

:::note You can skip docker entirely

Set `build.driver: local` and builds run `npm` directly on the VM. That is fine for repositories only you can push
to. It is not fine when anyone can open a pull request, because the build script arrives *in* the pull request.
See [Security](../reference/security.md).

:::

## Step 2: Install NetFoundry Preview

Use a package if your distribution takes one. Take the version from the
[release page](https://github.com/openziti-test-kitchen/docpreview/releases) and substitute it below. These
examples use `0.5.0`.

```bash
# RHEL, Fedora, Amazon Linux
sudo dnf install -y \
  https://github.com/openziti-test-kitchen/docpreview/releases/download/v0.5.0/nfpreview-0.5.0-1.x86_64.rpm
```

```bash
# Debian, Ubuntu
curl -fsSLO \
  https://github.com/openziti-test-kitchen/docpreview/releases/download/v0.5.0/nfpreview_0.5.0_amd64.deb
sudo apt install -y ./nfpreview_0.5.0_amd64.deb
```

`aarch64` and `arm64` builds of both are on the release page. Watch the filenames: the rpm spells the architecture
`x86_64` and `aarch64`, the deb spells it `amd64` and `arm64`, and neither has the leading `v` that the tag has.

The package does everything in Step 3 (the service account, the directories, the units) and starts nothing. It
prints the next three commands when it finishes.

**To upgrade, run the same command with a newer file.** There is no apt or dnf repository, so nothing upgrades on
its own. `/var/lib/nfpreview` and `/etc/nfpreview` survive an upgrade and a removal, because they hold the vault,
the database and the config that names the master key's source. Restart the service to run the new binary:

```bash
sudo systemctl restart nfpreview
```

### No package for your distribution

The installer script works anywhere with systemd. It downloads a release, **verifies it against the `SHA256SUMS`
published with it**, and refuses to install anything that does not match:

```bash
curl -fsSLO https://raw.githubusercontent.com/openziti-test-kitchen/docpreview/v0.2.0/install/install.sh
chmod +x install.sh
sudo ./install.sh --version v0.2.0
```

Fetch the installer from the repository at that tag, not from inside an archive. It downloads the right archive
for the machine, checks it, and reads the systemd units out of it.

Releases carry `linux/amd64`, `linux/arm64`, `darwin/arm64` and `windows/amd64`.

### Or build it yourself

On the VM or on your laptop:

```bash
# on the VM, with Go installed
git clone https://github.com/openziti-test-kitchen/docpreview
cd nfpreview
go build -o nfpreview ./cmd/nfpreview
```

```bash
# or cross-compiled from a laptop
GOOS=linux GOARCH=amd64 go build -o nfpreview ./cmd/nfpreview
scp nfpreview install/*.service install/install.sh you@vm:
```

However you got it, `nfpreview version` prints the tag and the commit:

```text
nfpreview v0.2.0  a162fad0ffe3  2026-08-01T16:30:48-04:00  linux/amd64  go1.26.0
```

## Step 3: Run the installer

Skip this if you installed the `.deb` or `.rpm`, or ran `install.sh --version`. Otherwise point it at the binary
you built:

```bash
sudo ./install.sh --binary ./nfpreview
```

Every route ends at the same host: the `nfpreview` service account, `/var/lib/nfpreview` (0700, owned by it),
`/etc/nfpreview` (0770, `root:nfpreview`), the binary at `/usr/local/bin/nfpreview`, and three systemd units.
Nothing is started, and no config, key or credential is written. The next steps make those choices one at a time.

`/etc/nfpreview` is group-writable because the service account writes into it. Step 4 creates `config.yml` there
and Step 5 puts the master key beside it, both as `sudo -u nfpreview`.

### The service account

Everything from here on runs as `nfpreview`, so every CLI command below starts with `sudo -u nfpreview`. A file
written as root in the data directory is a file the daemon can't read at three in the morning.

| | |
|---|---|
| Name | `nfpreview`, a system account with no login and no password |
| Home | `/var/lib/nfpreview`, the same as `data_dir` |
| Shell | `/usr/sbin/nologin` |
| Groups | `docker`, so builds can run containers |

The home directory matters. zrok keeps its environment under the *user's* home by default, so a service account
without one leaves an enrolment nobody can find. The daemon reports "no zrok environment is enrolled here" while
`zrok2 overview` in your own shell shows a working one. Step 6 points zrok at the data directory, which avoids the
problem.

:::caution The docker group is root-equivalent

The service account joins it so builds can run containers. Anyone who can reach the docker socket can start a
container that mounts `/`, so this account is effectively root on the VM. That is the cost of the docker build
driver, and it is why this VM should do nothing else.

:::

:::note Why the uid matters

A build container runs as **root**, so everything it writes through the bind mount is root-owned on the host. The
daemon, running as `nfpreview`, then can't read its own build output, copy it into `artifacts/`, or remove the
workspace.

To handle that, the daemon appends a `chown` to the build's command, using its own uid and gid. You don't have to
do anything. But if you change `User=` in the unit file to an account that does not own `/var/lib/nfpreview`, the
daemon builds fine and then fails to publish.

:::

## Step 4: Write the config

```bash
sudo -u nfpreview /usr/local/bin/nfpreview init -config /etc/nfpreview/config.yml
```

It asks one question, which exposer, then writes commented YAML and prints everything it defaulted. Answer
`zrok2`.

Then fix the data directory. `init` defaults it to `.nfpreview` *inside* the account's home, so here it writes
`data_dir: "/var/lib/nfpreview/.nfpreview"`. That is one level below the directory the package created, which the
units and every command in this guide use:

```yaml
data_dir: "/var/lib/nfpreview"
```

If you leave it, nothing fails. The daemon builds and publishes out of the nested directory, and a backup, a
migration and `shares list` all miss it.

### Sizing the build for the box

Four settings decide whether a build fits, and the defaults (`workers: 2`, `cpus: 4`, `memory: 6g`) suit a
developer machine. Copying a laptop's config onto a small VM is how this goes wrong.

```yaml
workers: 1

build:
  cpus: 2         # must not exceed the machine's core count
  memory: 3g      # per build container
```

**`cpus` above the core count is refused**, and the message only shows up in the build log:
`Range of CPUs is from 0.01 to 2.00, as there are only 2 CPUs available`. Set it to `nproc` or less.

**`workers` multiplies `memory`.** Two workers at `3g` is 6 GB of permitted allocation on a 4 GB machine. Nothing
checks the product, so both builds start and the machine runs out, not the container. That takes the host off the
network instead of failing a build: load above 40 on two cores, and ssh timing out during the banner exchange. On a
4 GB box, run one worker.

**A large docs build peaks above 3 GB**, and Node does not back off. V8 grows its heap until the cgroup refuses,
and docker kills the process at the limit with nothing in the build log. The evidence is in `dmesg`:

```text
oom_memcg=/system.slice/docker-….scope   Killed process (MainThread) anon-rss:1994752kB
```

`anon-rss` at the cap and `CONSTRAINT_MEMCG` mean the container hit its own `memory` limit. `global_oom` means the
machine ran out, so `workers × memory` is too high.

**Swap makes the peak survivable.** With none, a build that wants 200 MB more than the cap dies. With a few GB,
docker gives the container an equal amount of swap on top of `memory`, the overshoot spills to disk, and the build
finishes slower. On a 4 GB VM, one large site went from being killed twice to finishing in 3m40s using 349 MB of
swap:

```bash
sudo dd if=/dev/zero of=/swapfile bs=1M count=4096 status=none
sudo chmod 600 /swapfile
sudo mkswap /swapfile
sudo swapon /swapfile
echo "/swapfile none swap sw 0 0" | sudo tee -a /etc/fstab
```

Don't skip the `fstab` line. Without it the swap is gone after a reboot, and the build that worked yesterday is
killed today.

Swap buys headroom with build time. It is the cheap fix, not the fast one. When builds start queuing behind each
other, you have a throughput problem, and you need a bigger machine.

## Step 5: Mint a master key

Without a key the daemon boots **locked**. It starts and serves its dashboard, but it can decrypt nothing until a
person unlocks it. That suits a laptop. It does not suit a VM you expect to reboot unattended.

```bash
sudo -u nfpreview /usr/local/bin/nfpreview vault keygen -out /etc/nfpreview/master.key
```

```yaml
vault:
  key_source: "file:/etc/nfpreview/master.key"
```

:::danger Do not widen the key's permissions

`keygen` writes the key `0600`, owned by `nfpreview`, and the daemon requires exactly that. **A key file readable
by group or other is refused at startup.** A `chmod 0640` to "let the group read it" leaves a daemon that can't
open its own vault, and the error, `it must not be readable by group or other`, reads like a bug in the check.

The file decrypts every secret in the vault. One account reads it: the one running the daemon.

:::

The key lives outside `data_dir` on purpose, and the daemon refuses a key file inside it. That directory holds the
vault, and reading one directory must not give anyone both halves.

:::tip Better than a file

`key_source: "exec:op read op://ops/nfpreview/master-key"` runs a command and keeps the key out of every file on
the machine. Any secrets manager with a CLI works. The command runs as the service account and must not prompt.

:::

## Step 6: Sign up for zrok and enrol this host

**Run these as the service account.** `nfpreview zrok use project` keeps the zrok environment in
`/var/lib/nfpreview/zrok2` instead of a home directory. The whole installation is then one directory to back up,
and an enrolment made by the wrong user can't hide from the daemon.

```bash
D=/etc/nfpreview/config.yml
sudo -u nfpreview /usr/local/bin/nfpreview zrok use project    -config $D
sudo -u nfpreview /usr/local/bin/nfpreview zrok invite you@example.com -config $D
# open the email, then:
sudo -u nfpreview /usr/local/bin/nfpreview zrok register '<the link from that email>' -config $D
```

`register` asks for a password on stdin. It is the password for the **zrok account**, not for NetFoundry Preview.
It is not stored here, and you need it to recover that account.

Already have a zrok account? `nfpreview zrok enable -token-stdin -config $D`.

Check it:

```bash
sudo -u nfpreview /usr/local/bin/nfpreview zrok status -config $D
```

## Step 7: Set the dashboard passwords

There are two roles. Every dashboard request needs one of these passwords; with none set, the dashboard is closed.

```bash
sudo -u nfpreview /usr/local/bin/nfpreview console password -role admin  -config $D
sudo -u nfpreview /usr/local/bin/nfpreview console password -role viewer -config $D
```

## Step 8: Check, then start

```bash
sudo -u nfpreview /usr/local/bin/nfpreview doctor -config $D
```

Read these four lines:

```text
exposer: zrok2
key:     file /etc/nfpreview/master.key
login:   required — either password reaches the dashboard
scm:     none — set local.enabled or github.app_id to receive webhooks
```

`scm: none` is expected here, and Step 9 fixes it. Anything else wrong is cheaper to fix now than after a failed
build.

```bash
sudo systemctl enable --now nfpreview
sudo systemctl enable --now nfpreview-webhook
journalctl -u nfpreview-webhook
```

The webhook tunnel logs the public URL it claimed, on the line `this is the webhook URL to configure`. That URL
goes into the App or the Bitbucket webhook.

## Step 9: Connect your source control

Follow [the GitHub App guide](./github-app.md) or [the Bitbucket guide](./bitbucket.md), using the URL from Step
8. With the unit's default `-zrok-name nfpreview` it looks like this:

```text
https://nfpreview.shares.zrok.io/webhook/github
```

Then check it end to end without pushing anything. The command signs a test delivery with the secret from the
vault, which is never printed and never reaches your shell history:

```bash
sudo -u nfpreview /usr/local/bin/nfpreview webhook-check \
  -url https://nfpreview.shares.zrok.io/webhook/github -config $D
```

## Step 10: Reach the dashboard

The dashboard binds loopback, so from your laptop:

```bash
ssh -N -L 8471:127.0.0.1:8471 you@vm
```

and open `http://127.0.0.1:8471/`.

To reach it without the ssh tunnel, publish it. Do this **after Step 7**, because it puts the page listing every
open pull request on the internet:

```bash
sudo systemctl enable --now nfpreview-dashboard
```

## Afterwards

| | |
|---|---|
| Logs | `journalctl -u nfpreview -f`, and `-u nfpreview-webhook` and `-u nfpreview-dashboard` for the two tunnels |
| Restart | `sudo systemctl restart nfpreview`. Previews adopt their existing shares and come back at the same URLs |
| Back up | `/var/lib/nfpreview` and `/etc/nfpreview/master.key`. That is everything |
| Move it | [Move an installation](./move-an-installation.md) |
| Installed as `docpreview` (v0.4.9 or earlier) | [Upgrade from docpreview](./migrate-from-docpreview.md) |

:::danger One daemon per zrok environment

At startup the daemon deletes the shares in its zrok environment that its database no longer claims, and adopts
the rest. Two installations on one enrolment each delete the other's previews on every restart. Give a second
installation its own account, or at least its own enrolment and its own [hostname prefix](../reference/cli.md).
The prefix does not reach the tunnels, so also change `-zrok-name` in `nfpreview-webhook.service` and
`nfpreview-dashboard.service`.

:::

## When something is wrong

| | |
|---|---|
| The service will not start | `journalctl -u nfpreview -n 50`. A bad config is named on the first line |
| `502` from the public URL | The tunnel is up and nothing is behind it. `systemctl status nfpreview` |
| Builds fail with `ENOMEM` | The VM is too small. See the note at the top |
| Builds fail with a docker permission error | The service account is not in the `docker` group, or was added while the service was running. `systemctl restart nfpreview` |
| Everything looks fine, nothing happens | [Troubleshooting](../troubleshooting.md) |
