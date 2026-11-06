---
id: jamstack
title: Jamstack, and why a preview is easy
sidebar_position: 2
---

# Jamstack, and why a preview is easy

**Jamstack** stands for JavaScript, APIs, and Markup. It is a way of building sites, not a product, and it comes down
to three commitments.

**Pre-rendering.** Pages are generated into static HTML at build time, not assembled per request. A visitor receives a
file that already exists.

**Decoupling.** The front end is built independently of any back end. It is a directory of files. It does not know what
produced it and does not talk to it at runtime.

**API-driven enhancement.** Search, forms, sign-in and anything else dynamic run in the browser against APIs or
serverless functions. They sit on top of the static markup instead of inside a server that renders it.

Static site generators make this practical: Docusaurus, Hugo, Jekyll, Gatsby, Next.js in export mode. Feed them content
and templates, and get a directory of HTML.

## Why this matters for previews

A docs site is the purest Jamstack case there is. It has no user accounts, no writes, no personalization, no
request-time state. A single-page app such as the Ziti admin console is close behind: its build output is static files,
and everything dynamic happens in the browser against an API.

So a preview of either is **a directory of files, behind a URL**.

That is what keeps NetFoundry Preview small. Compare the two shapes:

| If previews needed a running application | Because they do not |
|---|---|
| One process per preview, holding memory | One Go process, N directories on disk |
| A port per preview, and a port allocator | Zero ports under zrok, an overlay listener per preview |
| Health checks and restart policies | Nothing to restart |
| Database and service dependencies per preview | None |
| Twenty open PRs = twenty Node processes | Twenty open PRs = twenty folders |

The obvious design runs `docusaurus serve` once per preview, and it is a trap. Each one is a Node process holding
hundreds of megabytes, listening on a port you have to allocate, supervise and reclaim. All for content that does not
change until the next rebuild.

NetFoundry Preview serves the built directory with Go's `http.FileServer` instead. The whole preview server is one
short file, and most of it decides what to do about 404s.

## The one place it bites back

Pre-rendering freezes decisions at build time, including the path the site will be served from. Docusaurus writes
`baseUrl` into every emitted `href` and `src`. Build for `/my-project/`, serve at `/`, and every asset 404s while
`index.html` loads fine.

The site gave up the runtime logic that would have let it work out its own URL. That is the same trade that makes a
preview cheap. See [Troubleshooting](../troubleshooting.md) for how NetFoundry Preview handles it.

## Sources

- [Jamstack (Wikipedia)](https://en.wikipedia.org/wiki/JAMstack)
- [What is Jamstack? (Sanity)](https://www.sanity.io/glossary/jamstack)
- [The Evolution of Jamstack (Smashing Magazine)](https://www.smashingmagazine.com/2021/05/evolution-jamstack/)
