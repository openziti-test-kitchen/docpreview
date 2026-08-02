---
id: age
title: age, and why the vault uses it
sidebar_position: 4
---

# age, and why the vault uses it

docpreview's [vault](../reference/security.md#credentials-at-rest) is one file encrypted with
[age](https://age-encryption.org). If you have not run into age before, this page is what it is, why it was
chosen, and what it means for you operationally.

## What age is

A **file encryption format**, plus a CLI and a Go library that implement it. The name stands for "Actually
Good Encryption". It was designed by [Filippo Valsorda](https://filippo.io/), who ran the Go security team,
and Ben Cartwright-Cox, as a replacement for GPG in the one case GPG handles worst: encrypting a file.

docpreview links [the Go library](https://pkg.go.dev/filippo.io/age) directly. You do **not** need the age CLI
installed — nothing shells out to it, and the binary has no external dependency to satisfy.

## What an age file looks like

A real vault, straight off disk:

```text
age-encryption.org/v1
-> scrypt l679obn3QvRR2+L5otAKng 18
ZJZaL98LqO6IsRdH3YXxo/7OoKaB7znGUISQ/AnfHUo
--- RmLazOvHAbS9aSdEJFiDDuZYB4h8kS9FqNg4kSiPNp0
<binary ciphertext>
```

That is the whole format:

| Line | Meaning |
|---|---|
| `age-encryption.org/v1` | Version marker. |
| `-> scrypt <salt> 18` | One stanza per recipient. Here, a passphrase with a 2^18 scrypt work factor. |
| `--- <mac>` | A MAC over the header, so a tampered header fails rather than silently changing behaviour. |
| body | ChaCha20-Poly1305 ciphertext. |

With a keypair instead of a passphrase, the stanza is `-> X25519 <ephemeral public key>`.

## How it works

age generates a random **file key** and encrypts your data with it using ChaCha20-Poly1305, in 64 KiB chunks
under the [STREAM](https://eprint.iacr.org/2015/189.pdf) construction. That last detail matters for a vault: a
truncated or reordered file fails to decrypt rather than returning partial plaintext.

It then wraps that file key once per recipient — each `->` stanza is one wrapping. Two ways to wrap it, and
docpreview supports both:

- **X25519.** An ECDH exchange against your public key. This is what `docpreview vault keygen` produces, and
  what an `AGE-SECRET-KEY-1...` value is.
- **scrypt.** A key derivation function over your passphrase. The work factor is why the passphrase path is
  noticeably slower to open. That is the feature, not a defect — it is what makes a guessing attack expensive.

Because the file key is wrapped separately per recipient, one file can be readable by several keys without
re-encrypting the body. docpreview uses one today. That is why "a vault shared across a team" is a small change
rather than a rewrite.

## Why age and not something else

### Why not hand-roll it

Encrypting a JSON blob with a symmetric cipher is a dozen lines, and two of those lines are where it goes
wrong.

**Nonce reuse.** Use the same nonce twice with a stream cipher and the two ciphertexts XOR into plaintext.

**The wrong KDF.** Derive a key from a passphrase with a fast hash instead of scrypt or Argon2, and the
passphrase falls to a wordlist overnight.

Neither failure announces itself. The file still encrypts, still decrypts, still passes every test you would
think to write — right up until somebody else decrypts it too. age makes both decisions for you and does not
expose a knob to get them wrong.

### Why not GPG

GPG brings a keyring, a web-of-trust model, an agent, its own home directory, and thirty years of accumulated
options to solve "encrypt this one file with one key". Its answer to that question involves reading a manual.
age's answer is one function call.

### Why not a hosted secret manager

AWS KMS, HashiCorp Vault, and Azure Key Vault are all better than a file, for a service that can reach them.
docpreview's premise is that it runs anywhere — a laptop, a VM, a container in a network with no outbound
internet. A hard dependency on a cloud service would break that on day one.

The [threat model](../reference/security.md#threat-model) is deliberately modest to match: it defends against
someone who walks off with the disk, and not against someone already running code as your user. A file plus an
environment variable is the right size for that claim, and dressing it up as more would be the actual mistake.

### Why the API size is the point

This is the complete age surface docpreview touches:

```go
age.GenerateX25519Identity()   age.ParseX25519Identity(s)
age.NewScryptIdentity(pass)    age.NewScryptRecipient(pass)
age.Encrypt(w, recipient)      age.Decrypt(r, identity)
```

No cipher selection, no key size, no mode, no padding, no keyring, no trust database, no config file. You
cannot configure age into being insecure, because there is nothing to configure. For a credential store that
somebody will set up once and then not think about for a year, that is worth more than flexibility.

## What this means for you

**Two things, in two places, and neither is useful alone.**

| | Where it lives | Created by |
|---|---|---|
| The encrypted secrets | `~/.docpreview/vault.age` on disk | `docpreview vault set`, on first use |
| The master key | wherever `vault.key_source` points, or nowhere | you, from `docpreview vault keygen` |

Losing the key means the vault is unrecoverable — which is what "encrypted at rest" has to mean to be worth
anything. **Put it in a password manager before you do anything else.**

**Where the key comes from is a real choice, and docpreview does not make it for you.** The vault is a secrets
manager, so its own key has nowhere inside the system to live. Every answer is a trade:

| `vault.key_source` | What you get | What it costs |
|---|---|---|
| `exec:op read op://ops/docpreview` | the key exists in the daemon process and nowhere else on the machine | the helper has to be able to run unattended |
| `file:/etc/docpreview/master.key` | the daemon survives a reboot on its own | anyone who can read that path can read every secret |
| unset | no key at rest anywhere | a person unlocks after every restart, from the dashboard |

Unset is the default. `docpreview vault keygen -out <path>` writes a key to a file at mode 0600 for the middle
row, and the bare form prints it for piping into a password manager for the first.

`$DOCPREVIEW_MASTER_KEY` still works and is consulted when `key_source` is unset. It is the weakest of the
options: an environment variable is readable by every process under the same user, and lands in service
definitions, process listings and crash dumps.

**A passphrase works instead of a key.** Anything that does not start with `AGE-SECRET-KEY-1` is treated as one
and stretched with scrypt. Convenient for a quick trial. A generated key is better for a long-running service,
because a passphrase a human can remember is one a wordlist can too.

**With no source at all, `serve` starts anyway** with the vault locked, and the dashboard unlocks it. On a
terminal the CLI commands prompt instead, with echo off. Off a terminal, with nothing configured:

```text
vault is locked: set vault.key_source in the config, unlock it from the dashboard, or run on a terminal
```

## Inspecting a vault yourself

The vault is a standard age file, so the [age CLI](https://github.com/FiloSottile/age) reads it — useful for
confirming for yourself that what is on disk is what this page claims.

```bash
head -c 60 ~/.docpreview/vault.age          # the header shown above
grep -c "BEGIN RSA PRIVATE KEY" ~/.docpreview/vault.age   # 0
grep -c "github" ~/.docpreview/vault.age                  # 0 — even the names are encrypted
```

```bash
age --decrypt -i key.txt ~/.docpreview/vault.age
```

docpreview itself has no command that prints a secret value, deliberately. `docpreview vault list` shows names
only.

## Further reading

- [age-encryption.org](https://age-encryption.org) — the spec and the CLI
- [`filippo.io/age`](https://pkg.go.dev/filippo.io/age) — the Go package docpreview links
- [FiloSottile/age](https://github.com/FiloSottile/age) — source and releases
- [The age format specification](https://github.com/C2SP/C2SP/blob/main/age.md) — at C2SP, the community
  cryptography specification project
- [Security model](../reference/security.md) — how docpreview uses all of this
