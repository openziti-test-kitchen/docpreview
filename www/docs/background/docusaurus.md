---
id: docusaurus
title: Docusaurus, in the parts that matter here
sidebar_position: 3
---

# Docusaurus, in the parts that matter here

[Docusaurus](https://docusaurus.io/) is a static site generator built by Meta and aimed squarely at
documentation. You write Markdown or MDX; it produces a React-powered site with a sidebar, versioning,
search, dark mode, and i18n. This site is one.

docpreview is not Docusaurus-specific — anything that turns a repository into a directory of static files
works — but Docusaurus is the default assumption, so these are the parts worth knowing.

## The build

```bash
npm install     # or npm ci, if there is a lockfile
npm run build   # emits static files into build/
npm run serve   # serves build/ locally, for a sanity check
```

`npm run build` produces `build/`: HTML, a hashed CSS bundle, hashed JS chunks, and copied static assets.
Nothing in that directory needs Node to serve it. docpreview runs the first two commands and then serves the
third's output itself.

## `url` and `baseUrl`

Two required fields in `docusaurus.config.ts`, and the source of essentially every self-hosted preview
problem.

```ts
url: 'https://example.com',   // the origin
baseUrl: '/my-project/',      // the path prefix, with both slashes
```

For a site at `https://example.com/my-project/`, that is the split. For a site at the root of its own
hostname, `baseUrl` is `/`.

**`baseUrl` is compiled in.** Every `<link href>`, every `<script src>`, every internal link in the output is
written with that prefix already applied. It is not read at runtime. It cannot be overridden by a header, a
reverse proxy, or a `<base>` tag.

This collides with previews immediately. The commonest reason a repository has a non-root `baseUrl` is GitHub
Pages, which serves projects at `https://org.github.io/repo/`. A preview served at the root of a zrok hostname
needs `/`. Same source tree, two different builds.

## Making it configurable

One line, in `docusaurus.config.ts`:

```ts
const config: Config = {
  baseUrl: process.env.DOCUSAURUS_BASE_URL ?? '/my-project/',
  // ...
};
```

Existing deployments keep the hardcoded default; docpreview overrides it per build. This site does exactly
that — see [`www/docusaurus.config.ts`](https://github.com/openziti-test-kitchen/docpreview/blob/main/www/docusaurus.config.ts).

If you cannot change the config — someone else's repository, a vendored theme — set `build.base_url` in
[`.docpreview.yml`](../reference/repo-config.md) to whatever the config hardcodes. docpreview will serve the
preview under that prefix instead of the root, and redirect the bare origin to it so the link still works.

Either way, docpreview checks the built `index.html` before publishing and refuses to serve a site whose asset
URLs disagree with where it is about to be mounted. The reasoning is in
[Troubleshooting](../troubleshooting.md).

## `onBrokenLinks`

```ts
onBrokenLinks: 'throw',
```

Docusaurus can fail the build on internal links that go nowhere. Leave this on. Catching a broken cross-
reference in a preview build is the single highest-value thing this whole system does — it is a class of
error that reviewers reliably miss and readers reliably hit.

## Sources

- [Docusaurus deployment guide](https://docusaurus.io/docs/deployment)
- [`docusaurus.config.js` reference](https://docusaurus.io/docs/api/docusaurus-config)
