# Documentation previews for private Bitbucket repositories

The hosted preview services are built for GitHub first. Support for anything else exists, trails behind, and
gets thinner the moment the repository is private.

That combination — Bitbucket Cloud, private source — is where we ended up needing our own answer. This is what
it takes, and what is genuinely different about Bitbucket once you stop treating it as GitHub with different
branding.

## The asymmetry that makes self-hosting easy to justify

Our published documentation is public. The source is not always.

That sounds like a problem and it is the opposite. The thing a reviewer needs to look at is already destined to
be a public web page, so putting a preview of it on a URL costs nothing in exposure. What we are protecting is
the repository, and the repository never has to leave the machine we already trust with it.

A hosted service inverts that. To show you a preview of a page that will be public, it wants a copy of the
source that is not, plus permission to run the scripts in your `package.json` on its hardware.

## Repository access tokens are the good part

Bitbucket has no App to install. There is no click-through, no private key to download, and no installation to
approve. You create a token and paste it.

That means less ceremony than GitHub and one property GitHub does not offer as readily: a **repository access
token** is scoped to exactly one repository. A leak costs you that repository. Workspace and project tokens
exist and are worth it once you have more than a few repositories, and they are also a wider credential sitting
in a process that runs builds, so the narrow one is the better default.

docpreview stores a credential per project as well as workspace-wide. A project with its own token uses it, a
project without one falls back to the workspace token, and the page tells you which is happening rather than
leaving you to work it out from a failure.

That per-project fallback is not a nicety. An administrator who does not permit workspace tokens leaves an
operator with one token per repository and nothing global to store, and a daemon that insists on a global
credential cannot be configured at all in that situation.

## Name the token carefully, because it signs every comment

Bitbucket posts the preview comment as the token, and shows the token's **name** next to it on every pull
request.

A token called `test` or `mytoken2` puts that on every documentation review your team does, forever, and
renaming it means creating a new token and re-storing it.

Call it `docpreview`. This is the cheapest mistake to avoid and the most annoying one to undo.

Two scopes, and no more:

| Scope | For |
|---|---|
| Repositories: Read | Cloning the branch, and reading which files a pull request changed |
| Pull requests: Write | Posting and editing the one preview comment |

## Four things that are actually different

Treating Bitbucket as GitHub with different URLs gets you most of the way and then fails in specific places.

**HTML comments render as visible text.** The usual way to make a bot comment self-identifying is an HTML
comment carrying an ID, so the bot can find its own comment later and edit it instead of posting a second one.
Bitbucket escapes raw HTML and renders that marker as a paragraph the reader can see. The replacement is a
CommonMark link reference definition, `[docpreview]: #<id>`, which renders to nothing on every platform.

The rule that matters more than the syntax: a matcher may gain a style and may never lose one. Comments live on
somebody else's server. A daemon that forgot the marker it used to write would post a second comment on every
open pull request the moment it shipped.

**Commit hashes arrive abbreviated.** Deliveries carry a 12-character hash. It checks out fine, and then fails
every comparison against a full hash, which produces a preview that builds correctly and matches nothing.
docpreview resolves it to the full hash on the way in.

**`Updated` means more than new commits.** Bitbucket fires it for a title change, a description edit, a reviewer
change and a retarget. So editing a typo in a pull request description rebuilds the site.

We accepted that rather than filtering it. Suppressing it means remembering the last commit built per pull
request, and the machinery to absorb the churn already exists, because a newer build cancels the one in flight.
The cost of getting it wrong is a wasted `npm install`, not a wrong answer.

**Fork detection reads a different field.** Same rule as GitHub, different place: the source repository's
`full_name` differing from the destination's. Fork pull requests are refused at the webhook, before anything is
queued, because building one means running a stranger's `package.json` scripts on your build host under a
credential that can write to your repository.

## Two that will waste an afternoon if nobody tells you

**Authenticated REST calls go to `api.bitbucket.org`.** A call to `bitbucket.org/api` answers 403 with a body
that does not explain itself, which reads as a permissions problem and sends you back to re-check the token you
just created. docpreview refuses that base URL at startup and names the right one.

**IPv6 to the API can fail after the handshake.** The connection is accepted, TLS completes, and the response is
reset partway through. It looks like a flaky network rather than a configuration issue, and it is the reason a
hand-run `curl` sometimes needs `--ipv4`. docpreview's HTTP transport dials IPv4 first.

Neither of these is interesting. Both cost real time when you meet them without warning.

## What you end up with

Four steps: create a token, generate a webhook secret, add the webhook, add the project. Adding the project
queues every open pull request, so there is something to look at before anybody pushes.

After that, a pull request gets a comment with a link, pushing updates the same comment, and closing the pull
request removes the preview.

No App, no seats, no source leaving your network. The whole thing runs on one VM, and the number of people
opening documentation pull requests does not change what it costs.

The setup is written up at
[Connect Bitbucket Cloud](https://github.com/openziti-test-kitchen/docpreview/blob/main/www/docs/guides/bitbucket.md).
