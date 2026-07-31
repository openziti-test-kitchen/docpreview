import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

// The sidebar is written by hand, and it is grouped by what the reader is trying to
// do rather than by where the files live.
//
// # Diátaxis, and why the four groups are not arbitrary
//
// A page fails when it tries to be two things at once. A tutorial that pauses to
// explain a design decision loses the person following it; a reference page that
// tells a story is one nobody can scan. So each group answers one need:
//
//   INTRO / GET STARTED   learning     "I have never run this. Walk me through it."
//   HOW-TO                a task       "I need to do a specific thing, now."
//   LEARN                 understanding "Why is it built like this?"
//   REFERENCE             information   "What is the exact name of that flag?"
//
// The practical value is editorial: when a page starts to sprawl, its group says
// which half does not belong and where that half goes.
//
// # The section headings
//
// `className: 'sidebar-title'` is the NetFoundry theme's own convention — a level-1
// link carrying it renders as a small-caps separator with `pointer-events: none`, so
// the `href` is never followed. That is why these are `#` and why they are not
// categories: a category is collapsible and clickable, and these are labels.
//
// Matching the theme rather than styling our own means the left nav here reads like
// the rest of the NetFoundry documentation, which is the point.
//
// # Files did not move
//
// Only the reading order changed. Moving pages on disk would change their URLs, and
// this documentation is already linked from pull request comments and runbooks. A
// sidebar is free to disagree with the directory layout, so it does.
const section = (label: string) => ({
  type: 'link' as const,
  label,
  href: '#',
  className: 'sidebar-title',
});

const sidebars: SidebarsConfig = {
  docsSidebar: [
    section('INTRO'),
    'intro',
    'quickstart',

    // Task-oriented. Every one of these has a human following it with a browser
    // open, so they are ordered by when that human needs them: connect the App,
    // reach the daemon, choose how previews are served, then what to do when it
    // breaks.
    section('HOW-TO'),
    'runbooks/github-app',
    // Was missing from the sidebar entirely — the page existed and nothing linked
    // to it, which for the one runbook covering the hardest step is the worst page
    // to lose.
    'runbooks/webhook-tunnel',
    'runbooks/zrok2',
    'runbooks/frontdoor',
    // Deployment, after the pages about getting one instance working: nobody containerises
    // or moves an installation they have not run yet.
    'runbooks/container',
    'runbooks/move-an-installation',
    'troubleshooting',

    // Understanding-oriented: read when deciding whether to use this, or before
    // changing it. Nothing here tells you to do anything.
    section('LEARN'),
    'architecture',
    'exposers',
    'local-platform',
    {
      type: 'category',
      label: 'Why these dependencies',
      collapsed: true,
      items: [
        'background/vercel',
        'background/jamstack',
        'background/docusaurus',
        'background/age',
      ],
    },
    {
      type: 'category',
      label: 'Research',
      collapsed: true,
      // Not features. Each of these says so at the top, and the grouping repeats it
      // — a reader who finds one of these by search should not go looking for a flag
      // that does not exist.
      items: ['future/ziti-native-previews', 'future/vercel-features'],
    },

    // Information-oriented. Exhaustive, scannable, and the thing you come back for.
    section('REFERENCE'),
    'reference/configuration',
    'reference/repo-config',
    'reference/projects',
    'reference/cli',
    'reference/build-logs',
    'reference/security',
  ],
};

export default sidebars;
