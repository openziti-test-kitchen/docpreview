---
id: jamstack
title: Jamstack, and why a preview is easy
sidebar_position: 2
---

# Jamstack, and why a preview is easy

**Jamstack** stands for JavaScript, APIs, and Markup. It is an architectural position rather than a product,
and it comes down to three commitments.

**Pre-rendering.** Pages are generated into static HTML at build time, not assembled per request. A visitor
receives a file that already exists.

**Decoupling.** The front end is built independently of any back end. It is a directory of files; it does not
know what produced it and does not talk to it at runtime.

**API-driven enhancement.** Anything genuinely dynamic — search, forms, authentication — happens in the
browser against APIs or serverless functions, layered onto the static markup rather than baked into a server
that renders it.

Static site generators are the tool that makes this practical: Docusaurus, Hugo, Jekyll, Gatsby, Next.js in
export mode. Feed them content and templates; get a directory of HTML.

## Why this matters for previews

A documentation site is the purest Jamstack case there is. It has no user accounts, no writes, no
personalization, no request-time state. It is text, and text does not need a server.

Which means a preview of a documentation site is: **a directory of files, behind a URL**.

That single observation is what makes docpreview small. Compare the two shapes:

| If previews needed a running application | Because they do not |
|---|---|
| One process per preview, holding memory | One Go process, N directories on disk |
| A port per preview, and a port allocator | Zero ports under zrok — an overlay listener per preview |
| Health checks and restart policies | A file server cannot crash |
| Database and service dependencies per preview | None |
| Twenty open PRs = twenty Node processes | Twenty open PRs = twenty folders |

The alternative implementation — spawning `docusaurus serve` per preview — is the obvious one and it is a
trap. Each instance is a Node process with a resident set measured in hundreds of megabytes, listening on a
port you have to allocate, supervise, and reclaim. For content that will never change until the next rebuild.

docpreview serves the built directory with Go's `http.FileServer` instead. The entire preview server is about
a hundred lines, most of which is deciding what to do about 404s.

## The one place it bites back

Pre-rendering means decisions get frozen at build time — including where the site will be served from.
Docusaurus writes `baseUrl` into every emitted `href` and `src`. Build for `/my-project/`, serve at `/`, and
every asset 404s while `index.html` loads fine.

The dynamism you gave up to make previews cheap is exactly the dynamism that would have let the site figure
out its own URL at runtime. See [Troubleshooting](../troubleshooting.md) for what docpreview does about it.

## Sources

- [Jamstack (Wikipedia)](https://en.wikipedia.org/wiki/JAMstack)
- [What is Jamstack? (Sanity)](https://www.sanity.io/glossary/jamstack)
- [The Evolution of Jamstack (Smashing Magazine)](https://www.smashingmagazine.com/2021/05/evolution-jamstack/)
