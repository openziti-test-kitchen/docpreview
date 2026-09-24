# Status, 25 August 2026

Where the work stands today, what is sitting uncommitted, and what happens next. Written to survive a change of
directory, because the last three days of work exist only as an unstaged diff in one worktree.

Three documents, three jobs. `TODO.md` is the backlog. `docs/history/2026-08-hazards-and-history.md` is what can
hurt you and how we found out. This is the shortest of the three: what is true right now.

## Where the code is

| | |
|---|---|
| Branch | `aug-revisions` |
| HEAD | `185160e`, `v0.3.0-3-g185160e`, committed 20 August |
| Worktree | `D:\worktrees\github\openziti-test-kitchen\docpreview\aug-revisions` |
| Deployed on `docprev` | **v0.3.0-ui4, installed by hand** |

The deployed version is not a tag and does not correspond to a commit anybody can name. **Cut a release before
deploying anything else**, so the next upgrade goes through the deb or rpm and the version on the box is one git
can identify.

The systemd units execute `/usr/local/bin/docpreview` (`install/docpreview.service`, `-dashboard`, `-webhook`). A
binary copied to `/usr/bin` instead is **silently ignored**: the unit keeps running the old one, nothing reports a
mismatch, and the deploy looks like it worked.

## Uncommitted work

Nothing from 21 August is committed. Three changes, which hang together because they were three parts of one
request: make everything durable tracked, get the passwords out of the repository, and close the hole that made a
public GitHub App unsafe.

### 1. A project row is now required before a webhook delivery builds

The change that matters. `Daemon.DeliveryAllowed` (`internal/daemon/daemon.go`) is called from `Ingress.webhook`
before `Handle`. A verified delivery for a repository with no project row is answered `202` and dropped.

The reasoning, so it is not re-litigated:

- **Sited at the ingress, not in `handleBuild`.** That function is the funnel for operator-initiated builds too —
  Rebuild, Build now, `LinkPR`, the scan that runs when a project is added — and those arrive from behind the admin
  gate, where a missing row is a reason to create one rather than a reason to refuse. Only the path a stranger can
  reach is gated.
- **Before the queued report**, which is the half that matters second. That report is a comment on somebody's pull
  request, so a check sited after it would let an uninvited repository make the App write into its own pull request.
- **Still `202`.** A distinguishable refusal would report the operator's project list one probe at a time.
- **The `local` platform is exempt.** Its repositories are directories the operator put under `local.repos_dir`
  themselves, so the choice a project row records has already been made on the filesystem. Without the exemption the
  demo builds nothing while looking correctly wired: `demo/New-Projects.ps1` creates git repositories, not rows.
- **A failed lookup refuses**, which is the opposite of how `applyProject` treats the same error and is right for
  the opposite reason. That one is choosing settings for a repository already known to be allowed; this one decides
  whether it is allowed at all, and a failed query is not evidence of a row.

Covered by `internal/daemon/deliverygate_test.go` — four cases, including a positive control, so the main test
cannot pass against a daemon that has stopped building anything at all. `TestIngressAcceptsVerifiedWebhookImmediately`
needed a project row added and is the only pre-existing test the change touched.

**This closes the only self-service onboarding path**, which is the next thing to design. Installing the App used to
be the whole of it and is now half: somebody with dashboard access has to add the row before the first build, so the
person installing gets a silent `202` and a preview that never appears. That is the correct failure and a bad first
experience. Filed in `TODO.md` with three candidate shapes.

### 2. The productionizing list folded into the repository

`.docpreview/PRODUCTIONIZING.md` was gitignored, so it followed neither a worktree nor a machine, and **three**
copies existed on disk rather than the two we thought. Its content is now distributed through `TODO.md` under the
existing headings — no new headings — and the rekey design notes were folded into `docs/design/05-secrets.md`, which
already held the whole design. The one genuine gap there was refusing an empty key, because an empty scrypt
passphrase is valid age.

The vault master key's value is **not** quoted anywhere in what was merged. It reads "its original four-character
key".

One item on that list was stale: "cancel a running or queued build" is shipped (`Daemon.CancelBuild`, the dashboard
button, `POST /api/builds/{preview}/cancel`) and was not added.

### 3. The credentials file is retired

`.docpreview/CREDENTIALS.md` held the dashboard admin and viewer passwords. All three copies are deleted and the
logins moved to a password manager. Nothing tracked references the file or either password.

**This leaves no trace in `git diff`**, because `.docpreview/` is gitignored. If you are reconstructing this change
from the diff alone, that is why the third item is invisible.

### The diff

```
 TODO.md                         | 97 +++++++++++++++++++++++++++++++++++---
 docs/design/05-secrets.md       |  6 +++
 internal/daemon/daemon.go       | 55 ++++++++++++++++++++++
 internal/daemon/ingress.go      | 15 ++++++
 internal/daemon/ingress_test.go |  9 ++++
 www/docs/guides/github-app.md   | 21 ++++++++-
 www/docs/reference/projects.md  | 19 ++++---
 www/docs/troubleshooting.md     |  1 +
```

Plus `internal/daemon/deliverygate_test.go`, untracked.

`www/docs/reference/projects.md` opened with "Projects are optional", which the gate made false. Rewritten rather
than amended.

### Verification

`go build` and `go vet` are clean. The daemon suite passes. Two things that look like failures and are not:

- `TestRekeyDoesNotRaceWithSave` fails intermittently under the full suite with a Windows rename error, and passes
  on its own with `-count=3`. It touches no code changed here. See hazard 2 in the history document.
- `gofmt` flags `internal/daemon/oauth.go`, which is pre-existing and unmodified.

## What happens next, in order

1. **Commit the above.** It is three days old and lives in one directory.
2. **Rekey the production vault.** `docpreview vault rekey` exists specifically so this needs no plaintext, the
   daemon has to be stopped, and it has never been run against production. This is hazard 1.
3. **Design the install experience**, then test it as an outsider against the `dovholuknf` account. The gate above
   is what makes this necessary and what makes the test meaningful: install the App as somebody with no dashboard
   access and find out what they see.
4. **Cut a release** before the next deploy, so `docprev` stops running a hand-installed binary.

## Waiting on a decision

- **The four-character key is a literal in three tracked test files**, 29 occurrences across
  `cmd/docpreview/rekey_test.go`, `cmd/docpreview/rekeypaths_test.go` and `internal/vault/rekey_test.go`, with a
  comment at `internal/vault/rekey_test.go:256` naming it as the production key. Nothing there says which
  installation uses it, so it is a coincidence rather than a disclosure — and a needless one. Change the fixture
  when the key rotates.
- **Where the two deployment facts at the top of this file belong permanently.** They are recorded here and in
  `docs/history/`, and neither is derivable from git. `www/docs/guides/linux-service.md` is the candidate.

## Related

- [TODO.md](TODO.md) — the backlog, and the reasoning behind each open item
- [docs/history/2026-08-hazards-and-history.md](docs/history/2026-08-hazards-and-history.md) — the eleven hazards,
  the traps that have already cost an afternoon, and how this installation got here
- [docs/design/05-secrets.md](docs/design/05-secrets.md) — the vault, the master key, and the rekey ordering
- [www/docs/reference/projects.md](www/docs/reference/projects.md) — what a project row now means
