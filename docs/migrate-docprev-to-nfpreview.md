# Upgrading docprev from docpreview v0.4.9 to nfpreview v0.5.0

The specific version of [Upgrade from docpreview](../www/docs/guides/migrate-from-docpreview.md) for the production
host, with what is actually on it. The guide is the procedure. This is what it meets on `docprev`, and what has to be
decided first.

Delete this file once the cutover is done and `finish` has run.

## What is there, as surveyed on 10 November 2026

Read-only survey over ssh. Nothing was changed.

| | |
|---|---|
| Host | Amazon Linux 2023, `x86_64`, 100 GB disk, 72 GB free |
| Package | `docpreview-0.4.9-1.x86_64`, `rpm -V` clean, binary `v0.4.9 1acf08e` |
| Units | `docpreview`, `-webhook`, `-dashboard`: active since 5 November, **all three disabled at boot**. No drop-ins, no `Environment=` |
| docker | `docker.service` disabled, `docker.socket` enabled, so it starts on first use |
| Account | `docpreview`, uid 992, in group `docker` |
| `/etc/docpreview` | `config.yml`, `master.key`, and two leftover copies `config.yml.one-share` and `config.yml.pre-one-share` |
| `/var/lib/docpreview` | 8.5 GB: `docpreview.db` (+ WAL), `vault.age`, `daemon.log`, `artifacts/`, `logs/`, `workspaces/`, `zrok2/` |
| Config paths | `data_dir`, `log_file` under `/var/lib/docpreview`; `key_source: file:/etc/docpreview/master.key`; `dashboard_url: https://docpreview-dash.shares.zrok.io` |
| DB settings | `exposer.kind=zrok2` (overrides the config's), `exposer.prefix=a`, `exposer.zrok.scope=project`, both console passwords set. `exposer.ziti.*` hold a stale Windows path, unused under zrok2 |
| Projects | 7: four on GitHub, three on Bitbucket, every one with a dashboard row and `driver: docker` |
| Previews | 11 rows: 8 ready, 2 failed, 1 skipped. Every ready one's `artifact_dir` is under `/var/lib/docpreview/artifacts/` |
| Queue | empty |
| zrok shares | 8 tagged `docpreview:<preview>`, one per ready preview, plus `docpreview` (webhook) and `docpreview-dash` (dashboard), all in the daemon's own environment, `/var/lib/docpreview/zrok2` |
| Share gate | `docpreview-share-gate.json` and `docpreview-shares.jsonl` in `zrok2/`, last create 9 November |
| Build caches | 33 `docpreview-cache-*` volumes, 10.4 GB |
| Rollback rpm | `/var/tmp/docpreview-0.4.9-1.x86_64.rpm`, sha256 `b86fffef5dd58d3f37ba482c57ec68613320d547d5adae11977b79cc0aa5dc79` |

The daemon's zrok environment is the same enrolment as one on the workstation. A local daemon with a zrok exposer
reaps production's shares. With the `docpreview:` compatibility commit it reaps the old-tagged ones as well as the
new. Use `exposer: kind: local` for any local run.

### Repositories

- **`.docpreview.yml` and `.docpreview/`**: none on the default branch of `netfoundry/docusaurus-shared` or
  `openziti/ziti-doc` (full tree listing). This repository already has `.nfpreview.yml`. The private GitHub
  repository and the three Bitbucket ones could not be read from here. Every project has a dashboard row that sets
  its build, so a missed repository file changes nothing unless it sets something the row leaves blank.
- **`DOCPREVIEW_*` in builds**: no reference in production's retained workspaces or build logs. GitHub code search
  is not reliable enough on these repositories to rule it out. The only `docpreview` text in the logs is the
  daemon's own hints.
- **Required `docpreview` check**: none. `docusaurus-shared` and this repository have no protection on `main`.
  `ziti-doc` protects `main` but requires no status checks. None of the three has a ruleset. The private repository's
  protection and the Bitbucket merge checks could not be read.
- **Open pull requests with a preview**: three on `docusaurus-shared` (#150, #153, #165) and two on Bitbucket. The
  check run on #165 is `docpreview` from the `netfoundry-preview` App.

## Decisions before the window

1. **The version.** v0.5.0: the rename breaks every installation, so it is not a patch, and
   `docs/migrate-to-the-vm.md` already names `nfpreview-0.5.0-1.x86_64.rpm`. Tag it on `main` after this branch
   lands, then run `./release.sh v0.5.0`.
2. **The two compatibility commits.** Each is its own commit and can be dropped on its own.
   - *Comment marker*: `HasMarker` also matches `<!-- docpreview:<id> -->` and `[docpreview]: #<id>`. Kept, the
     first build of each open pull request edits its existing comment, which rewrites the marker. Dropped, each gets
     a second comment and the first goes stale. Nothing else depends on it.
   - *Share tag*: the zrok exposer also recognises `docpreview:` when it adopts, reaps and reclaims. Kept, run with
     `OLD_SHARES=adopt`: previews stay up through the cutover and nothing is created. Dropped, run with
     `OLD_SHARES=delete`: the script deletes the 8 shares and the new daemon creates 8, within the share gate's 15
     per 10 minutes.
3. **Public names.** Recommended: keep `docpreview` and `docpreview-dash` with the drop-ins, which is the script's
   default. The new names need the GitHub App's webhook URL, the webhook on each Bitbucket repository, two new zrok
   names and `dashboard_url` changed, and they break the dashboard link in every existing comment.
4. **Start at boot.** All three units are disabled today, so a reboot leaves production down. Recommended:
   `ENABLE_AT_BOOT=1`. That changes how the host behaves, so it is your call.
5. **Build caches.** Recommended: run `stage` before the window, so no build starts cold.
6. **The soak** before `finish`. Until `finish`, rollback needs nothing but the host.

Not part of this upgrade, and worth a separate decision: `master.key` is 4 bytes. `nfpreview vault rekey -generate
-yes` replaces it and keeps every secret. See [Move an installation](../www/docs/guides/move-an-installation.md#afterwards).

## Changes outside the host

With the recommended choices, **nothing outside the host has to change for the cutover.** The webhook URLs stay
the same, no required check names `docpreview`, and no known repository file needs renaming.

To confirm, and for the choices that do need them:

| Where | Change | Needed when |
|---|---|---|
| Bitbucket, each of the three repositories: **Repository settings → Merge checks** (and **Branch restrictions**) | if a check names the build key `docpreview`, change it to `nfpreview` | at cutover, if one exists |
| The private GitHub repository: **Settings → Branches** and **Rules** | if a rule requires the `docpreview` check, require `nfpreview` | at cutover, if one exists |
| The three Bitbucket repositories and the private GitHub one | `git mv .docpreview.yml .nfpreview.yml`, `git mv .docpreview .nfpreview`, and `DOCPREVIEW_` to `NFPREVIEW_` in build scripts and the Docusaurus config | any time, if they exist |
| GitHub App `netfoundry-preview`: **General → Webhook URL** | `https://nfpreview.shares.zrok.io/webhook/github` | only with the new public names |
| Each Bitbucket repository: **Repository settings → Webhooks** | URL to `https://nfpreview.shares.zrok.io/webhook/bitbucket` | only with the new public names |
| zrok | `zrok2 create name nfpreview` and `nfpreview-dash` from the daemon's environment | only with the new public names, before cutover |
| `/etc/nfpreview/config.yml` | `dashboard_url: "https://nfpreview-dash.shares.zrok.io"` | only with the new public names |
| The five open pull requests | delete the stale `docpreview` comment once the `nfpreview` one appears | only if the marker commit is dropped |

To look for repository files in a clone: `git ls-files | grep -i docpreview` and `git grep -n DOCPREVIEW_`.

## Expected downtime

Webhooks are unanswered from the moment `docpreview` stops until `nfpreview` answers `/healthz`. Preview URLs answer
502 from the same moment until each preview is adopted or recreated.

| | Webhooks | Preview URLs |
|---|---|---|
| `OLD_SHARES=adopt`, caches staged | 1 to 2 minutes | 2 to 4 minutes |
| `OLD_SHARES=delete`, caches staged | 2 to 4 minutes | 5 to 10 minutes |
| caches not staged | add a few minutes to both, for the copy | |
| rollback | 2 to 4 minutes | 5 to 10 minutes, because `docpreview` recreates every share `nfpreview` replaced |

GitHub does not retry a failed delivery. Redeliver anything in the window from the App's **Advanced → Recent
deliveries**.

## The run

```bash
./release.sh v0.5.0                              # from main, after the tag
tools/migrate-docprev.sh upload dist
tools/migrate-docprev.sh check                   # read-only
tools/migrate-docprev.sh stage                   # caches; docpreview keeps running
OLD_SHARES=adopt ENABLE_AT_BOOT=1 tools/migrate-docprev.sh cutover
tools/migrate-docprev.sh verify
# push to one pull request on GitHub and one on Bitbucket, and watch the build, comment and check
tools/migrate-docprev.sh finish                  # after the soak
```

If anything is wrong: `tools/migrate-docprev.sh rollback`.
