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

## The hosted answer, and where it runs out

Vercel and Netlify solved this years ago. Every pull request gets built, and a bot drops a link on it. It
works, and for a public repository publishing a public site it is hard to argue with.

Three things push against it.

Your source goes to somebody else's build machine, which runs the scripts in your `package.json`. For an
open source project that is nothing. For internal documentation it is a procurement conversation, and
sometimes the answer is no.

The second is that the further you get from GitHub, the thinner the support. On Bitbucket the preview
story is worse, and if the repository is private it gets worse again. That is a real gap and it is the
subject of the next post in this series.

The third is the one that decided it for us.

### You pay per person who edits documentation

The hosted platforms bill per member. That model fits a product team, where the people who need an account
are the people who ship the application, and the number is stable.

Documentation has the opposite shape. The people who edit it are the widest and most occasional group in
the company. Engineers on every team, product managers, support, whoever wrote the feature, and the person
who noticed a broken sentence on their way past. Most of them contribute a handful of times a year.

Charge per seat for that population and the arithmetic turns hostile fast. Thirty occasional contributors
at a typical per-member rate runs to hundreds of dollars a month, several thousand a year, to review
documentation that is going to be published publicly anyway. Worse, the pressure it creates is exactly
backwards. The obvious way to control the bill is to reduce the number of people who can open a
documentation pull request, and the person you price out is the one who spotted the mistake.

Then there is what it does to the pull request itself.

A contributor without a seat opens a documentation change, and the deployment check goes red. Not skipped,
not neutral. Failed, in the same list and the same colour as a broken build and a failing test, and
everyone watching the repository gets the mail about it.

Usually it stops there, and that is the complaint. The change is fine, the merge goes through, and a red
mark sits on the record meaning nothing. On a repository configured to require every check before merging,
it stops being noise and holds the pull request shut.

Either way the contributor cannot clear it. There is nothing to fix on the branch, no command to run, no
amount of care that turns it green, because what the check reports is that they do not hold a license.

For a first-time contributor that is their whole experience of your project. They fixed a sentence, and
the system told them in the strongest signal it has that they got it wrong.

The slower damage is what it teaches everyone else. Once a red check routinely means nothing, people stop
reading the checks, and the one that was about to tell them something real gets waved through with the
rest.

We wanted the opposite property: anybody can open a pull request, every pull request gets a preview, and
the cost of the next contributor is zero. What we pay for is one VM. That number does not move whether
five people or five hundred are editing.

A docpreview build reports what the build did. There is no licensing state it can fail on, because there
are no licenses.

We had a third reason, and it is the one that made the tradeoff obvious. Our published documentation is
public. The source is not always. The thing we needed to show a reviewer was already destined to be a
public web page, so publishing a preview of it costs us nothing in exposure. The source never has to leave.

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

This is the part we did not expect to care about as much as we do.

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

The whole thing costs one VM, and every contributor after the first is free.
