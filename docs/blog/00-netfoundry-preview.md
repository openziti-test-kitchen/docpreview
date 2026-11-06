# Love Vercel previews? Keep them. Here's another way.

Vercel made something so good that it now feels like it was always there. You open a pull request, a bot comments
with a link, and the link is your site built from that branch. Push again and the same comment updates instead of
adding another. Merge or close, and the preview cleans itself up. Nobody has to clone anything, find the right Node
version, or start a dev server to see whether a page renders. Reviewers look at the page, not just the diff.

We loved that loop, and we still think it's the right way to review anything that builds to a site. Vercel defined
it, and much of what we built copies their design on purpose. The single comment edited in place is a good example:
it's the detail that keeps a busy pull request readable, and we took it straight from them.

If Vercel works for you, keep using it. It's a great product. This post is for anyone who wants another way to get
the same loop, and it explains why we wanted one.

The bot is called **netfoundry-preview**, and this is what it posts:

> **NetFoundry Preview**
>
> | Project | Status | Preview | Commit | Updated (UTC) |
> |---|---|---|---|---|
> | openziti-test-kitchen/docpreview | ✅ Ready · 41s | [Preview](https://docpreview-my-branch.shares.zrok.io/) | `a1b2c3d` | Nov 5, 2026 2:22pm |

The software behind it is [docpreview](https://github.com/openziti-test-kitchen/docpreview). It's a single Go binary
on a single VM. It's open source, and its [own documentation](https://openziti-test-kitchen.github.io/docpreview/)
is built and previewed by itself.

## The one thing we wanted to be different

Most of our documentation repositories are private. In our setup, Vercel would only build a pull request on a
private repository if its author was a member of our Vercel team. When somebody outside the team opened one, the
build was blocked until somebody added them. That's a sensible rule from Vercel's side, because a build runs code
and they want to know whose code it is. For us it was mostly friction. A developer would push a fix, the preview
wouldn't build, and they had to ask to be added. Then the next contributor would ask again.

Documentation contributors are exactly the people this happens to most. They come from every team, open a handful
of pull requests a year, and have no other reason to hold an account. [The first post](01-nobody-clones-the-repo.md)
covers why that group matters so much for docs.

We already had a way to put a site at a URL without anyone joining anything:
[zrok](https://zrok.io) ([source](https://github.com/openziti/zrok)), which runs on [OpenZiti](https://openziti.io)
([source](https://github.com/openziti/ziti)), NetFoundry's own open-source networking stack. So docpreview builds
the pull request on our own VM and publishes the result as its own zrok share. Anybody who can open a pull request
on the repository gets a preview, and the reviewer clicks the link and reads the page. The one exception is
deliberate: pull requests from forks are refused, because building one would run a stranger's scripts on our build
host.

When a preview shouldn't be public, the same setting chooses who can open it:

- an open link,
- a link gated by Google or GitHub OAuth and limited to your email domain,
- NetFoundry Frontdoor in front of your own identity provider,
- an OpenZiti service that only enrolled machines can reach at all, with no public DNS name and no port.

Switching between them is one line of config. [Post 3](03-preview-url-is-a-security-decision.md) covers how to
choose.

## What came with running it ourselves

Once the build runs on our own VM, a few things follow that we're glad of.

**Private source stays where it already lives.** The clone and the build happen on a machine we already trust, and
the only thing that leaves is the built site, and only when we publish it.

**The credentials stay on our hardware too.** On GitHub, the App we created asks for read access to contents, write
access to pull requests and checks, and nothing else, and it sees only the repositories it's installed on. On
Bitbucket, a repository access token is scoped to exactly one repository. Either credential lives in an
[age](https://age-encryption.org)-encrypted vault on the VM, and it's scrubbed from build output before any log
reaches a pull request.

**Bitbucket and GitHub, from one place.** Some of our private documentation lives on Bitbucket Cloud. One of those
repositories already had Vercel's Bitbucket integration attached, and it gave us an excellent working reference to
learn from. [Post 2 in this series](02-bitbucket-private-repos.md) covers what is actually different about
Bitbucket.

## What it does

A webhook arrives, and docpreview:

1. clones the branch,
2. checks whether the pull request touches the site at all, and skips it if not, so a README edit doesn't rebuild
   your docs,
3. runs the build, locally or in a container,
4. checks that the built site will actually render where it is about to be served,
5. publishes it at a stable per-branch URL, plus a pinned URL for every individual build, and
6. writes or edits the one comment, and a check run on GitHub.

A newer push cancels the build still running for that pull request, and a cancelled build never overwrites the
comment. State lives in one sqlite file. There's an operator dashboard with live build logs, behind its own login.

### Not only docs

Anything that builds to static files works. The project form has presets for Docusaurus, VitePress, VuePress, Astro
Starlight, MkDocs, Sphinx, mdBook, Antora, Retype, Astro, Eleventy, Hugo, Jekyll, Gatsby, Next.js static export, Nuxt
static, SvelteKit with the static adapter, and Vite. A preset only fills in the build command and output directory,
so anything else can be set by hand.

Two limits:

- **Static files only.** There's no server runtime, so no API routes, no on-request rendering, and no middleware.
- **No fallback to `index.html`.** A path that isn't a file gets the site's own `404.html`. A single-page app with
  client-side routing loads at its root, but a deep link like `/settings/profile` 404s on reload. Hash routing
  (`/#/settings/profile`) works.

If your preview needs a running server, Vercel does that well and docpreview doesn't try to.

## The trap every preview builder has to handle

Docusaurus bakes its base URL into every `href` and `src` at build time. Build a site for `/docs/` and serve it at
`/`, and `index.html` loads with every stylesheet and script returning 404. You get unstyled text, and nothing in
the build log explains it.

docpreview reads the built `index.html` before publishing, works out which base URL the site was really built for,
and refuses to publish if that doesn't match where the site is about to be mounted. The error names both values and
the two ways to fix it. [Post 5](05-what-it-took.md) explains how the check works and why the obvious version of it
is wrong.

We relearned this the hard way this week. We tried serving every preview from one zrok share under
`/preview/<name>/`, which is simpler to run than one share per preview. Then we pointed it at
[netfoundry/docusaurus-shared](https://github.com/netfoundry/docusaurus-shared), whose sites hardcode `/docs/`.
Under a path prefix, those sites cannot render. The check caught it and refused to publish, which beats a broken
page but still leaves a reviewer with nothing to look at. Asking every repository to make its base URL configurable
just to suit the previewer gets the dependency backwards, so we rolled it back. Each preview gets its own share
again and is served at `/` on its own hostname, which is where most sites expect to live. A site that genuinely
lives under a prefix declares it with `build.base_url` in `.docpreview.yml`, and docpreview builds and mounts it
there.

## Making the bot look like it belongs to you

Every comment the bot writes carries the App's name and avatar, so for everyone except the operator the App *is* the
product. Ours started as a personal test App, which put one person's face on every pull request. We transferred it to
the NetFoundry organization and renamed it `netfoundry-preview`. Its avatar is the
[NetFoundry mark](https://github.com/netfoundry/branding) with a small magnifier on it.

Two lessons from that, if you're setting up your own:

- **Transfer the App, don't recreate it.** A transfer keeps the App ID, private key, webhook secret and every
  installation, so docpreview needed no config change. A new App would also have stranded the old comments, because
  one App can't edit another App's comments, so every open pull request would have ended up with two.
- **Set the heading.** It defaults to **NetFoundry Preview** and is configured with `preview.title`. Comments are
  matched by a hidden marker, not by their heading, so changing the title on a running installation edits existing
  comments in place. On Bitbucket, where the comment author is the access token, name the token `docpreview`.

## Set it up yourself

First, publish something with no App and no webhook. You need Go 1.26+, git, Node 20+ (or Docker), and a
[zrok v2](https://docs.zrok.io/) account if you want a public URL. The `local` exposer needs no account, but its URL
only works on your own machine.

```powershell
git clone https://github.com/openziti-test-kitchen/docpreview
cd docpreview
go build -o bin/ ./cmd/docpreview

docpreview init                                        # one question: which exposer
docpreview doctor                                      # makes real calls, not just a config lint
docpreview preview -build -name my-first-preview ./www
```

That builds docpreview's own docs site and prints a URL. It runs the same build, base URL check and static server as
a real pull request, without the webhook and the comment. Point it at your own site's directory next, and press
Ctrl-C to withdraw the URL.

Then wire it to pull requests:

1. **Make a vault key** with `docpreview vault keygen`, and put a copy in a password manager. Without it, the vault
   cannot be decrypted.
2. **Expose the webhook** with `docpreview webhook-only`. It publishes the one route GitHub posts to, and not the
   dashboard or credential API. See [Expose the webhook][tunnel].
3. **Create a GitHub App** under your organization with Contents read, Pull requests and Checks read and write, and
   the Pull request event. Store its private key and webhook secret in the vault. Give it a real name and an avatar.
   See [Create the GitHub App][app].
4. **Install it** on the repositories you want, start `docpreview serve`, and **add a project** for each repository
   on the dashboard at `http://127.0.0.1:8471/projects`. A repository with no project is never built, even with the
   App installed. That's deliberate: a valid webhook signature proves GitHub sent the delivery, not that you chose to
   build that repository.

On Bitbucket, steps 3 and 4 become a repository access token and a webhook. See [Connect Bitbucket Cloud][bitbucket].
For something long-lived there are `.deb` and `.rpm` packages and a [Linux VM runbook][linux]. Use a VM rather than a
laptop, because preview links die when the machine serving them sleeps.

The full walkthrough is the [quickstart][quickstart].

## Where it stands

There's no high availability: one daemon on one VM is the deployment. The Frontdoor exposer is written but hasn't
been tested against a live tenant. The OpenZiti exposer keeps previews apart by the `Host` header, so any reviewer
with the reader attribute can reach every preview, not only the ones meant for them. Each of these is written up in
the repository's design docs, along with what it would take to fix.

What you get is the loop Vercel showed us all, running on hardware you control, on GitHub and Bitbucket, and a
preview for every contributor without adding anybody to anything first. Our thanks to Vercel for showing how good
pull request review can be. If you love it, keep it. If you want another way, start at
[github.com/openziti-test-kitchen/docpreview](https://github.com/openziti-test-kitchen/docpreview).

[tunnel]: https://openziti-test-kitchen.github.io/docpreview/docs/guides/webhook-tunnel
[app]: https://openziti-test-kitchen.github.io/docpreview/docs/guides/github-app
[bitbucket]: https://openziti-test-kitchen.github.io/docpreview/docs/guides/bitbucket
[linux]: https://openziti-test-kitchen.github.io/docpreview/docs/guides/linux-service
[quickstart]: https://openziti-test-kitchen.github.io/docpreview/docs/quickstart
