# Demoing docpreview, and making it look like it belongs to somebody

Two related jobs. The first is what a stranger sees on a pull request, which is the App's name, avatar and the
comment it writes. The second is what to show somebody who has two minutes.

This is a working document, not a description of code. Delete the parts that get done.

## Part 1 — The App, so the comment looks deliberate

The comment on a pull request is the whole product for everyone who is not the operator. It carries the App's
avatar and name, and the current one is a test App called `docpreview-dovholuknf-test` with the default
identicon — which reads as somebody's experiment, because it is.

### Create the real App

**Owner: the NetFoundry organization**, not a personal account. An App owned by a person cannot be transferred
without the receiving org accepting it, and the account that owns it is the account that can rotate its key.

GitHub → the organization → **Settings** → **Developer settings** → **GitHub Apps** → **New GitHub App**.

| Field | Value | Why |
|---|---|---|
| **Name** | `docpreview` if it is free, else `docpreview by NetFoundry` | It is the comment author. Global namespace, so the plain one may be taken |
| **Description** | `Documentation previews for pull requests. Builds the docs on every push and comments with a URL. Self-hosted, over zrok.` | Shown on the install screen, which is the one moment somebody decides whether to trust it |
| **Homepage URL** | the published docs site | Not the repository. Somebody clicking this wants to know what it does, not to read Go |
| **Webhook URL** | your tunnel's `/webhook/github` | |
| **Webhook secret** | generated in docpreview, pasted here | See the [App runbook](../www/docs/runbooks/github-app.md) |

Permissions and events are in the runbook; nothing here changes them.

### The avatar

The one thing that makes it look like a product rather than a script. 200×200 minimum, square, no transparency —
GitHub composites it onto white in some places and dark in others, so a transparent PNG with a dark mark
disappears on one of them.

What it should carry, in order of importance:

1. **Legible at 20 px.** That is the size on a pull request comment, and it is the only size most people ever see
   it at. Anything with a word in it is a grey smudge.
2. **A NetFoundry tie.** The house palette, or the mark, so it reads as coming from somewhere rather than from
   nobody. `www/static/img` already has the brand assets the docs site uses.
3. **A hint of what it is.** A document with a link, an eye over a page, a branch with a page on it — something
   about *previewing docs*, not a generic cloud.

Not a zrok logo. zrok is how the URL gets out and that is worth saying in the description and the docs; putting
somebody else's mark on the avatar makes it look like a zrok product.

### Where else the branding shows

| Surface | What to set |
|---|---|
| The pull request comment | Avatar and name, from the App |
| The **Checks** tab | The check run is named by the App too |
| The install screen | Name, description, homepage — the trust moment |
| The preview URL | `docpreview.shares.zrok.io` today. An owned subdomain is a zrok account-level setting; see `docs/design/19-zrok-namespacing.md` |
| The dashboard | Whatever the operator sees, which nobody outside the team does |

### Bitbucket has no App, and the token's name is the author

Worth stating because it is invisible until it is embarrassing: Bitbucket posts the comment as the **access
token**, and shows the token's name beside it on every pull request. A token called `mytoken2` says that on every
documentation review the team does, and renaming means creating a new token and re-storing it.

Call it `docpreview`. This is now in the [Bitbucket runbook](../www/docs/runbooks/bitbucket.md).

## Part 2 — The demo

### What actually convinces people

Not a tour of the dashboard. The dashboard is for the person running it, and showing it first invites "so I have
to run a server?" before they have seen why they would want to.

**The pull request comment is the demo.** Everything else is supporting material.

### The two-link version, for a message

Send exactly this, and nothing else:

> Docs previews on every PR — here's one: `<link to an open pull request>`
> The preview it built: `<the URL from that comment>`

If they click the second link and see the docs render, they are sold or they are not, and it took eleven seconds.

**Both links must survive being clicked next week.** They point at whatever machine is running the daemon, so
this only works once it lives on a VM rather than a laptop. That is the strongest argument for doing the move
before showing anybody.

### The ninety-second version, in person

1. **Open a pull request that changes one line of a doc.** Real repository, real change, nothing staged.
2. **Say nothing while it builds.** The comment appears within a few seconds saying Queued, then Building. The
   silence is the point: nobody configured anything between step 1 and now.
3. **Click the preview URL** when it goes green. The docs render, styled, at a public address.
4. **Push a second commit.** The same comment edits itself in place — it does not post a second one. That is the
   detail people notice and remember.
5. **Close the pull request.** The preview is withdrawn and the comment says so.

Total configuration shown: none. That is the argument.

### What to have ready before demoing

| | |
|---|---|
| A repository with a real docs site | `www/` in this repository is the obvious one |
| An open pull request already in the ready state | So step 1 has a fallback if the network is slow |
| The daemon on a machine that is not your laptop | Or every link in the room dies when you close it |
| A second commit prepared | For step 4, which is the part that lands |
| The branch preview URL | The docs site, permanently published by the tool it documents |

### Do not

- **Record a video.** It dates the moment the UI changes, and this UI changes weekly.
- **Lead with the architecture.** Nobody adopting a preview builder cares how the exposer abstraction works until
  they have decided they want previews.
- **Demo the local exposer.** A `127.0.0.1` URL cannot be clicked by the person you are showing.

## Part 3 — A carousel on the docs landing page

Better than a video, for the reason above: it is HTML, it is in the repository, and a UI change is a screenshot
swap rather than a re-record. It also works with the sound off, in a search result, and on a phone.

### The frames

Four, in the order the story happens. Each is one screenshot plus one line of caption — the caption carries the
meaning, the screenshot proves it.

| # | Screenshot | Caption |
|---|---|---|
| 1 | The pull request, with the comment showing **Building** | "Open a pull request. Nothing else to do." |
| 2 | The same comment, **Ready**, with the URL | "A preview URL, a few seconds later." |
| 3 | The rendered docs site at that URL | "The real site, built from the branch." |
| 4 | The same comment after a second push, still one comment | "Push again and the comment updates. It never posts twice." |

An optional fifth, for the operator rather than the reviewer: the dashboard with several previews and a live build
log. Last, because it answers a question the first four raise rather than opening with it.

### How to build it

`www/src/pages/index.tsx` already has section components and its own CSS module, so this is one more:

- A `<Carousel>` with the four frames as an array, dots underneath, and arrow keys bound.
- **No autoplay.** A landing page that moves while somebody is reading it is a landing page they leave. If it must
  advance on its own, stop on hover, on focus, and under `prefers-reduced-motion`.
- Screenshots in `www/static/img/demo/`, at 2× for retina, with the real repository names left visible — a
  screenshot with `acme/docs` in it looks like a mockup, and this one does not have to be.
- Alt text that says what happened, not "screenshot 1". It is the only version a screen reader gets.
- Each frame's caption in the DOM as text rather than baked into the image, so the page is searchable and
  translatable.

**The frames must be retakeable in one sitting.** Note the repository and pull request they came from in
`www/static/img/demo/README.md`, or the day the comment format changes somebody has to invent a new demo instead
of reshooting this one.
