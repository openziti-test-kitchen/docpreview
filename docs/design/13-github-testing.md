# Testing the GitHub integration

## The problem this document solves

Every line in `internal/scm/github` that touches the network has never run. The webhook parser is tested
(`internal/scm/github/webhook_test.go`) because it is a pure function of bytes; everything past it — the JWT, the
token exchange, the comment upsert, the changed-file walk, the check run, the error classifier — has only ever been
read. `docs/design/11-github-setup-state.md` treats one live pull request as the proof, and it would be: a single run
exercises the whole path at once. It is also a proof that needs a person with a browser, a tunnel, two GitHub
accounts and an hour, which means it happens rarely and proves nothing about the commit after it.

So the plan is inverted from how the work was sequenced. Almost everything on that list is testable without GitHub,
because the client reaches the network through exactly one seam — `Client.cfg.APIBase`,
`internal/scm/github/github.go:53` — and that seam is already a config field, `api_base`
(`internal/config/config.go:359`). Point it at an
`httptest.Server` and the real client makes real requests against a server we control and can assert on. What is left
for the live run is small and genuinely un-fakeable: that our beliefs about api.github.com's actual responses are
correct, and that a fork PR from a stranger's account behaves as the parser assumes.

Nothing here proposes recording and replaying real traffic. A cassette is a snapshot of one afternoon's API, and the
failures this integration will have are ordering failures — which request came second, which context was cancelled —
not payload-shape failures. A programmable fake can be asked "how many POSTs did you see"; a cassette cannot.

## What has and has not executed

| Path | Covered today | By |
|---|---|---|
| Signature verification, payload → `scm.Event` | yes | `internal/scm/github/webhook_test.go` |
| Fork refusal at the parser | yes | `webhook_test.go:119` |
| 401 with no diagnostic detail at the ingress | yes, against a stub client | `internal/daemon/ingress_test.go:114` |
| Client construction across a vault unlock | yes | `cmd/docpreview/rewire_test.go` |
| Supersede ownership, commit phase | yes, at unit level | `internal/daemon/supersede_test.go`, `commit_test.go` |
| App JWT, installation token, caching, refresh | **no** | — |
| Comment upsert, marker matching, Retract | **no** | — |
| Check run create/update | **no** | — |
| `ChangedFiles` pagination, empty diff, truncation | **no** | — |
| Rate-limit and error classification | **no** | — |
| `CloneURL`'s token escaping and host derivation | **no** | — |
| Supersede under webhook-shaped timing | **no** | — |

The daemon's tests substitute `fakeClient` (`internal/daemon/ingress_test.go:25`), which is the right stand-in for
testing the daemon and the wrong one for testing GitHub: it returns canned values and records calls, so every
assertion about it is an assertion about the test.

## 1. The fake GitHub API

One `httptest.Server` in `internal/scm/github/fake_test.go`, in-package, so a test can build a `*Client` by struct
literal the way `webhook_test.go:19` already does and skip the vault entirely.

```go
type fakeGitHub struct {
    srv   *httptest.Server
    pub   *rsa.PublicKey // to verify the App JWT
    appID int64

    mu       sync.Mutex
    calls    []call            // the ledger: method, path, query, body, auth kind
    comments []comment         // the PR's comment list, in order
    checks   []checkRun
    nextID   int64
    tokens   map[string]int64  // issued installation token -> installation id

    files   []string           // what /pulls/N/files serves, paged by the handler
    respond func(*call) *canned // per-test override: status, headers, body
    gate    func(*call) chan struct{} // per-test rendezvous, see §3
}
```

### What it must serve

Every endpoint the client calls, found by reading the client rather than by guessing:

| Method and path | Called from | Auth | Must answer |
|---|---|---|---|
| `POST /app/installations/{id}/access_tokens` | `auth.go:121` | App JWT | **201** and `{"token","expires_at"}` |
| `GET /app` | `github.go:81` | App JWT | 200 and `{"slug","name"}` |
| `GET /repos/{o}/{r}/pulls/{n}/files` | `github.go:167` | token | JSON array of `{filename, previous_filename}` |
| `GET /repos/{o}/{r}/issues/{n}/comments` | `github.go:254` | token | JSON array of `{id, body}` |
| `POST /repos/{o}/{r}/issues/{n}/comments` | `github.go:222` | token | `{"id"}` |
| `PATCH /repos/{o}/{r}/issues/comments/{id}` | `github.go:234` | token | any 2xx |
| `DELETE /repos/{o}/{r}/issues/comments/{id}` | `github.go:391` | token | any 2xx |
| `GET /repos/{o}/{r}/commits/{sha}/check-runs` | `github.go:320` | token | `{"check_runs":[{"id"}]}` |
| `POST /repos/{o}/{r}/check-runs` | `github.go:308` | token | any 2xx |
| `PATCH /repos/{o}/{r}/check-runs/{id}` | `github.go:315` | token | any 2xx |

The 201 is not a detail. `doOnce` accepts anything under 300 but the token exchange demands exactly
`http.StatusCreated` (`auth.go:136`), so a fake that answers 200 there fails every test with "requesting installation
token: 200 OK", which reads like a GitHub problem and is not one.

The `files`, comment-list and check-run handlers must honour `per_page` and `page` themselves rather than returning a
fixed array. Pagination termination in the client is `len(batch) < perPage` (`github.go:185`, `github.go:269`), so a
fake that ignores `page` and always returns 100 entries turns `ChangedFiles` into a 30-request loop and `findComment`
into an infinite one — `findComment` has no page bound at all, which is worth knowing and is itself a thing to test.

### What it asserts on every request

The fake is a test double and an assertion site at once. Anything it is certain about, it rejects rather than records:

- `Accept: application/vnd.github+json` and `X-GitHub-Api-Version: 2022-11-28` on every request (`doOnce` in
  `github.go`, `auth.go:127`). Missing either is a 400 from the fake. The version header is the reason this
  integration will still work when GitHub changes a default, and it is exactly the header a refactor drops.
- `Content-Type: application/json` when and only when there is a body (`doOnce`).
- `Authorization`. Two token namespaces, and the fake distinguishes them: a value that parses as a JWT signed by the
  App key is only accepted on the two App-level endpoints, and an opaque installation token is only accepted on the
  `/repos/...` endpoints. Anything else is 401. api.github.com makes this distinction; a fake that accepts any
  `Bearer` hides a credential mix-up until the live run.
- The JWT itself: `jwt.Parse` with the public key, `alg` must be RS256, `iss` must equal the App ID as a decimal
  string, `iat` must be in the past, and `exp - iat` must be at most ten minutes (`auth.go:31`, `auth.go:40`,
  `auth.go:89`). GitHub rejects a JWT that violates any of these with a bare 401, so the fake being stricter than
  necessary here is what buys a legible failure.

Everything it is not certain about goes in the ledger for the test to assert on: method, path, decoded query, decoded
body, which auth kind was presented, and arrival order.

### How a test drives it

```go
gh := newFakeGitHub(t)          // generates the RSA key, starts the server, t.Cleanup closes it
c := gh.client(t)               // real *github.Client with cfg.APIBase = gh.srv.URL

gh.comments = []comment{{ID: 1, Body: "LGTM"}}          // pre-existing noise
gh.files = []string{"docs/intro.md", "README.md"}

if err := c.Publish(ctx, report(scm.StateReady)); err != nil { t.Fatal(err) }

gh.wantWrites(t, "POST /repos/acme/docs/issues/42/comments")  // ordered, one line per write
```

`gh.client` sets `APIBase` to the server URL, which also drags the GHE branch of `CloneURL` (`github.go:127`) into
every test for free, since the URL is not `https://api.github.com`. Nothing in `config` validates the GitHub block —
there is no `github.` reference anywhere in `internal/config/config.go` — so injection needs no test-only escape
hatch. If a scheme check is ever added it must exempt loopback, or this whole approach dies.

Failure injection is one hook. `respond func(*call) *canned` is consulted before the default handler and returns nil
to fall through, so "the third PATCH gets a 401" and "every `/pulls/*/files` gets a 403 with
`X-RateLimit-Remaining: 0`" are both two lines in the test that needs them, and no test carries machinery for a case
it does not exercise.

## 2. What each test must prove, and why it is worth a test

### The App JWT is well formed and signed with the right key

The fake verifies it, so a single successful call proves it. What the test adds beyond that is the boundaries: assert
`exp - iat <= 10*time.Minute` and `iat < time.Now()` from the test as well, reading the claims out of the ledger.
Worth a test because both constants exist to work around clock skew on a laptop that has been suspended
(`auth.go:38`), the symptom of getting them wrong is a 401 that mentions nothing about clocks, and a well-meaning
change from nine minutes to ten breaks only on hosts whose clock runs fast.

### The installation token is exchanged, cached, and refreshed

Three tests, because they fail independently.

*Exchanged and cached*: two `ChangedFiles` calls for the same installation must produce exactly one
`POST /app/installations/{id}/access_tokens`. `doOnce` asks for a token on every single request, and one
`Publish` makes up to five requests — list comments, patch comment, list check runs, patch check run — so a broken
cache means five token exchanges per state change and four state changes per pull request. That is a rate-limit
budget spent entirely on authentication.

*Refreshed*: the fake returns `expires_at` at now+4 minutes, inside `tokenRefreshMargin` (`auth.go:48`), and the
second call must exchange again. Then now+1 hour, and it must not. Then `expires_at` absent entirely, which the
client backfills to an hour (`auth.go:151`) — assert it does not re-exchange, because the alternative reading of that
line is a zero time that expires immediately and re-exchanges forever.

*Per installation*: two installation IDs must get two distinct tokens, and the fake must see each token only on
requests for its own installation. The cache is keyed by installation (`auth.go:58`) and one docpreview can watch
several. Presenting installation A's token for B's repository returns 404, which reads as a deleted file.

### A 401 mid-build is handled

`do` now retries a 401 exactly once with a fresh token, calling `authenticator.invalidate` in between, because a
cached token can be revoked without expiring — a permissions change, a suspension, a reinstall — and the expiry
margin never notices. That retry is the single most test-shaped piece of logic in the client, and the fake is the only
way to drive it: three cases, all keyed on the ledger.

*A transient 401 recovers.* The fake 401s the first `PATCH .../issues/comments/{id}` and succeeds on the next. Assert
the ledger contains, in order: the failing PATCH, a second `POST /app/installations/{id}/access_tokens`, the
succeeding PATCH — and that the two PATCHes presented **different** tokens. Without the second exchange the retry is
just a repeat of the same rejected credential, and it would still pass a test that only counted PATCHes.

*A persistent 401 gives up after one retry.* The fake 401s everything. Assert exactly two attempts and exactly two
token exchanges, then an error. The flag is deliberately not a counter, and the failure mode of turning it into one is
an infinite loop against a genuine permissions problem.

*The retry does not duplicate a write.* The 401 must be injected on a `PATCH`, and the assertion is that exactly one
comment exists afterwards. `do` is method-agnostic, so a retried `POST .../issues/comments` would post twice — which
is the one thing the entire single-comment protocol exists to prevent. If the fake shows two comments, the retry needs
to be restricted to idempotent methods, and that is a finding worth the test.

Then the two daemon-level shapes, which are not the same as each other. A 401 that survives the retry on the token
exchange fails the build: `CloneURL` is the first thing `runPipeline` does (`daemon.go:699`), so the error propagates
and `build` reports `StateFailed` with a scrubbed one-line reason (`daemon.go:657`). But the failure report goes
through the same broken client, so assert that the second failure is *logged and swallowed* — `report` never returns
an error (`daemon.go:938`) — and that the daemon is still serving. A build that cannot tell anyone it failed must not
take the process down.

A 401 on the comment PATCH alone does not fail the build. The preview is live, the row is written, only the comment is
missing; `Publish`'s error is logged (`daemon.go:939`) and the dashboard event was recorded before the attempt
(`daemon.go:925`). Assert exactly that: the preview appears in `Status` and the activity feed shows `ready`.

### The comment is edited, not duplicated, and is found by its marker

Drive `Publish` five times with one `PreviewID` and states queued → building → ready. Assert: exactly one
`POST .../issues/42/comments` in the ledger, four `PATCH .../issues/comments/{id}` all to the same id, one comment in
the fake's list, and the marker (`internal/scm/scm.go:133`) appearing exactly once in the final body.

The part that makes this test worth more than the obvious version is the decoys. Pre-seed the fake with a human
comment, a comment whose body contains the literal string `docpreview` but no marker, and a comment carrying a
*different* preview's marker. `findComment` matches with `strings.Contains` (`github.go:265`), so all three must be
skipped. The last one is the one that matters: `Marker` embeds the preview ID specifically so two docpreview instances
on one repository do not fight over one comment, and that claim has never been executed.

Then the inverse: two different `PreviewID`s on the same pull request number must produce two comments.

Pagination too. Seed 150 comments with ours last, and assert `findComment` walks two pages and finds it. `findComment`
is the one loop in the client with no page bound (`github.go:253`); a fake that mishandles `page` would hang the test
suite, which is a good reason for the fake to be strict about it.

### The revision counter increments correctly

This one has to be stated precisely, because the number does not exist where the documents say it does.
`docs/design/09-scm.md:33` shows `revision 6` inside a rendered GitHub comment, and `RenderComment`
(`internal/scm/comment.go:24`) never emits a revision. The counter is a sqlite column
(`internal/store/store.go:75`) incremented by `UpsertComment` (`internal/store/store.go:312`), and the only caller is
the local simulator's `Publish` (`internal/scm/local/local.go:279`), rendered on the `/pr` page
(`internal/daemon/ingress.go:265`).

So: the counter test belongs in `internal/store` — two upserts leave `revision == 2` with `created_at` preserved and
`updated_at` advanced, which is what "one comment, updated" means. On the GitHub side the equivalent observable is
the write ledger, and the assertion is the one above: one POST, N PATCHes, one comment id. Either `09-scm.md` is
wrong or the renderer lost a field; the discrepancy is the kind a test of the renderer's output would have caught,
and writing that test is cheaper than deciding which.

### A newer push supersedes an older build

See §3.

### `ChangedFiles` handles pagination and an empty diff

Four cases against the fake's paging handler:

- 237 files: exactly three requests, `per_page=100`, pages 1/2/3, and 237 entries back in order.
- An empty diff: one request, a nil slice, no error. This is the skip path, so the daemon-level assertion is that the
  report is `StateSkipped` and the previous preview keeps serving.
- 3100 files: the walk stops at `maxChangedFilePages` (`github.go:159`) and logs a warning (`github.go:190`). Assert
  exactly 30 requests — the bound is there so a pathological pull request cannot pin a worker on pagination, and an
  off-by-one that removes it is invisible until it happens in production.
- A rename: `previous_filename` set, and both paths in the result (`github.go:181`). A file moved out of `docs/`
  is a documentation change even though its new path matches no doc glob, and that reasoning is only in a comment.

### A fork pull request is refused

The parser test exists (`webhook_test.go:119`). What the fake adds is the stronger claim: a fork delivery costs
**zero API requests**. Post a signed fork payload through the real ingress with a real client and assert
`len(gh.calls) == 0`. The refusal today is a `return nil, nil` before any client method is reached
(`webhook.go:138`), and the invariant worth guarding is not "no build" but "no use of the installation token on
behalf of a stranger's branch" — which is what a future "post a comment explaining why we refused" would break.

### A bad signature is rejected without leaking why

Covered at the parser (`webhook_test.go:65`) and at the ingress against a stub (`ingress_test.go:114`). The gap is
that no test runs the real verifier behind the real ingress. Install a real `*github.Client` in the daemon fixture,
post an unsigned and a wrongly-signed delivery, and assert 401, a body that does not contain "signature", and an
empty ledger. Zero API calls is the interesting half: an unauthenticated request that costs a token exchange is a
free amplifier on an internet-facing endpoint.

This requires a variant of `testIngress` (`internal/daemon/ingress_test.go:75`) that accepts an `scm.Client` instead
of a `*fakeClient`. Small change, and it is what lets one delivery-to-comment round trip run through production code
end to end.

### Rate limits, secondary rate limits, and the classifier

Table test over `errorFromResponse` and `retryAfterFrom`, driven through the fake so the real response path runs:

| Response | Expected |
|---|---|
| 403 with `X-RateLimit-Remaining: 0` | `RateLimited`, `Retryable()` |
| 403 with remaining 47 and no `Retry-After` | not rate-limited, not retryable |
| 403 with remaining 47 and `Retry-After: 60` | `RateLimited`, `RetryAfter == 60s` |
| 403 with remaining 47 and "secondary rate limit" in the message | `RateLimited` |
| 429 | `RateLimited` |
| `X-RateLimit-Reset` at epoch now+120 | `RetryAfter` within a second of 120s |
| `X-RateLimit-Reset` in the past | `RetryAfter == 0`, never negative |
| `Retry-After` as an HTTP-date | parsed, and a past date gives 0 |
| 500, 502 | `Retryable()` |
| 404 | `IsNotFound`, not retryable |
| 403 with an HTML body from a proxy | message is the trimmed body, not a JSON decode error |

Plain 403 must not be retryable: a permissions problem is the more common cause of one and retrying it spins forever.
The secondary limit is the opposite mistake — it fires on burst rather than volume, which is exactly what a supersede
storm produces, and classifying it as a permissions error turns a wait into a build that fails with the comment stuck
on "Building". The `Retry-After`-versus-`X-RateLimit-Reset` precedence and the two encodings of each are why this is a
table and not three assertions: every branch of `retryAfterFrom` is one line and every one of them is a plausible
place to invert a comparison.

One finding to record rather than fix here: `Retryable()`, `RetryAfter` and `IsNotFound` have no callers anywhere in
the tree. The classification is correct and informs no decision. Pin the classification with these tests and put the
missing consumer in `TODO.md` — a classifier nobody calls is dead code that reads like a safety net, and the tests are
what make it safe to add the consumer later.

### The check run is a convenience, and behaves like one

- A 500 from the check-run endpoints must leave `Publish` returning nil with the comment written (`github.go:205`).
  The comment is the durable artifact; losing a status line is cosmetic.
- No `PATCH` body may carry `head_sha` (`github.go:314`), because GitHub rejects it as an update field.
- Two runs sharing the name: `findCheckRun` takes the first (`github.go:334`). Assert it, so the arbitrary choice is
  recorded as a choice.
- `Retract` deletes the comment and issues no DELETE against check-runs (`github.go:378`).

### `CloneURL` builds a credential, and nothing else sees it

With `APIBase` at the fake's URL, `CloneURL` must produce `http://x-access-token:<escaped>@127.0.0.1:PORT/acme/docs.git`
— scheme and host derived from the base (`github.go:140`, `github.go:147`), token `url.QueryEscape`d
(`github.go:136`). That branch exists for GitHub Enterprise and is otherwise never executed by anything.

Then the negative: force a failure after the token is minted and assert the token string appears in no returned
error and in no log line captured by a `slog` handler the test installs. `CloneURL`'s doc comment claims this
(`github.go:117`) and nothing checks it.

## 3. The supersede test, and how to make it deterministic

This is the test most likely to be written with a `time.Sleep` and most likely to be deleted six months later for
flaking. The fix is to stop treating time as the coordination mechanism and use the fake as a rendezvous.

The insight is that the fake server sits *inside* the build. Every API call the client makes carries the build's
context (`doOnce`, `auth.go:122`), so an HTTP handler in the fake can (a) signal that a specific build has
reached a specific point and (b) observe that build being cancelled, via `r.Context().Done()`.

Hold on the **token exchange**, not on the files endpoint:

```go
// The gate makes the first build stop at a known instruction. It resolves itself
// when that build's context is cancelled, so nothing here waits on a clock.
arrived := make(chan struct{})
gh.gate = onceOn("POST /app/installations/999/access_tokens", func(r *http.Request) {
    close(arrived)
    <-r.Context().Done()   // the supersede releases this; no timer, no sleep
})
```

The sequence:

1. Post a signed `synchronize` delivery for `sha1` through the real ingress. A worker claims it and `build` calls
   `CloneURL`, which needs a token.
2. Wait on `arrived`. Build 1 is now provably parked before its clone, holding a `running` entry.
3. Post a delivery for `sha2`. `handleBuild` clears the `running` entry and cancels build 1 (`daemon.go:477`), then
   enqueues and reports `queued`.
4. The fake's handler unblocks because the request context is done. `installationToken` returns a context error,
   `runPipeline` returns it, and `build` takes the suppression branch — `ctx.Err() != nil && parent.Err() == nil`,
   `daemon.go:640` — so build 1 writes no report.
5. Build 2 runs to completion and writes `ready`.

Why the token exchange rather than `ChangedFiles`: `runPipeline` clones before it asks for changed files
(`daemon.go:704`), and with `APIBase` pointing at the fake the clone URL points at the fake too, so a gate on the
files endpoint is never reached. Gating the first API call needs no git repository at all. The clone failing
afterwards is irrelevant, because the cancelled context already routes build 1 to the same suppression branch.

Assertions, all on the ledger and none on timing:

- Exactly one comment id ever created.
- The final body contains `model.ShortSHA(sha2)` and does not contain `sha1`'s.
- No write carrying `StateReady` or `StateFailed` for `sha1`. Build 1's `building` PATCH is legitimate and expected;
  what must never appear is a terminal state from the loser, in any position.
- No write at all after build 2's `ready`. This is invariant 7 of `09-scm.md` and invariant 2 of `04-concurrency.md`:
  a superseded build publishes nothing.

What makes it deterministic, stated as rules for whoever writes it:

- No `time.Sleep` anywhere. Waiting is on channels, or on a bounded poll of an observable — `store.PendingCount` with
  a deadline, in the shape `ingress_test.go:150` already uses.
- The gate fires once, keyed by call ordinal rather than by path alone. Build 1's exchange never completed, so
  nothing was cached (`auth.go:155` is after the response), and build 2 issues its own exchange; a gate keyed only by
  path would park build 2 as well and deadlock.
- One worker. Two workers can claim in either order and the assertion about the final body becomes a coin flip.
- `-race -count 20` in CI. A supersede test that passes once tells you nothing.

What it deliberately does not prove: the commit phase. That is guarded by pointer identity and a per-preview lock
already covered by `supersede_test.go:81` and `commit_test.go`, and reproducing it here would need a real build. This
test proves the report suppression and the ownership handoff under webhook-shaped delivery, which is the half nothing
covers.

## 4. Driving a real `serve` across a restart

`11-github-setup-state.md:71` records four bugs, all found by using the page and none by the tests, and diagnoses it
correctly: the tests assert request and response, and every one of those bugs lived in a *sequence* — open the page,
set a passphrase, restart, come back. `rewire_test.go` closes part of it by asserting on the sequence rather than the
shape, but in one process, with the vault opened by a helper rather than by the daemon at boot.

### It belongs in `go test`, not in a PowerShell script

A `demo/Test-Restart.ps1` would be a second description of the wiring, maintained by whoever remembers it exists, run
by nobody. These four bugs were missed precisely because the behaviour lived outside the test run; moving the check
outside the test run again is the same mistake with better intentions. `demo/` keeps the job it is good at: driving
the *live* smoke run in §5, where a human is present anyway.

### Starting the process without building it

Re-exec the test binary. `main.go:44` already exposes `run(args []string) error`, so `TestMain` needs three lines:

```go
func TestMain(m *testing.M) {
    if os.Getenv("DOCPREVIEW_TEST_CHILD") == "serve" {
        if err := run(os.Args[1:]); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
        os.Exit(0)
    }
    os.Exit(m.Run())
}
```

No `go build`, no hunting for `build.claude/docpreview.exe`, no skew between the binary under test and the code in the
tree. The child is a real OS process with a real listener, which is the whole point.

### Ports, environment, readiness, and stopping

**Ports.** Bind `127.0.0.1:0`, read `ln.Addr().(*net.TCPAddr).Port`, close it, write that port into the temp config.
Racy in principle; in practice the kernel does not hand the same port to another process in the microseconds between,
and one retry on a bind failure covers it. The alternative — a fixed port — collides with the demo on `:8493` and
the GitHub work on `:8471`, which is the exact failure this avoids.

**Environment.** Set `cmd.Env` explicitly and *omit* `DOCPREVIEW_MASTER_KEY` (`internal/vault/vault.go:51`). A
developer with it exported would otherwise get a vault unlocked at boot and a test that proves nothing while passing.
The temp config names no `key_source`, so the file and exec sources (`internal/vault/keysource.go:48`) stay out too.

**Readiness.** Poll `GET /healthz` until 200 with a 20-second deadline. On timeout, `t.Fatal` with the child's
captured stderr — `slog` writes there, and the answer is almost always in a log line.

**Stopping.** `cmd.Process.Kill()`, deliberately. Windows has no `SIGTERM` and `os.Interrupt` is unsupported there,
and a hard kill is the stronger test anyway: "the passphrase did not survive a restart" was a hard-kill bug. Register
the kill in `t.Cleanup` so a panicking test does not leave a `docpreview` holding the sqlite file, which on Windows
breaks the *next* run and looks like a different bug.

### The sequence it asserts

1. Start with no vault. `GET /api/secrets` reports locked and no vault; `POST /webhook/github` is 501, which is the
   truth — nothing could be verified (`11-github-setup-state.md:57`).
2. `POST /api/secrets/unlock` with a passphrase. Assert 200 **and that `vault.age` exists on disk before the kill** —
   that is the bug from `secrets_test.go:204`, seen this time through a socket.
3. `PUT github.private_key` and `POST github.webhook_secret/generate`. `/webhook/github` stops answering 501.
4. Kill.
5. Start a second process on the same data dir, a fresh port. Assert locked **and vault present** — the bug showed
   here as a page offering to create the vault again, indistinguishable from data loss.
6. Unlock with the same passphrase. `/webhook/github` goes non-501 with no further writes, which is `rewireGitHub`
   firing on unlock (`rewire_test.go:136`) inside a real process rather than a fixture.
7. A third process, wrong passphrase: 401, and a body that does not speculate about corruption
   (`secrets_test.go:249`).

Roughly a second of wall clock for two process starts. Keep it out of `-short`, and keep it as one test function: the
steps share a data directory and are meaningless individually.

## 5. The live smoke test

The part a person does. Numbered because the order matters and because a half-finished run is worse than none — the
App is left pointing at a tunnel that no longer exists.

1. **Tunnel.** `zrok share public 127.0.0.1:8471`, note the public URL, and confirm `https://<share>/healthz` returns
   `ok` from a network that is not this machine — a phone works. If that fails, nothing after it means anything.
2. **Point the App at it.** Paste `https://<share>/webhook/github` into the App's Webhook URL and save. GitHub sends a
   `ping` immediately: look for `received github ping` in the log (`webhook.go:50`) and a green tick under
   Advanced → Recent Deliveries. A red tick here is a signature problem, and the delivery body is visible in that UI.
3. **Install on the one test repository.** Nothing should happen. Confirm the dashboard shows no build.
4. **Open a pull request touching `docs/`.** Watch for, in order: a 202 in Recent Deliveries; Queued then Building on
   the dashboard; a comment inside a minute; the preview URL in it opening in a browser. Then check the comment
   carries one marker (view source) and that a `docpreview` check appears in the PR's Checks tab.
5. **Push three commits inside a minute.** Look for: the comment count stays at one, its Updated timestamp advances,
   the final Commit matches `git rev-parse --short HEAD`, `superseding in-flight build` appears in the log at least
   once (`daemon.go:479`), and no `publishing report` error follows the final `ready`.
6. **Push a commit touching only a root `README`.** Status becomes Skipped and the previous preview URL still works.
7. **From a second account, fork and open a pull request.** Nothing happens; the log says
   `refusing to build a fork pull request` (`webhook.go:139`) and the fork's PR gets no comment. This is the step
   that cannot be faked convincingly, because a fake supplies its own `head.repo.full_name`.
8. **Rotate `github.webhook_secret` in the UI without updating the App.** The next push is a 401 with an empty body in
   Recent Deliveries. Put it back and confirm deliveries succeed again.
9. **Close the pull request.** The comment disappears, the preview URL 404s, and the check run stays on the commit
   (`github.go:378`).
10. **Come back after an hour and push once more.** This is the only exercise of installation-token refresh against
    GitHub's real expiry (`auth.go:112`); everything before it runs inside one token's lifetime.
11. **Uninstall the App.** Nothing in docpreview reacts to an uninstall — the previews stay live and the rows stay in
    the database. Confirm that is what happens, and record it as a deliberate limit or a TODO.

Steps 1–5 are the run that lets `11-github-setup-state.md` be deleted. Steps 6–11 are the ones worth doing once,
before anyone else uses this.

## The order to build it in

1. `internal/scm/github/fake_test.go`: the server, the ledger, the key, the fixture, the strict header and auth
   assertions. Nothing else can be written first, and every later step is short because of it.
2. The auth tests: JWT shape, exchange-and-cache, refresh at the margin, absent `expires_at`, per-installation
   isolation, and the three 401-retry cases — transient, persistent, and "does the retry duplicate a POST".
3. The comment tests: upsert with decoys, marker per preview, pagination in `findComment`, `Retract`, the check-run
   behaviours, `CloneURL`.
4. `ChangedFiles`: 237 files, empty, 3100, rename.
5. Error classification: the full response table, and every branch of `retryAfterFrom`. Record in `TODO.md` that
   `Retryable()` and `RetryAfter` have no caller.
6. Widen `testIngress` to accept an `scm.Client`, install a real one, and move the bad-signature and fork-refusal
   assertions to where they cost zero API calls to prove.
7. The supersede test on the gated token exchange. Run it `-race -count 20` before believing it.
8. The `serve`-across-a-restart test in `cmd/docpreview`.
9. The live smoke run, once, with a human. Then delete `11-github-setup-state.md` and fold whatever survives into
   `09-scm.md`.

Steps 1–5 need no other package to change. Step 6 is the only one that edits an existing test fixture, which is why
it is late.

## Not verified

- **Line numbers in `internal/scm/github`.** `github.go` and `auth.go` were being edited while this was written — the
  401 retry and the `Retry-After` handling landed mid-document — so anything cited past `do` is named by function
  rather than by line, and the auth cites may have drifted by a few lines. The claims are from reading the code as it
  stands; the line numbers are a convenience.
- **api.github.com's actual responses.** That the token exchange returns 201, that `X-RateLimit-Reset` is epoch
  seconds, that secondary limits use `Retry-After`, that a check-run PATCH rejects `head_sha` — all from
  documentation and memory, not from a live response. The fake encodes my beliefs about GitHub, and only §5 checks
  them. This is the irreducible reason the live run exists.
- **That an `http.Client` request cancellation surfaces in an `httptest` handler as `r.Context().Done()`.** True of
  `net/http` as I understand it, and the whole determinism argument in §3 rests on it, but nothing in this repository
  exercises it today. Prove it with a five-line spike before writing the supersede test.
- **`docs/design/09-scm.md:33`** showing `revision 6` in a GitHub comment that `RenderComment` cannot produce. I do
  not know whether that is a stale document or a lost feature.
- **Whether `SecretsAdmin.Available()` accepts `127.0.0.1:0`.** The restart test writes a real port into the config so
  it should not arise, but I read `secrets.go:86` only by name, not by body.
- **Windows port reuse timing** after closing the probe listener in §4. The one-retry mitigation is a guess at the
  right amount of paranoia.
- **`run()` being callable twice in one process.** The child calls it once so it does not arise; anyone tempted to run
  `serve` in-process instead of re-execing should audit package-level state first.
