# August 2026: what this installation is, and what is frightening about it

Written on 2026-08-24, at the end of a stretch of work that ran from 29 July to 21 August and shipped v0.3.0,
moved the live installation onto a VM, and rebuilt most of the operator surfaces. It exists because the
knowledge below kept living in one person's memory, in a gitignored markdown file, or in a chat transcript,
and none of those three survive a change of directory.

Two audiences. Somebody picking this up cold, who needs to know where the sharp edges are before touching
anything. And us in six months, when the reason for a decision has evaporated and only the decision is left.

`docs/design/` says how the code is shaped and why. `www/` is what operators are promised. This is neither. This
is the list of things that can hurt, and the history of how we found out.

## The short version

The tool works. It has been building real previews for real pull requests on two platforms for weeks. What is
frightening is not the build pipeline, it is everything around it: one weak key protecting every credential, no
backups, no monitoring, a build host that runs other people's code, and an admin surface reachable only through
an SSH tunnel. None of that is a surprise to the people who built it. All of it would surprise somebody who
inherited it.

## The scary list

Ordered by what would hurt most if it happened tomorrow.

### 1. The vault master key is four characters, and it is the key to everything

The vault holds the GitHub App private key, six Bitbucket repository tokens, two webhook secrets, and three
per-project source-control credentials. The App private key alone mints installation tokens for every repository
the App is on.

The key protecting all of that, on the production host, is a four-character numeric passphrase chosen during a
hurried first setup.

On the host itself this buys nothing either way, and that is worth being precise about: anything that can read
`/var/lib/docpreview/vault.age` runs as the service account or as root, and both can read the key file sitting
in `/etc/docpreview` beside it. What protects the secrets there is file permissions, not the passphrase.

It matters wherever the vault travels without its key. An EBS snapshot. A backup. A disk image. The copy that a
migration leaves behind on the old machine. Four numeric characters is ten thousand candidates, and age's scrypt
work factor makes each guess cost milliseconds, so the whole space falls in well under a minute.

`docpreview vault rekey` exists now precisely so this can be fixed without needing every plaintext. It has not
been run against production. **Run it.**

Related and not yet done: the same four characters are a test fixture in three tracked test files, in a public
repository. That is not a disclosure on its own, since nothing in the tests says which installation uses it, but
the coincidence is needless. Change the fixture when you rotate the key.

### 2. `Vault.Save` is not safe for concurrent use on Windows

The package documents itself as safe for concurrent use. It is not, and this was found on 24 August by a test
written for something else.

`Save` writes a temp file and renames it over `vault.age`. Two concurrent `Save` calls both reach the rename,
and Windows refuses a rename while another handle is on the target:

```
rename ...\.vault-2970486952.tmp ...\vault.age: Access is denied.
```

Reproduced at four failures in ten runs with `-race`, and one in one without. On Linux the rename is atomic and
this does not happen, which is why production has never shown it.

The blast radius is bounded: a failed write, not a corrupt vault, because the rename is all or nothing. But a
Windows operator making two dashboard writes at once gets one unexplained failure, and the fix is a dedicated
write mutex held across the whole of `Save`. Not `v.mu`, which must not be held across file I/O.

### 3. There are no backups, and no restore has ever been rehearsed

`/var/lib/docpreview/docpreview.db` and `vault.age` are the entire installation. They sit on one EBS volume with
no snapshot schedule. Nobody has ever restored either one anywhere.

A restore that has never been performed is a plan, not a backup. The database carries every project row, every
preview, the console password hashes and the installation's name prefix. The vault carries the credentials. Lose
the volume and the fastest recovery is re-creating both by hand from memory, which means re-obtaining the
GitHub App private key and every Bitbucket token.

### 4. Nothing watches it

No alerting on the daemon being down, on repeated build failures, on the disk filling, or on zrok refusing state
changes. Every incident this month was noticed because a person looked at a preview URL and it did not answer.

### 5. The admin surface is only reachable over an SSH tunnel, and its gate is a boundary rather than authentication

Projects and credentials require two independent things: every listener must be loopback (`Available()`), and the
request itself must be loopback with no forwarding headers (`isLocalRequest`). The second exists because the
first has a hole a tunnel drives straight through: a zrok share in proxy mode connects from a local process, so
`RemoteAddr` is `127.0.0.1` for a request that came from the internet.

Two consequences to hold at once.

It works, and the forwarding-header test is what closes the tunnel hole. But it assumes nothing ever proxies to
the daemon while stripping forwarding headers, and that assumption is not verified anywhere. It is a boundary,
not a credential.

And since the move to the VM it means the admin half of the dashboard is only usable through
`ssh -N -L 8471:127.0.0.1:8471 docprev`. That is documented and awkward, and the awkwardness is itself a hazard:
awkward paths get worked around.

### 6. The build host runs other people's code, on purpose

`.docpreview.yml` arrives in the pull request, and `build.command` is honoured. Under the docker driver the blast
radius is a container. Under the local driver it is a shell command on the build host as the daemon's user.

This is the largest deliberate hole in the design. It is gated by `build.allow_local_driver`, it is documented at
length in `www/docs/reference/repo-config.md`, and it is why the Docker driver exists. It is not a bug. It is a
thing to know before enabling the local driver on a repository where opening a pull request is not a privilege.

Two things narrow it and neither closes it: forks are refused at the webhook, and a project row's command wins
over the branch's.

### 7. The service account is in the `docker` group, which is root-equivalent

The postinstall script says so out loud when it adds the account. Saying it does not make it safe: membership of
the `docker` group is equivalent to root on that host. The ways out are rootless Docker or a socket proxy
permitting only the calls a build needs, and neither is done.

### 8. A public GitHub App would let strangers reach the builder, and the gate for that is one day old

A GitHub App has **one** webhook secret for the whole App, set on its registration page, shared by every
installation. There is no per-installation secret, and no way for an installer to supply one. So a signature
proves the delivery came from GitHub, and says nothing about whether the operator wanted that repository.

Making the App installable across more than one organisation means making it public, at which point anyone who
finds it can install it and their signed deliveries reach this daemon.

`Daemon.DeliveryAllowed` (added 21 August) closes that: a verified delivery for a repository with no project row
is acknowledged with 202 and dropped. Read its doc comment before changing anything near it, particularly the
reasoning for gating the ingress rather than `handleBuild`, and the reason it fails closed on a lookup error
where `applyProject` fails open.

What it costs: a repository can no longer onboard itself by having the App installed and a `.docpreview.yml`
committed. Somebody has to add a project row. Designing the self-service version of that, which is what Vercel
does with a setup URL and a claim proved by a user access token, is the open work.

### 9. One daemon per exposer account, and nothing enforces it

`Reap` runs at startup and deletes every share the environment recognises as its own, on the documented
reasoning that anything it finds is an orphan from a previous process. That reasoning holds for exactly one
daemon per account.

Two daemons sharing one zrok account delete each other's live previews. Confirmed for zrok, where the filter is
the environment identity plus a `docpreview:` target prefix, and for Frontdoor, where the tag carries the
preview ID and nothing identifying the instance. Nothing detects or prevents it. The name prefix on the
Settings page separates the *names* two installations publish under, which is a different problem.

### 10. The build host is half the machine, and has already taken itself down

docprev is 2 cores and 3.7 GB. Vercel builds the same commit on 4 cores and 8 GB, and the Docusaurus build is
2.8 times slower here as a result.

Worse, it has been driven into the ground: `workers: 2` with `memory: 3g` on 3.7 GB produced a global OOM, load
average 43, and an unreachable SSH. It runs at `workers: 3` now because the per-container limit is what actually
bounds it. Nothing detects a container killed for memory, so it surfaces as a Docusaurus page failure rather
than as what it is.

### 11. Nothing bounds the package cache

The dependency cache is keyed on the repository, which is what makes a new pull request's first build warm. It
also means nothing deletes it: teardown must not, because every other pull request on that repository is using
it. Twenty-seven stale per-preview volumes holding 9.8 GB were found on the host in August, left over from the
previous keying.

There is a clear-cache control on the projects page and no age-based prune.

## Traps that have already cost an afternoon

These are not risks. They have all happened.

### The deploy path

The systemd units execute `/usr/local/bin/docpreview`. A copy also exists at `/usr/bin/docpreview`, and
`/usr/local/bin` comes first on `PATH`, so installing to `/usr/bin` succeeds, restarts the services, and leaves
`docpreview version` reporting the old build. That is indistinguishable from a deploy that never ran. Two rounds
of "is it deployed?" went to this.

The host is rpm-managed: `rpm -qf /usr/local/bin/docpreview` names the package. Upgrade with
`dnf install ./docpreview-<version>-1.x86_64.rpm` and the units stopped.

**As of 24 August the host runs `v0.3.0-ui4`, installed by hand, which is ahead of the released `v0.3.0`.** Cut a
release before deploying anything else, or rpm's view and reality stay out of step.

### The daemon boots with a locked vault on purpose

`Open` returning `ErrLocked` is a normal state, not a startup failure. The page that unlocks the vault is served
by the daemon that would refuse to start. Anything constructed during wiring that reads the vault recreates a
bug that has now been fixed twice. Build such things after unlock, via the rearm callback.

### Publishing a name is destructive

The exposer withdraws whatever holds a name before taking it. A superseded build reaching `Publish` does not
merely waste work, it tears down the newer preview and serves older content in its place. That is why
"am I still current?" and "publish" are one atomic step under `commitLock`, and why cancellation alone cannot
protect it: zrok's `CreateShare` takes no context and cannot be interrupted.

### A merged pull request loses its preview and its comment, by design

Teardown on close removes the preview and retracts the comment. Watching PR 18's comment disappear twelve
seconds after merging looks exactly like data loss and is correct behaviour.

### zrok rate limits state changes, and names are quota-counted objects

A burst of publishes, which is what a restart or several workers finishing together produces, gets
`SERVER_TOO_MANY_REQUESTS`, arriving wrapped in an authentication failure so it reads as a credential problem.
There is now a process-wide pacer plus a longer backoff schedule for that specific refusal. Before it, a
rate-limited publish was a build that succeeded with no URL.

Names outlive the shares bound to them, on purpose, because that is what keeps a reviewer's URL working across
rebuilds. They are also what the account's quota counts.

### Windows bind mounts are pathologically slow for many small files

A package cache filling through a bind mount ran at 0.4 MB/s against 100+ MB/s natively. `npm ci` writing
`node_modules` through one took 5m46s against 14s to a volume. This is why the caches and `node_modules` are
docker volumes rather than host paths, and why `build.cache_dir` applies to the local driver only.

### A NUL byte was committed inside `dashboard.html`

Inside `const NEW = " new"`, where the space should have been. Consequences: grep reported the file as binary for
weeks, so every search needed `-a`, and some exact-match edits silently would not match. Fixed on 19 August. If
grep starts calling that file binary again, this is why.

### `dashboard.html` is one 8000-line file, and its initialisation order bites

Three separate outages this month were a `const` or `let` read before its declaration executed, which throws and
takes the rest of the script with it. The page renders an error where the content belongs and everything below
the throw simply never happens. `ro`, `admin` and `NEW` all did this within a week.

If the page renders one broken panel and nothing else works, look for a temporal dead zone before looking at
anything else. And note that the reason all three were caught before an operator saw them is
`tools/dashboardtest/`, which is the only reliable way to diagnose this page. Reading it is not.

## Things that are true and easy to get wrong

- **Two clones.** `D:\git\github\openziti-test-kitchen\docpreview` and any worktree under
  `D:\worktrees\github\...`. Check which one a command lands in before running it.
- **`go build` needs `-o build.claude/` and an explicit package list.** `demo/node_modules` carries Go files from
  other modules, so `./...` fails outright. A running daemon also holds the exe open on Windows, which is the
  most common self-inflicted build failure here.
- **A claim of the form "X must not Y" gets a test.** This is the rule that has repeatedly paid: the data race,
  the same-second backup overwrite, the empty-key refusal and the fork refusal all have one.
- **Comments describe behaviour, not history.** That rule is why this document exists as a separate file rather
  than as paragraphs of archaeology inside the code.

## What is deliberately unfinished

- Self-service onboarding for a publicly installable App, which the project-row gate made necessary.
- A real credential on the admin surface, replacing the loopback-and-headers boundary.
- Nothing detects a running daemon during `vault rekey`, so `-yes` is the operator asserting it. An advisory
  lock held for the daemon's lifetime would make it real and would serialize two concurrent rekeys, which
  nothing does today.
- Audit of secret reads. `SecretsAdmin` logs writes, `Vault.Get` logs nothing, so a build that injected a
  credential leaves no record of having read one.
- Nothing expires. Vault entries carry no timestamp, so no rotation can be reported as overdue.
- Cancelling a build exists; skipping a single pull request without editing the repository does not.

## The history, briefly

Late July: the tool worked on a laptop. zrok shares, one GitHub App, a local vault.

Early August: Bitbucket support, per-project credentials, the console login, deb and rpm packages, v0.2.0.

Mid August: the installation moved to `docprev`, an EC2 instance, running three systemd units. That move
surfaced most of the operational hazards above, in the order they appear in this document. The dashboard stopped
rewriting itself once a second, the build cache became repository-scoped, and the PR comment shrank to one row.

Late August: `vault rekey`, one project's settings on its own page, the hostname prefix moved to Settings, and
the project-row gate that makes a public App possible. v0.3.0 released, and the host then drifted ahead of it by
hand.

The pattern worth naming: every operational hazard in the first section was found by running the thing in
production, not by reading the code. The build pipeline has been reliable. Everything around it was discovered
the hard way.
