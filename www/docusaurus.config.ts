import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

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
// mount point. docpreview sets DOCUSAURUS_BASE_URL from build.base_url and then
// serves the output at exactly that prefix, so the two cannot drift apart. It
// also verifies the built output agrees before publishing, and refuses to
// publish a preview it knows is broken.
const baseUrl = process.env.DOCUSAURUS_BASE_URL ?? '/';
const siteUrl = process.env.DOCUSAURUS_URL ?? 'https://docpreview.example.com';

// The repository this site documents. Named once because it appears in the star
// banner, the footer, the navbar and the docs edit link, and a wrong one is the
// kind of thing nobody notices until a reader clicks it.
const repoUrl = 'https://github.com/openziti-test-kitchen/docpreview';

// The NetFoundry footer renders its link columns as plain <a href>, not as
// Docusaurus <Link>, so nothing applies baseUrl for us. Deriving the href from
// the same `baseUrl` constant above keeps footer links working when docpreview
// mounts this site under a preview prefix instead of at '/'. baseUrl always
// ends in a slash, hence no separator here.
const sitePath = (p: string) => `${baseUrl}${p}`;

const config: Config = {
  title: 'docpreview',
  tagline: 'Documentation previews for pull requests, running anywhere',
  // SVG rather than the .ico it replaced: one file, sharp at every size, and it can
  // be edited. Every browser this documentation targets has supported an SVG favicon
  // for years. img/favicon.ico is deliberately left in place for anything that asks
  // for /favicon.ico by convention rather than by reading the markup.
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
        {repoUrl, label: 'Star docpreview on GitHub'},
      ],
      footer: {
        description:
          'Documentation previews for pull requests. One Go binary, running on your hardware, ' +
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
        ],
        communityLinks: [
          {href: repoUrl, label: 'GitHub'},
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
    // No social card. The one here was Docusaurus's own artwork, so every link
    // preview of this site advertised a dragon that has nothing to do with it —
    // worse than no image. A replacement has to be a raster: Open Graph and every
    // consumer of it ignore SVG. See TODO.md.
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'docpreview',
      logo: {
        alt: 'docpreview',
        src: 'img/logo.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
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
