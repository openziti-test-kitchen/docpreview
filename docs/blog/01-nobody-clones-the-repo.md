# Nobody clones the repo to review your docs

Somebody opens a pull request against your documentation. Forty files, mostly prose, four new pages, and a
restructured sidebar.

To review that properly you have to see it. Clone the branch, install the dependencies, start the dev
server, wait, click around. Ten minutes when the toolchain cooperates and you already have the right Node
version on the machine. Longer when you do not.

So nobody does it. They read the diff and approve.

## What a diff hides

A markdown diff is good at catching a typo and bad at catching most other things.

It does not show you the link pointing at a page that got renamed in the same pull request. It does not
show you the heading somebody reworded, quietly breaking the six anchors elsewhere that pointed into it.
It does not show you the new page that never made it into the sidebar, so it exists and nothing links to
it. It does not show you the image that 404s, the code fence that lost its closing backticks and swallowed
the next three paragraphs, or the table that renders as one long line.

Those all look fine as source. They are only visible as a page.

There is a worse category. A documentation site build can succeed and produce something broken. Build a
Docusaurus site for `/my-project/` and serve it at `/`, and every generated `href` carries a prefix that
is not there. `index.html` loads, returns 200, and every stylesheet and script 404s. What you get is
unstyled text with no navigation, and nothing in the build log says why, because as far as the build was
concerned it worked.

The reviewer sees a broken page and reports "the preview is broken". Nobody thinks to look at asset URLs.

## The hosted answer, and why we wanted another way

Vercel and Netlify solved this years ago, and solved it well. Every pull request gets built, a bot drops a link on
it, and the link stays current as the branch moves. If that works for you, keep using it. It's the model we copied.

Our reason for wanting another way came down to one thing: who gets a preview at all.

### Every contributor needed to be on the team

Most of our documentation repositories are private. In our setup, Vercel would only build a pull request on a
private repository if its author was a member of our Vercel team. Somebody outside it would push a change, and the
build was blocked until somebody added them. That's a sensible rule from Vercel's side. A build runs the author's
code, and on a private repository they want to know whose code that is.

For a product team that's no trouble. The people opening pull requests are the people who ship the application,
and that's a short, stable list.

Documentation contributors are a different crowd. They are the widest and most occasional group in the company:
engineers on every team, product managers, support, whoever wrote the feature, and the person who noticed a broken
sentence on their way past. Most of them open a documentation pull request a handful of times a year.

So the preview, the thing meant to save the reviewer ten minutes, often started with a request instead. A developer
pushes a fix, the build doesn't run, and they ask to be added. Somebody adds them. A week later a different
contributor asks again. Nothing was broken and nobody was wrong. It was just friction in exactly the step that was
supposed to have none, and friction is what turns "click and look" back into "read the diff and approve".

### We already had a way to publish a page

NetFoundry builds [zrok](https://zrok.io), which publishes a service at a URL without opening a port. A preview is
a directory of static files, and putting a directory of static files at a URL is precisely what zrok is for. So we
asked ourselves why anybody should have to join anything for a pull request to get built and shown.

With docpreview, the build runs on our own VM for whoever opens the pull request, and each preview is its own zrok
share. Pull requests from forks are the one deliberate exception: they're refused, because building one runs a
stranger's scripts on our build host. The reviewer clicks the link and reads the page. When a preview shouldn't be
open to anyone holding the link, zrok can put Google or GitHub sign-in in front of it, limited to your own email
domain, which uses an identity the reviewer already has. There are stricter options as well, covered below.

What we pay for is one VM, and that doesn't change whether five people or five hundred are contributing.

The private source mattered too. Our published documentation is public, but the source isn't always. Running the
build ourselves means the clone and the build happen on a machine we already trust, and the only thing that goes
out is the built site, which was going to be public anyway.

Bitbucket was the other half. Some of our private documentation lives there, and that's the subject of the next
post in this series.

## What we built

docpreview is one Go binary. A pull request arrives as a webhook, and it clones the branch, detects the
framework, runs the build, publishes the output at a URL, and posts a comment with the link.

Push again and the same comment updates rather than adding a new one. Close the pull request and the
preview goes away.

It stores its state in one sqlite file and its credentials in one encrypted file, and it runs wherever you
run things. A small VM is plenty.

There is no cloud account in the middle of any of that.

## The details that took the longest

Two are worth naming, because they are the difference between a demo and something you leave running.

**Superseding.** A reviewer fixing typos pushes five commits in two minutes. Building all five wastes four
builds and publishes four previews nobody will look at before the fifth replaces them. So a newer push
cancels the build already running for that pull request and replaces any build still queued for it. At
most one pending job exists per pull request at any moment.

The subtle half is that a superseded build must not report. Without that rule, a build cancelled halfway
finishes late, fails, and overwrites a comment that already said "ready" with a failure — for a preview
that is sitting there working.

**Refusing to publish a broken preview.** Before publishing, docpreview reads the built `index.html`,
works out which base URL the site was actually built for, and compares it to where the preview is about to
be served. When they disagree it refuses, and the error names both values and the two ways to fix it.

That check exists because the failure it prevents is invisible. A preview that renders as unstyled text
looks like a bug in the preview system, and the person who reports it is never the person who set
`baseUrl`.

## Who gets to open the link

This is where it started for us.

A preview URL is a decision about who can read unreleased documentation, and the right answer differs per
team. So the way a preview is published is one setting with four values.

| | Reachable by | Public surface |
|---|---|---|
| `local` | you, on loopback | none |
| `zrok2` | anyone with the link, or an OAuth-gated subset | yes |
| `frontdoor` | whoever your identity provider admits, with MFA enforced | yes |
| `ziti` | only a machine running a tunneler with an enrolled identity | **none at all** |

That last row is the interesting one. Published over an OpenZiti overlay, a preview has no public surface
whatsoever. The hostname does not resolve. The address is not routable. There is no port to scan, and no
login page to attack, because there is nothing listening that an unenrolled machine can reach.

Nothing above that setting changes when you switch. Not the queue, not the builder, not the comment. The
third post in this series is about choosing among those four.

## Try it without committing to anything

Creating a GitHub App is a ten-minute detour through a web form, and nobody should have to take it on the
strength of a README. So the binary will publish a directory without one:

```
docpreview preview -build ./www
```

That exercises the build, the base URL check, the exposer and the static server. Everything except the
webhook and the comment.

docpreview is at [github.com/openziti-test-kitchen/docpreview](https://github.com/openziti-test-kitchen/docpreview),
and it previews its own documentation, which is the only test that has caught anything embarrassing before
a reader did.

The whole thing costs one VM, and every contributor gets a preview.
