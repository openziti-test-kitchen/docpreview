---
id: build-logs
title: Build logs and secrets
sidebar_position: 5
---

# Build logs and secrets

Every build's output is captured, redacted, streamed live to the dashboard, and kept for download.

Redaction is the part that matters. A site build may need a credential, such as a search-index write key or a private
registry token, and build tools are careless with them. npm echoes its config on failure. A script under `set -x`
prints every command it runs. A failing HTTP client logs the request it made, credentials and all.

NetFoundry Preview quotes that output into a pull request comment, which is about the most public place a credential
could go.

## Giving a build a secret

There are two halves, and both go in the **server** config, never in `.docpreview.yml`:

```yaml
build:
  secrets:
    ALGOLIA_WRITE_KEY: algolia.write_key
    NPM_TOKEN: npm.token
```

The key is the environment variable the build sees. The value is a [vault](./security.md#credentials-at-rest) key.
Store the secret itself separately:

```powershell
docpreview vault set algolia.write_key
```

Every project gets these. For a credential that differs per repository, like a token for each private source a site
pulls content from, put it on the [project](./projects.md#environment-variables-scoped-to-a-project) instead. Those
are registered with the redactor the same way, and a project's value wins over a server-wide one of the same name.

:::danger Never in `.docpreview.yml`

`build.env` in a repository's own config takes literal values only. There is no vault lookup there. A pull request
author must not be able to *name* a secret and have it handed to a script they wrote.

Secret names are also reserved. A repository cannot shadow `ALGOLIA_WRITE_KEY` with a value of its own, which would
let it swap in a value the redactor does not know about.

:::

## What redaction guarantees

Every configured value is replaced with **exactly five asterisks**, wherever it appears:

```text
$ npm ci
npm ERR! code E401
npm ERR!   //registry.npmjs.org/:_authToken=*****
$ npm run build
+ ALGOLIA_WRITE_KEY=*****
[INFO] indexing to https://user:*****@algolia.net/1/indexes
{"apiKey":"*****","index":"docs"}
Authorization: Basic *****
```

**Five, always, whatever the secret's real length.** A mask that mirrored the length would leak it. Length tells a
40-character token from a 4-character PIN, and it tells anyone brute-forcing how much work they face.

### It catches encoded forms too

A build tool rarely prints a credential exactly as it was given. Each secret is matched in six shapes:

| Form | Where it shows up |
|---|---|
| raw | `set -x`, an echoed environment |
| URL query-escaped | a credential in a URL |
| URL path-escaped | the same, in a path segment |
| JSON-escaped | a request body being logged |
| base64 (three variants) | an `Authorization` header echoed in an error |

That set cannot be complete. A tool that hashes or encrypts a value before printing it gets past this, and so does
one that prints it in pieces. Redaction is a last line of defence over output nobody here controls. It is not a
licence to log secrets.

### Where it is applied

- Before anything reaches the log file. Nothing unredacted is written to disk, even for a moment. A file that holds
  a secret briefly can still be read, backed up, or swept into a crash dump.
- Before any line reaches a live tail.
- On the text of every error the build returns, since those are quoted into pull request comments.
- Again in the daemon on the way to a comment, because failures also come from the clone and the detect script,
  which the builder never saw.

### Why the log is buffered by line

A pipe can split a secret across two writes, and a scrubber that checks each write alone would miss it. The writer
holds output until it has a whole line, scrubs the line, then writes it.

There is one limit. A line longer than 64 KiB with no newline in it, like a minified bundle echoed to stdout or a
progress bar drawn with carriage returns, is flushed at the cap instead of buffered forever. It is still scrubbed on
the way out, so a secret is missed only if it straddles that exact boundary.

### Values shorter than four characters are refused

Redacting `"a"` would replace every `a` in the log, wrecking it and telling an attacker the secret is one character.
Short values are skipped, and the count is logged at startup:

```text
WARN some build secrets are too short to redact and will appear in logs verbatim count=1
```

The variable is not named, because the name is the lookup key into the vault.

## Reading logs

**Live**, on the dashboard: pick a preview and its log tails as the build runs. The log follows new lines only while
you are at the bottom. Turn off `Following` to stop it scrolling.

**After the fact**, the same panel replays the most recent log from disk.

**Every line carries the time it arrived**, as `18:31:18.128`. Hover one to see the rest:

| | |
|---|---|
| Your zone | `Aug 13, 2026, 6:31:18 PM` |
| UTC | `2026-08-13 22:31:18.128 UTC` |
| Relative | `6 minutes ago` |
| Relative to start | `1.5s` |
| Relative to previous | `112ms` |

The gap before a line is how long the step above it took, which is usually why you opened the log.
The stamp has no date and no zone so the column stays one width. The date comes from the build's start, and the zone
from the daemon, which may not be yours.

:::note The stamp is when NetFoundry Preview *read* the line, not when the build printed it

Output inside the container goes to a pipe, not a terminal, so node, yarn and Docusaurus switch to block buffering and
flush several kilobytes at once. Every line in one flush gets nearly the same stamp. The stamps are exact about
arrival and rough about origin, which is still enough to answer "what took fifty seconds".

:::

**As a file:**

```text
GET /logs/{preview}/download            the most recent build
GET /logs/{preview}/download/{build}    a specific one
GET /logs/{preview}                     JSON list of what is stored
```

Downloads are served as attachments with `nosniff`. Build output is arbitrary bytes from a tool nobody vetted, and
rendering it inline would let the browser interpret it.

## Retention

| | |
|---|---|
| Per preview | The five most recent builds. Enough to compare a failure with the build before it. |
| Overall | `build.keep_logs`, default 7 days, swept hourly. |
| On teardown | Deleted with the preview. |

A build in flight is never swept, however old its directory looks, so a long build cannot lose its own log
underneath it.

:::note Logs and artifacts are kept on different counts

Five logs per preview, against [`preview.keep_builds`](./configuration.md#preview) artifacts, ten by default. The two
answer different questions. An artifact directory lets you open an old commit's site, and a log lets you read a
failure. A failed build has a log and no artifacts at all.

So the dashboard can show a build you can open whose log is gone, and a build whose log is there but which you can no
longer open. Both are correct.

:::

```yaml
build:
  keep_logs: 168h
```

Logs can hold anything a build printed, so an unbounded pile of them is a liability.

## Failed builds

A failed build still writes a log, and the dashboard shows the failure with its reason.

**That includes a failure before the build starts.** A clone that cannot authenticate, a `.docpreview.yml` that does
not parse, a detect script at a path that does not exist: none of those reach the build, and each writes a log saying
so:

```text
$ this build did not start
detect script ".docpreview/detect": lstat /var/lib/docpreview/workspaces/4f38/scripts: no such file or directory
```

The wording tells the two cases apart. `did not start` means nothing was built. `did not publish` means the build
succeeded and the preview could not be served: a name already taken, an exhausted quota, a controller refusing state
changes. Each sends you to a different place.

**The publish step narrates itself** into the same log, so the time between a finished build and a live URL is
accounted for:

```text
$ build finished 1m57s
$ registering the zrok name a-acme-docs-add-guide
$ creating the public share
$ opening the listener for share wfcx1nwrfsze
$ zrok is rate limiting state changes, backing off (open listener wfcx1nwrfsze) — retrying in 15s
$ published https://a-acme-docs-add-guide.shares.zrok.io/
$ preview is live at https://a-acme-docs-add-guide.shares.zrok.io/ (1m3s)
```

A rebuild of a branch that already has a share skips all of that and says
`$ serving the new build on the existing share <url>`. When the zrok share budget for the window is spent, the log
says `$ zrok share queued: 15 of 15 used in this window, next slot at <time>` and waits for the slot.

If a preview was already published and a *rebuild* fails, the old preview stays live and its card keeps saying
`Ready`. That is accurate: the URL still works and still serves the last good build. The failure shows in the
activity feed, and the log is reachable by preview ID.

## Related

- [Security model](./security.md)
- [Server configuration](./configuration.md)
- [Projects](./projects.md): per-repository environment variables, redacted the same way
