# Where the GitHub App work stands

A working note, not a design document. **The smoke test has now passed**, so the only reason this file is still
here is that it records the App's identity and the two-configuration arrangement, which nothing else does, and that
five other documents cite it by section. Fold those two facts into a runbook and delete it; the plan for that is
step 9 of [13-github-testing.md](13-github-testing.md).

## The goal

docpreview is a GitHub App and has never run against GitHub. Everything proven so far went through the local
git simulator. This exercise closes that: create an App, install it on one repository, open a pull request,
and watch the comment appear and update.

That single run would be the first exercise of the App auth path (JWT → installation token), the comment
upsert and its marker, the revision counter, supersede under real webhook timing, `ChangedFiles` against the
API, the fork refusal, and — once the exposer moves to zrok — share creation and reaping.

## Done

**The App exists.** `openziti-test-kitchen`, owned by the personal account with "Any account" install scope.

| | |
|---|---|
| App ID | `4420399` |
| Permissions | Contents read, Pull requests read+write, Checks read+write, Metadata read |
| Events | Pull request |
| Webhook URL | `https://docpreview.shares.zrok.io/webhook/github`, served by `docpreview webhook-only` |

**A dedicated config**, deliberately separate from the demo: `.docpreview/config.yml`, data in
`.docpreview/data/`, listening on `127.0.0.1:8471`. The demo keeps `:8493`.

The separation is not tidiness. `demo/config.yml` is unlocked by a passphrase hardcoded in a committed script,
and it sets `local.enabled: true` — which serves `/webhook/local`, an unauthenticated build trigger. Neither is
acceptable behind the public tunnel this work needs.

**Credentials are in the vault** at `.docpreview/data/vault.age`, entered through the new setup page:
`github.webhook_secret` (generated in the UI, pasted into the App) and `github.private_key`.

## The boot-order blocker, fixed

With `app_id` set, the daemon built a GitHub client during wiring, which reads the private key from the
vault — so it refused to start while the vault was locked. The setup page that unlocks it lives inside the
daemon that would not start.

That was the second instance of the same shape. The first was `serve` failing outright when no source control
was configured, which made the page that configures it unreachable; that check is now a warning.

`scm.Client` is now swappable, the way `SetBuildSecrets` made the builder swappable: `Daemon.SetClient` and
`Ingress.SetClient`, each under its own mutex, each holding its own copy of the map rather than the aliased one
both used to share. `setup` treats `vault.ErrLocked` as "not unlocked yet" and warns instead of failing.
`rewireGitHub` builds the client and installs it on both, called from the `SecretsAdmin.rearm` callback.

`rearm` now carries the key that changed, empty on unlock. `rewireGitHub` rebuilds on unlock and on either
GitHub credential moving, and ignores everything else — both credentials are read at construction, so a
rotated webhook secret left in an old client would reject every delivery, while rebuilding for an unrelated
build secret would discard a cached installation token for nothing.

Until a client exists, `/webhook/github` answers 501, which is the truth: nothing could be verified.

Verified: with the vault locked and `app_id` set, `serve` starts, `/healthz` is 200, `/api/secrets` reports
`locked: true`, and `/webhook/github` is 501. `cmd/docpreview/rewire_test.go` covers the sequence — locked,
one credential, both, unrelated key, no app ID.

## Then — all four done

1. ~~**A tunnel** so GitHub can reach `127.0.0.1:8471/webhook/github`.~~ zrok, through `docpreview webhook-only`,
   which creates its own named share over the SDK rather than shelling out to the rc8 CLI — that CLI cannot bind a
   reserved name to a public share. The App's Webhook URL is `https://docpreview.shares.zrok.io/webhook/github`.
2. ~~**Install the App** on the test repository.~~
3. ~~**Open a pull request**, then push three commits inside a minute.~~
4. ~~**Switch `exposer.kind` to `zrok2`.**~~

What the run found, and it was all in the exposer rather than in the App path: a zrok name has to be registered
before a share can bind it, and a frontend endpoint comes back as a bare hostname rather than a URL. Neither was
reachable from the local git simulator, where nothing ever creates a share and no comment is ever rendered for a
browser. Both are fixed and written up in [16-exposer-zrok.md](16-exposer-zrok.md). Nothing else in the App path
changed as a result, which is the useful part of the answer.

The running arrangement — three processes, in order, and the two config traps that cost an afternoon each — is in
the root `CLAUDE.md`, which is where it belongs now that it is a thing somebody does rather than a thing somebody is
planning.

## What this exercise has cost so far

Four bugs, all found by using the page rather than by testing it:

- `.go` class collision greyed out and disabled the activity rail
- `serve` refused to start before setup, hiding the setup page
- "Create" did not write the vault, so a restart discarded the passphrase and looked like data loss
- the same boot-order problem again, one level deeper, with `app_id` set

The tests written alongside covered the API contract. None covered the sequence a person performs: open the
page, set a passphrase, restart, come back. `rewire_test.go` closes part of that — it asserts on the sequence
rather than the request shape — but nothing yet drives a real `serve` process across a restart.
