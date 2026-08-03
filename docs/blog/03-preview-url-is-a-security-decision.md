# Your preview URL is a security decision

A documentation preview is a URL that serves unreleased writing to whoever opens it.

Most of the time that is fine. Sometimes it is a product announcement under embargo, or a runbook naming
internal hostnames, or a page about a vulnerability that has not shipped a fix. The right audience for a preview
is not a constant, and a preview system that only knows how to publish one way has already decided for you.

So in docpreview it is one setting with four values, and everything above it is unchanged when you switch. Not
the queue, not the builder, not the comment.

| | Reachable by | Public surface |
|---|---|---|
| `local` | you, on loopback | none |
| `zrok2` | anyone with the link, or an OAuth-gated subset | yes |
| `frontdoor` | whoever your identity provider admits | yes |
| `ziti` | a machine running a tunneler with an enrolled identity | none at all |

## `local`: prove the pipeline without an account

Binds a loopback port and serves there. The comment gets a `http://127.0.0.1:54321/` link, which is useless to
anybody else and exactly right for the first hour.

It exercises the whole path — clone, detect, build, verify, comment — with no account anywhere. When something
misbehaves under a real exposer later, it is also the reference that tells you whether the problem is the build
or the publishing.

## `zrok2`: a public link, or a gated one

The default. A preview is published over an OpenZiti overlay and reachable at a stable public hostname.

Open by default, which is the correct setting for documentation that is about to be published anyway. The link
is the credential, and anyone holding it can read the page.

When that is too much, two dials sit on top:

- **`access_grants`** names the zrok accounts allowed to reach it, and nobody else can, link or no link.
- **`oauth_provider`** puts Google or GitHub in front, optionally restricted to email domains you list. A
  reviewer signs in and reads. Somebody who finds the URL does not.

That covers most of what a team actually needs, and it needs no identity infrastructure of your own.

## `frontdoor`: your identity provider, and MFA

NetFoundry Frontdoor is the same shape with an enterprise front end: a hardened distributed frontend, a web
application firewall, and access enforced by your own IdP with MFA.

The reason to reach for it over zrok's OAuth is rarely the authentication by itself. It is that access to
previews lands in the same place as access to everything else, audited alongside it, using the group
memberships that already exist. "Documentation previews" is a small enough thing that nobody wants a separate
access-control story for it.

One honest note. The Frontdoor exposer is implemented and its HTTP wire format has not been exercised against a
live tenant. The lifecycle around it — reaping, naming, retries, collision handling — is the same code the zrok
exposer uses and is covered by the same tests. Treat it as ready to try rather than ready to depend on, and
read [the exposer documentation](https://github.com/openziti-test-kitchen/docpreview/blob/main/www/docs/exposers.md)
before you do.

## `ziti`: no public surface at all

This is the one that has no equivalent in a hosted service, and it is the reason this project lives where it
does.

Published over an OpenZiti overlay, a preview has nothing on the public internet. The hostname does not resolve.
The address is not routable. There is no port to scan and no login page to attack, because there is nothing
listening that an unenrolled machine can reach at all.

Access is an identity enrolled on the controller, carrying an attribute a policy grants. A reviewer runs a
tunneler, opens the link, and reads the page. Somebody who does not have that identity cannot reach the service
to be refused by it.

The trade is real and worth stating. There is no clickable link in the pull request for a person without a
tunneler, and that rules it out for anything with outside reviewers. For internal documentation under embargo it
is the strongest answer available.

**A caveat we are not going to bury.** Today one wildcard service carries every preview, and requests are
separated by the HTTP `Host` header the client sends. So anyone holding the reader attribute can reach every
preview by asking for any hostname. Against an outsider that is a hard boundary. Among people who all hold the
attribute it is not a boundary at all, and calling it zero trust would be a stretch. Checking the dialing
identity in the HTTP handler is the fix, and it is written up rather than shipped.

## The dashboard is a surface too

Choosing how previews are published is half the question. The daemon also serves a dashboard that can store
credentials and decide what command runs on the build host, and that is a more direct route to executing code on
your machine than reading the vault would be.

So it has its own login, with two roles that are not two levels of the same thing. `admin` changes things from
anywhere the daemon is reachable. `viewer` reads everything and changes nothing. Passwords are argon2id, and the
session signing key is generated per process, so a restart signs everybody out.

Google sign-in grants **viewer** and never admin. An address at a domain you allow-list is a claim that somebody
works at your company, and that is not the same claim as being the person who administers this installation.

## What to pick

Start at `local` and confirm the pipeline works. Move to `zrok2`, which is where most teams stop. Add OAuth or
`access_grants` when the content warrants it. Reach for `frontdoor` when previews should be governed by the same
IdP as everything else, and `ziti` when the honest answer to "who should be able to reach this" is "nobody
outside, ever".

Changing your mind is one line of configuration and a restart. Every preview is republished at the new kind of
address and every open pull request comment is rewritten to match, which is disruptive on purpose — the
dashboard asks before doing it.

The comparison, in more detail, is at
[Exposers](https://github.com/openziti-test-kitchen/docpreview/blob/main/www/docs/exposers.md).
