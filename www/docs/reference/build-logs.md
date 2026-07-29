---
id: build-logs
title: Build logs and secrets
sidebar_position: 5
---

# Build logs and secrets

Every build's output is captured, redacted, streamed live to the dashboard, and kept for download.

The interesting part is the redaction, because a documentation build may legitimately need a credential — a
search-index write key, a private registry token — and build tools are careless with them. npm echoes its
config on failure. A script under `set -x` prints every command it runs. A failing HTTP client logs the
request it made, credentials and all.

Any of that lands in output docpreview then writes into a pull request comment, which is about the most public
place a credential could go.

## Giving a build a secret

Two halves, both in the **server** config, never in `.docpreview.yml`:

```yaml
build:
  secrets:
    ALGOLIA_WRITE_KEY: algolia.write_key
    NPM_TOKEN: npm.token
```

The key is the environment variable the build sees. The value is a [vault](./security.md#credentials-at-rest)
key. Store the actual secret separately:

```powershell
docpreview vault set algolia.write_key
```

:::danger Never in `.docpreview.yml`

`build.env` in a repository's own config takes literal values only, and there is no vault lookup there. A pull
request author must not be able to *name* a secret and have it handed to a script they wrote.

Secret names are also reserved: a repository cannot shadow `ALGOLIA_WRITE_KEY` with a value of its own, which
would otherwise let it substitute a value the redactor does not know about.

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

**Five, always, regardless of the secret's real length.** A mask that mirrored the length would leak it — and
length is a real clue: it distinguishes a 40-character token from a 4-character PIN, and it tells anyone
brute-forcing exactly how much work they face.

### It catches encoded forms too

A build tool rarely prints a credential exactly as it was given. Each secret is matched in six shapes:

| Form | Where it shows up |
|---|---|
| raw | `set -x`, an echoed environment |
| URL query-escaped | a credential in a URL |
| URL path-escaped | the same, in a path segment |
| JSON-escaped | a request body being logged |
| base64 (three variants) | an `Authorization` header echoed in an error |

That set is not exhaustive and cannot be. A tool that hashes or encrypts a value before printing it defeats
this, as does one that prints it in fragments. Redaction is a last line of defence over output docpreview does
not control, not a licence to log secrets.

### Where it is applied

- Before anything reaches the log file. Nothing unredacted is ever written to disk, not even momentarily —
  a file that holds a secret for a moment is a file that can be read, backed up, or swept into a crash dump.
- Before any line reaches a live tail.
- On the text of every error the build returns, since those are quoted into pull request comments.
- Again in the daemon on the way to a comment, because failures also arrive from the clone and the detect
  script, which the builder never saw.

### Why the log is buffered by line

Redaction operates on text, and a secret split across two writes is invisible to a scrubber looking at each
write in isolation. `os/exec` hands over whatever the pipe happened to deliver, so the split point is
arbitrary and this genuinely happens.

The writer therefore holds output until it has a complete line, scrubs it, and only then writes it anywhere.

One exception, stated because it is a real limit: a line longer than 64 KiB with no newline in it — a minified
bundle echoed to stdout, a progress bar drawn with carriage returns — is flushed at the cap rather than
buffered without limit. It is still scrubbed on the way out, so a secret is only missed if it straddles
exactly that boundary.

### Values shorter than four characters are refused

Redacting `"a"` would replace every `a` in the log, destroying it while telling an attacker the secret is one
character. Short values are skipped and the count is logged at startup:

```text
WARN some build secrets are too short to redact and will appear in logs verbatim count=1
```

The variable is deliberately not named, because the name is the lookup key into the vault.

## Reading logs

**Live**, on the dashboard: pick a preview and its log tails as the build runs. The `Following` toggle stops
it scrolling, and it only auto-scrolls when you are already at the bottom — being yanked back down while
reading further up is the most irritating thing a log viewer can do.

**After the fact**, the same panel replays the most recent log from disk.

**As a file:**

```text
GET /logs/{preview}/download            the most recent build
GET /logs/{preview}/download/{build}    a specific one
GET /logs/{preview}                     JSON list of what is stored
```

Downloads are served as attachments with `nosniff`. Build output is arbitrary bytes from a tool nobody vetted,
and rendering it inline would let it be interpreted.

## Retention

| | |
|---|---|
| Per preview | The five most recent builds. Enough to compare a failure against the build before it, which is the question people actually ask. |
| Overall | `build.keep_logs`, default 7 days, swept hourly. |
| On teardown | Deleted with the preview. |

A build in flight is never swept, however old its directory looks — otherwise a long build could have its own
log deleted underneath it.

```yaml
build:
  keep_logs: 168h
```

Logs can contain anything a build printed, so an unbounded pile of them is a liability rather than an asset.

## Failed builds

A failed build still produces a log, and the failure appears on the dashboard with its reason so there is
something to click.

One subtlety worth knowing: if a preview was already published and a *rebuild* fails, the old preview stays
live and its card keeps saying `Ready`. That is the truth — the URL still works and still serves the last good
build. The failure shows in the activity feed, and the log is reachable by preview ID.

## Related

- [Security model](./security.md)
- [Server configuration](./configuration.md)
