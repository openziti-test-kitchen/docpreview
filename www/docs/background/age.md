---
id: age
title: age, and why the vault uses it
sidebar_position: 4
---

# age, and why the vault uses it

NetFoundry Preview keeps its credentials in a [vault](../reference/security.md#credentials-at-rest): one file
encrypted with [age](https://age-encryption.org). This page covers what age is, why it was chosen, and what it means
for you when you run the daemon.

## What age is

age is a **file encryption format**, with a CLI and a Go library that implement it. The name stands for "Actually Good
Encryption". [Filippo Valsorda](https://filippo.io/), who ran the Go security team, designed it with Ben
Cartwright-Cox to replace GPG for the job GPG handles worst: encrypting a file.

NetFoundry Preview links [the Go library](https://pkg.go.dev/filippo.io/age) directly. You do **not** need the age CLI
installed. Nothing shells out to it.

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

age generates a random **file key** and encrypts your data with it using ChaCha20-Poly1305, in 64 KiB chunks under
the [STREAM](https://eprint.iacr.org/2015/189.pdf) construction. That matters for a vault: a truncated or reordered
file fails to decrypt instead of returning partial plaintext.

Then it wraps that file key once for each recipient, and each `->` stanza is one wrapping. There are two ways to wrap
it, and NetFoundry Preview supports both:

- **X25519.** An ECDH exchange against your public key. `nfpreview vault keygen` makes one of these, and an
  `AGE-SECRET-KEY-1...` value is one.
- **scrypt.** A key derivation function over your passphrase. Its work factor makes the passphrase path noticeably
  slower to open. That slowness is deliberate: it makes a guessing attack expensive.

Each recipient wraps the file key separately, so several keys can read one file without re-encrypting the body. The
vault uses one recipient today, which keeps "a vault shared across a team" a small change.

## Why age and not something else

### Why not hand-roll it

Encrypting a JSON blob with a symmetric cipher takes a dozen lines, and two of them are where it goes wrong.

**Nonce reuse.** Use the same nonce twice with a stream cipher and the two ciphertexts XOR into plaintext.

**The wrong KDF.** Derive a key from a passphrase with a fast hash instead of scrypt or Argon2, and the passphrase
falls to a wordlist overnight.

Neither failure shows. The file still encrypts, still decrypts, and still passes every test you would think to write,
until somebody else decrypts it too. age makes both decisions for you and offers no knob to get them wrong.

### Why not GPG

GPG brings a keyring, a web-of-trust model, an agent, its own home directory, and thirty years of options to solve
"encrypt this one file with one key". Its answer starts with reading a manual. age's answer is one function call.

### Why not a hosted secret manager

AWS KMS, HashiCorp Vault and Azure Key Vault all beat a file, for a service that can reach them. NetFoundry Preview
runs anywhere: a laptop, a VM, a container on a network with no outbound internet. A hard dependency on a cloud
service would break that on day one. You can still keep the vault's key in one of them through `exec:`, below.

The [threat model](../reference/security.md#threat-model) is modest to match. It defends against someone who walks
off with the disk, not against someone already running code as your user. A file and a key kept elsewhere are the
right size for that.

### Why the small API matters

This is all of age that NetFoundry Preview uses:

```go
age.GenerateX25519Identity()   age.ParseX25519Identity(s)
age.NewScryptIdentity(pass)    age.NewScryptRecipient(pass)
age.Encrypt(w, recipient)      age.Decrypt(r, identity)
```

No cipher selection, no key size, no mode, no padding, no keyring, no trust database, no config file. You cannot
configure age into being insecure, because there is nothing to configure. A credential store gets set up once and then
left alone for a year, so that matters more than flexibility.

## What this means for you

**Two things, in two places, and you need both.**

| | Where it lives | Created by |
|---|---|---|
| The encrypted secrets | `~/.nfpreview/vault.age` on disk | `nfpreview vault set`, on first use |
| The master key | wherever `vault.key_source` points, or nowhere | you, from `nfpreview vault keygen` |

Lose the key and the vault is gone for good. That is what "encrypted at rest" has to mean. **Put the key in a password
manager before you do anything else.**

**You choose where the key comes from.** The vault is the secrets manager, so its own key has nowhere inside the
system to live. Every choice is a trade:

| `vault.key_source` | What you get | What it costs |
|---|---|---|
| `exec:op read op://ops/nfpreview` | the key exists in the daemon process and nowhere else on the machine | the helper has to be able to run unattended |
| `file:/etc/nfpreview/master.key` | the daemon survives a reboot on its own | anyone who can read that path can read every secret |
| unset | no key at rest anywhere | a person unlocks after every restart, from the dashboard |

Unset is the default. For the `file:` row, `nfpreview vault keygen -out <path>` writes a new key at mode 0600 and
refuses to overwrite one that exists. The path must be outside `data_dir`, or the config is rejected. For the `exec:`
row, bare `nfpreview vault keygen` prints the key so you can pipe it into a password manager.

`$NFPREVIEW_MASTER_KEY` still works, and is read when `key_source` is unset. It is the weakest option: every process
under the same user can read an environment variable, and it ends up in service definitions, process listings and
crash dumps.

**A passphrase works instead of a key.** Anything that does not start with `AGE-SECRET-KEY-1` is treated as a
passphrase and stretched with scrypt. That is fine for a quick trial. Use a generated key for a long-running service,
because a passphrase a human can remember is one a wordlist can find.

**With no key at all, `serve` starts anyway** with the vault locked, and you unlock it from the dashboard. On a
terminal, the CLI commands prompt for the key with echo off. Off a terminal, with nothing configured, they fail with:

```text
vault is locked: no master key available: set vault.key_source in the config, unlock it from the dashboard, or run on a terminal
```

To change the key later without losing secrets, stop the daemon and run `nfpreview vault rekey -generate -yes`.

## Inspecting a vault yourself

The vault is a standard age file, so the [age CLI](https://github.com/FiloSottile/age) reads it. Use it to check that
what is on disk matches this page.

```bash
head -c 60 ~/.nfpreview/vault.age          # the header shown above
grep -c "BEGIN RSA PRIVATE KEY" ~/.nfpreview/vault.age   # 0
grep -c "github" ~/.nfpreview/vault.age                  # 0: even the names are encrypted
```

```bash
age --decrypt -i key.txt ~/.nfpreview/vault.age
```

NetFoundry Preview has no command that prints a secret value. `nfpreview vault list` shows names only.

## Further reading

- [age-encryption.org](https://age-encryption.org): the spec and the CLI
- [`filippo.io/age`](https://pkg.go.dev/filippo.io/age): the Go package NetFoundry Preview links
- [FiloSottile/age](https://github.com/FiloSottile/age): source and releases
- [The age format specification](https://github.com/C2SP/C2SP/blob/main/age.md), at C2SP, the community cryptography
  specification project
- [Security model](../reference/security.md): how NetFoundry Preview uses all of this
