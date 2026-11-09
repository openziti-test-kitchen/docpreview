---
id: docusaurus
title: Docusaurus, in the parts that matter here
sidebar_position: 3
---

# Docusaurus, in the parts that matter here

[Docusaurus](https://docusaurus.io/) is a static site generator from Meta, built for documentation. You write Markdown
or MDX. It produces a React site with a sidebar, versioning, search, dark mode, and i18n. This site is one.

NetFoundry Preview works with anything that builds a repository into a directory of static files. Its defaults
(`npm run build`, output in `build/`) match Docusaurus, so these are the parts you need to know.

## The build

```bash
npm install     # or npm ci, if there is a lockfile
npm run build   # emits static files into build/
npm run serve   # serves build/ locally, for a sanity check
```

`npm run build` produces `build/`: HTML, a hashed CSS bundle, hashed JS chunks, and copied static assets. Nothing in
that directory needs Node to serve it. NetFoundry Preview runs the install (npm, yarn or pnpm, picked from the
lockfile) and the build, then serves `build/` itself.

## `url` and `baseUrl`

These two required fields in `docusaurus.config.ts` cause almost every self-hosted preview problem.

```ts
url: 'https://example.com',   // the origin
baseUrl: '/my-project/',      // the path prefix, with both slashes
```

A site at `https://example.com/my-project/` splits that way. A site at the root of its own hostname has `baseUrl: '/'`.

**`baseUrl` is compiled in.** Every `<link href>`, every `<script src>`, every internal link in the output is written
with that prefix already applied. It is not read at runtime. No header, reverse proxy or `<base>` tag can override it.

That collides with previews right away. The commonest reason for a non-root `baseUrl` is GitHub Pages, which serves
projects at `https://org.github.io/repo/`. A preview served at the root of a zrok hostname needs `/`. Same source tree,
two different builds.

## Making it configurable

One line, in `docusaurus.config.ts`:

```ts
const config: Config = {
  baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
  // ...
};
```

Existing deployments keep the hardcoded default. NetFoundry Preview sets `DOCUSAURUS_BASE_URL` on every build. This
site does the same in
[`www/docusaurus.config.ts`](https://github.com/openziti-test-kitchen/docpreview/blob/main/www/docusaurus.config.ts).

Sometimes you cannot change the config, because the repository or the theme belongs to someone else. Then set
`build.base_url` in [`.nfpreview.yml`](../reference/repo-config.md) to whatever the config hardcodes. The preview is
served under that prefix instead of the root, and the bare origin redirects to it so the link still works.

Either way, NetFoundry Preview checks the built `index.html` before publishing. If its asset URLs disagree with where
the site is about to be mounted, the build fails instead of serving a broken page. The reasoning is in
[Troubleshooting](../troubleshooting.md).

## `onBrokenLinks`

```ts
onBrokenLinks: 'throw',
```

Docusaurus can fail the build on internal links that go nowhere. Leave this on. A broken cross-reference is easy for a
reviewer to miss and certain to annoy a reader, and a preview build is the cheapest place to catch it.

## Sources

- [Docusaurus deployment guide](https://docusaurus.io/docs/deployment)
- [`docusaurus.config.js` reference](https://docusaurus.io/docs/api/docusaurus-config)
