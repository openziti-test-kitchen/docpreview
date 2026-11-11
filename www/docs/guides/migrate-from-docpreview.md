---
id: migrate-from-docpreview
title: Upgrade from docpreview
sidebar_position: 9
---

# Upgrade from docpreview

Releases up to v0.4.9 were called `docpreview`. v0.5.0 renames the binary, the package, the units, the service
account, both directories, the database file, the environment variables and the repository file to `nfpreview`, and
reads none of the old names. A package upgrade alone does not carry an installation across: installing the
`nfpreview` package beside `docpreview` gives you a second, empty installation.

This guide moves a Linux host installed from the `docpreview` rpm or deb in place, keeps its previews, credentials,
settings and public URLs, and can be undone. `install/migrate-from-docpreview.sh` does every step on the host, and
`tools/migrate-docprev.sh` runs it from a workstation over ssh.

:::danger Stop the old daemon before the new one starts

Both use the same zrok environment. A daemon adopts the shares its database claims and deletes the others it
recognises, so two running at once delete each other's previews. The script stops `docpreview` first and never
runs both.

:::

## What changes, and what the script does about it

| Old | New | Handled by |
|---|---|---|
| `/usr/local/bin/docpreview`, package `docpreview` | `/usr/local/bin/nfpreview`, package `nfpreview` | installs `nfpreview`; leaves `docpreview` installed and stopped until `finish` |
| units `docpreview`, `-webhook`, `-dashboard` | `nfpreview`, `-webhook`, `-dashboard` | stops the old, starts the new |
| service account `docpreview` | `nfpreview` | renamed in place with `usermod -l`, so the uid and every file's owner stay the same |
| `/var/lib/docpreview` | `/var/lib/nfpreview` | moved |
| `/etc/docpreview` | `/etc/nfpreview` | moved, master key included |
| `docpreview.db` | `nfpreview.db` | WAL checkpointed, then renamed |
| each preview's stored `artifact_dir` | under the new data directory | rewritten in the database. Without this the new daemon finds no artifacts at startup and deletes every preview row |
| `docpreview-share-gate.json`, `docpreview-shares.jsonl` | `nfpreview-…` | renamed, so the zrok create budget carries over |
| `data_dir`, `log_file`, `key_source` in `config.yml` | new paths | rewritten. A copy is kept as `config.yml.pre-nfpreview`. Nothing else in the file is touched |
| docker volumes `docpreview-cache-*` | `nfpreview-cache-*` | copied by `stage`, so the first builds are not cold |
| zrok share tag `docpreview:<id>` | `nfpreview:<id>` | see [The old shares](#the-old-shares) |
| webhook share `-zrok-name docpreview` | `nfpreview` in the shipped unit | a drop-in keeps `docpreview`, so no webhook URL changes. See [Public names](#public-names) |
| dashboard share `docpreview-dash` | `nfpreview-dash` | a drop-in keeps `docpreview-dash`, which `dashboard_url` and every existing comment link to |

The database moves whole, so console passwords, the hostname prefix, the selected exposer, the zrok scope and the
projects need nothing re-entered.

These change and the script can't do anything about them, because they live on other systems:

- **The GitHub check is named `nfpreview`.** A branch-protection rule or ruleset that requires `docpreview` blocks
  every pull request until it requires `nfpreview` instead.
- **The Bitbucket build status key is `nfpreview`.** A merge check that names the old key needs the same change.
- **The comment marker is `nfpreview`.** Unless your build recognises the old marker, each open pull request gets a
  second comment at its next build. The old comment stays and goes stale.
- **A repository's `.docpreview.yml` is no longer read.** Rename it to `.nfpreview.yml`. Builds that read
  `DOCPREVIEW_BASE_URL`, `_PR`, `_URL`, `_BRANCH`, `_COMMIT` or `_REPO` need `NFPREVIEW_…` instead. Settings kept
  on the project's dashboard row are in the database and need nothing.
- **The console session cookie is renamed.** Everybody signs in to the dashboard once more.

## The old shares

Every preview's zrok share is tagged `docpreview:<preview>`, and each one holds the reserved name its preview is
published under. Set `OLD_SHARES` to say what happens to them:

| `OLD_SHARES` | When | Effect |
|---|---|---|
| `adopt` | the `nfpreview` build recognises the `docpreview:` tag | nothing is deleted. The new daemon adopts each share at startup, so previews stay up through the cutover and no share is created |
| `delete` | it does not | the script deletes them once the old daemon is stopped. The new daemon creates one share per preview at startup, through the share gate's 15 per 10 minutes, and each preview URL is down until its share exists |

With a build that does not recognise the tag, `adopt` is wrong: every restored preview fails to publish because an
old share holds its name. There is no default. `cutover` refuses to start until you choose.

## Public names

The shipped units publish the webhook as `nfpreview` and the dashboard as `nfpreview-dash`. Changing those names
means changing the webhook URL in the GitHub App and on every Bitbucket repository, creating two zrok names, and
breaking the dashboard link in every existing comment. The script writes drop-ins that keep the old names instead:

```ini title="/etc/systemd/system/nfpreview-webhook.service.d/10-keep-name.conf"
[Service]
ExecStart=
ExecStart=/usr/local/bin/nfpreview webhook-only -zrok-name docpreview -zrok-home /var/lib/nfpreview/zrok2 -path /webhook/github -path /webhook/bitbucket
```

The webhook process adopts the share that already holds the name, so it creates nothing. To use the new names
instead, set `WEBHOOK_NAME=` and `DASH_NAME=` to empty, create the two names first, and make the outside changes
before the cutover.

## Before the window

1. **Build the release** with `./release.sh v0.5.0`. The rpm is in `dist/`.
2. **Keep the v0.4.9 rpm on the host.** Rollback reinstalls it if `docpreview` has already been removed. The script
   looks for `/var/tmp/docpreview-0.4.9-1.x86_64.rpm`, or set `OLD_RPM`. `/tmp` does not survive a reboot, so use
   `/var/tmp`.
3. **Upload and look.** Both are safe to run against a live installation:

   ```bash
   tools/migrate-docprev.sh upload dist
   tools/migrate-docprev.sh check
   ```

   `check` changes nothing. It prints the packages, units, account, directories, the paths in the config, the
   database's previews and settings, the shares tagged with each name, running builds and cache volumes. Make sure
   the preview count and the share count agree, and that no build is running.

4. **Copy the build caches** while `docpreview` keeps serving:

   ```bash
   tools/migrate-docprev.sh stage
   ```

   It copies each `docpreview-cache-*` volume into an `nfpreview-cache-*` one with a throwaway `alpine:3`
   container. On a host with 10 GB of caches, expect a few minutes. `COPY_CACHES=0` skips it, and the first build
   of each preview then installs from scratch.

5. **Make the outside changes that must come first.** A required `docpreview` check has to be changed to
   `nfpreview` at the moment of cutover or pull requests stop being mergeable. Changes that can wait, such as
   renaming `.docpreview.yml`, can be done after.

## The cutover

```bash
OLD_SHARES=adopt tools/migrate-docprev.sh cutover
```

In order, and each step skips what is already done, so a fixed failure is resumed by running it again:

1. Checks the rpm against `SHA256SUMS`.
2. Stops the three `docpreview` units, waits until nothing runs as `docpreview`, and stops any build container still
   writing to the data directory. Its job is still queued in the database and is built again.
3. Backs up `config.yml` and a consistent copy of the database to `/var/tmp/nfpreview-migration/backup-<time>/`.
4. Deletes or keeps the `docpreview:` shares, per `OLD_SHARES`.
5. Renames the account, moves both directories, renames the files inside, rewrites the database and the config.
6. Installs the `nfpreview` package and writes the drop-ins.
7. Runs `nfpreview doctor`, and stops there if it fails.
8. Starts `nfpreview`, waits for `/healthz`, starts both shares, and checks all three are active.

Webhooks go unanswered from step 2 to step 8. GitHub does not retry a failed delivery, so note the times the
script prints and redeliver anything in that window from the App's **Advanced → Recent deliveries**. On Bitbucket,
push again or use **Rebuild** on the dashboard.

The units are left disabled at boot if they were before. `ENABLE_AT_BOOT=1` enables the new ones.

## Afterwards

```bash
tools/migrate-docprev.sh verify
```

It prints the units, `/healthz`, `nfpreview shares list` (every share against the database), and the HTTP status of
the webhook, the dashboard and every ready preview URL. A webhook path answers a GET with a 4xx, because it wants a
signed POST. Every preview should answer 200. Startup restores previews in the background, so a preview can take a
minute or two to answer.

Then push to one pull request on each platform and confirm the build, the comment and the check or status.

When you are satisfied, remove the old package and the old caches:

```bash
tools/migrate-docprev.sh finish
```

`finish` keeps `/var/tmp/nfpreview-migration` and the v0.4.9 rpm, so a rollback still works after it.

## Rollback

```bash
tools/migrate-docprev.sh rollback
```

1. Stops and disables the `nfpreview` units.
2. Deletes every share tagged `nfpreview:`. `docpreview` can't adopt them, and each holds a name it needs.
   `docpreview:` shares left by `adopt` are kept, and `docpreview` adopts them again.
3. Removes the drop-ins and the `nfpreview` package.
4. Renames the database, gate files and config paths back, rewrites `artifact_dir` back, moves both directories
   back, and renames the account back.
5. Reinstalls the v0.4.9 rpm if `finish` removed it.
6. Starts `docpreview`, waits for `/healthz`, starts both shares, and checks all three are active.

Previews built after the cutover come back too, because their artifacts are under the same directory. Pull requests
commented on by `nfpreview` keep that comment beside the old one, and their `nfpreview` checks stay on the commits
they ran against.

Each step skips what is already undone, so rollback also recovers from a cutover that stopped part way.
