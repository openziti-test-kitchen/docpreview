import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import userDocLinks from './src/remark/userDocLinks';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

// The single most important line in this file for preview builds.
//
// Docusaurus bakes baseUrl into every emitted href and src at build time. A
// site whose baseUrl is hardcoded can only ever be served at one path: build it
// for '/my-project/' and serve it at '/', and index.html loads while every
// stylesheet and script 404s. The page renders as an unstyled wall of text and
// nothing in the build log says why.
//
// Reading it from the environment makes the same source tree buildable for any
// mount point. nfpreview sets DOCUSAURUS_BASE_URL from build.base_url and then
// serves the output at exactly that prefix, so the two cannot drift apart. It
// also verifies the built output agrees before publishing, and refuses to
// publish a preview it knows is broken.
const baseUrl = process.env.DOCUSAURUS_BASE_URL ?? '/';
const siteUrl = process.env.DOCUSAURUS_URL ?? 'https://nfpreview.example.com';

// The repository this site documents. Named once because it appears in the star
// banner, the footer, the navbar and the docs edit link, and a wrong one is the
// kind of thing nobody notices until a reader clicks it.
const repoUrl = 'https://github.com/openziti-test-kitchen/docpreview';

// The NetFoundry footer renders its link columns as plain <a href>, not as
// Docusaurus <Link>, so nothing applies baseUrl for us. Deriving the href from
// the same `baseUrl` constant above keeps footer links working when nfpreview
// mounts this site under a preview prefix instead of at '/'. baseUrl always
// ends in a slash, hence no separator here.
const sitePath = (p: string) => `${baseUrl}${p}`;

const config: Config = {
  title: 'NetFoundry Preview',
  tagline: 'Pull request previews for any static site, running anywhere',
  // The App's logo (img/netfoundry-preview-logo.svg), copied as-is. img/favicon.ico is
  // still the older page icon: .gitattributes marks every file text with eol=lf, which
  // rewrites bytes in a binary .ico, so a regenerated one waits on a `*.ico binary` rule.
  favicon: 'img/favicon.svg',

  future: {
    v4: true,
  },

  url: siteUrl,
  baseUrl,

  // The repository the site is published from. These two name the GitHub Pages target, so they
  // have to match repoUrl above — a mismatch points the deploy at a repository that is not this one.
  organizationName: 'openziti-test-kitchen',
  projectName: 'docpreview',

  // Broken links in a preview are worth failing the build over: the whole
  // point of the preview is to catch them before a reviewer does.
  onBrokenLinks: 'throw',

  markdown: {
    // Without this a ```mermaid fence renders as a code block showing the diagram's source,
    // which looks like a page that forgot to finish rather than like a missing plugin. It needs
    // @docusaurus/theme-mermaid registered below as well — one without the other does nothing.
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  // The shared NetFoundry theme. It overrides @theme/Layout — so every page,
  // including the landing page, picks up the NetFoundry navbar, footer and star
  // banner without importing anything — and injects its own CSS (brand colours,
  // the --nf-* custom properties, the code-block palette) via getClientModules.
  // Registered here rather than in the preset so it loads after theme-classic
  // and its component overrides win.
  // theme-mermaid first, so the NetFoundry theme's component overrides still win where the two
  // touch the same component.
  themes: ['@docusaurus/theme-mermaid', '@netfoundry/docusaurus-theme'],

  // The design documents live in docs/design at the repository root, beside the code they describe, and
  // are written for someone changing that code. A second docs instance publishes them from where they
  // are: a copy under www/ would be a second copy, and the one nobody edits goes stale.
  // Inter and JetBrains Mono, self-hosted so the site makes no request to a font CDN. See src/fonts.ts
  // for why this is a client module and not an @import in custom.css.
  clientModules: ['./src/fonts.ts'],

  plugins: [
    [
      '@docusaurus/plugin-content-docs',
      {
        id: 'design',
        path: '../docs/design',
        routeBasePath: 'design',
        sidebarPath: './sidebarsDesign.ts',
        // A function, because the default joins editUrl to a path relative to www/, which for a
        // directory outside it starts with ../ and points at nothing.
        editUrl: ({docPath}: {docPath: string}) => `${repoUrl}/blob/main/docs/design/${docPath}`,
        beforeDefaultRemarkPlugins: [userDocLinks],
      },
    ],
  ],

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          routeBasePath: 'docs',
          editUrl: `${repoUrl}/tree/main/www/`,
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    // Configuration read by @netfoundry/docusaurus-theme. Only the keys the
    // theme actually declares (see its options.ts) do anything here; unknown
    // keys are silently ignored, which is why this block stays minimal.
    netfoundry: {
      // Path-aware: one entry with no pathPrefix means the banner shows on
      // every page, which is what we want on a single-product site.
      starBanners: [
        {repoUrl, label: 'Star NetFoundry Preview on GitHub'},
      ],
      footer: {
        description:
          'Pull request previews for any static site. One Go binary, running on your hardware, ' +
          'publishing over an OpenZiti overlay.',
        socialProps: {
          githubUrl: repoUrl,
        },
        // The theme's defaults for these three columns point at the OpenZiti
        // docs, which do not exist on this site. Override rather than inherit.
        documentationLinks: [
          {href: sitePath('docs/intro'), label: 'What this is'},
          {href: sitePath('docs/quickstart'), label: 'Quickstart'},
          {href: sitePath('docs/architecture'), label: 'Architecture'},
          {href: sitePath('docs/reference/configuration'), label: 'Configuration'},
          {href: sitePath('docs/reference/security'), label: 'Security model'},
          {href: sitePath('design/'), label: 'Design documents'},
        ],
        communityLinks: [
          {href: repoUrl, label: 'GitHub'},
          {href: `${repoUrl}/tree/main/docs/blog`, label: 'Blog drafts'},
          {href: `${repoUrl}/issues`, label: 'Issues'},
          {href: 'https://openziti.discourse.group/', label: 'Discourse forum'},
        ],
        resourceLinks: [
          {href: sitePath('docs/exposers'), label: 'Exposers'},
          {href: 'https://docs.zrok.io/', label: 'zrok'},
          {href: 'https://netfoundry.io/docs/frontdoor/intro/', label: 'NetFoundry Frontdoor'},
        ],
      },
    },
    // The social card: og:image and twitter:image on every page, 1200x630. A raster, because Open Graph
    // and every consumer of it ignore SVG. Rendered from social/card.html by social/render.mjs, so edit
    // the HTML and re-render rather than editing the PNG. Docusaurus makes it absolute from url + baseUrl,
    // which is why DOCUSAURUS_URL has to be the real address for a published build.
    image: 'img/social-card.png',
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'NetFoundry Preview',
      logo: {
        alt: 'NetFoundry Preview',
        src: 'img/netfoundry-preview-logo.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
        },
        {
          type: 'doc',
          docId: 'quickstart',
          position: 'left',
          label: 'Quickstart',
        },
        // Two dropdowns because the sidebar already lists everything; these are the pages people come back
        // for, one click from any page including the landing page, which has no sidebar.
        {
          type: 'dropdown',
          label: 'Guides',
          position: 'left',
          items: [
            {type: 'doc', docId: 'guides/github-app', label: 'Create the GitHub App'},
            {type: 'doc', docId: 'guides/bitbucket', label: 'Connect Bitbucket'},
            {type: 'doc', docId: 'guides/webhook-tunnel', label: 'Expose the webhook'},
            {type: 'doc', docId: 'guides/zrok2', label: 'Publish with zrok v2'},
            {type: 'doc', docId: 'guides/frontdoor', label: 'Publish with Frontdoor'},
            {type: 'doc', docId: 'guides/linux-service', label: 'Run as a Linux service'},
            {type: 'doc', docId: 'guides/container', label: 'Run in a container'},
            {type: 'doc', docId: 'troubleshooting', label: 'Troubleshooting'},
          ],
        },
        {
          type: 'dropdown',
          label: 'Reference',
          position: 'left',
          items: [
            {type: 'doc', docId: 'reference/configuration', label: 'Server configuration'},
            {type: 'doc', docId: 'reference/repo-config', label: 'Repository configuration'},
            {type: 'doc', docId: 'reference/cli', label: 'CLI'},
            {type: 'doc', docId: 'exposers', label: 'Exposers'},
            {type: 'doc', docId: 'reference/security', label: 'Security model'},
            {type: 'doc', docId: 'architecture', label: 'Architecture'},
          ],
        },
        {
          type: 'docSidebar',
          docsPluginId: 'design',
          sidebarId: 'designSidebar',
          position: 'left',
          label: 'Design',
        },
        // The posts in docs/blog are drafts that say they are not linked from this site until they are
        // published, so this points at them on GitHub rather than publishing them here.
        {
          href: `${repoUrl}/tree/main/docs/blog`,
          label: 'Blog',
          position: 'left',
        },
        {
          href: repoUrl,
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    // No `footer` key here on purpose. The NetFoundry theme replaces
    // @theme/Layout with one that renders its own footer from
    // themeConfig.netfoundry.footer above, so Infima's footer config would be
    // dead weight that reads as if it were live.
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'powershell', 'yaml', 'json', 'go'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
