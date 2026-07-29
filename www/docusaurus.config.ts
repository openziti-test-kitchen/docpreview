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

const config: Config = {
  title: 'docpreview',
  tagline: 'Documentation previews for pull requests, running anywhere',
  favicon: 'img/favicon.ico',

  future: {
    v4: true,
  },

  url: siteUrl,
  baseUrl,

  organizationName: 'netfoundry',
  projectName: 'docpreview',

  // Broken links in a preview are worth failing the build over: the whole
  // point of the preview is to catch them before a reviewer does.
  onBrokenLinks: 'throw',

  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          routeBasePath: 'docs',
          editUrl: 'https://github.com/netfoundry/docpreview/tree/main/www/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/docusaurus-social-card.jpg',
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
          href: 'https://github.com/netfoundry/docpreview',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Start here',
          items: [
            {label: 'What this is', to: '/docs/intro'},
            {label: 'Quickstart', to: '/docs/quickstart'},
            {label: 'GitHub App runbook', to: '/docs/runbooks/github-app'},
          ],
        },
        {
          title: 'Reference',
          items: [
            {label: 'Architecture', to: '/docs/architecture'},
            {label: 'Configuration', to: '/docs/reference/configuration'},
            {label: 'Exposers', to: '/docs/exposers'},
          ],
        },
        {
          title: 'Background',
          items: [
            {label: 'zrok', href: 'https://docs.zrok.io/'},
            {label: 'NetFoundry Frontdoor', href: 'https://netfoundry.io/docs/frontdoor/intro/'},
            {label: 'Docusaurus', href: 'https://docusaurus.io/'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} NetFoundry. Built with Docusaurus.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'powershell', 'yaml', 'json', 'go'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
