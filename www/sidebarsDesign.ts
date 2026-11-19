import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

// The design documents, published from docs/design at the repository root.
//
// The groups are the ones docs/design/README.md draws, in its order: what the code does now, plans for
// code that does not exist yet, and research. A reader has to be able to tell which kind of document
// they are in, because a plan read as a description is a feature that does not exist.
//
// Ids are the filenames with the number prefix removed, which Docusaurus does by default. A document
// added to docs/design and not listed here is published but reachable only by link, so add it.
const section = (label: string) => ({
  type: 'link' as const,
  label,
  href: '#',
  className: 'sidebar-title',
});

const sidebars: SidebarsConfig = {
  designSidebar: [
    'README',

    section('HOW IT WORKS'),
    'architecture',
    'exposers',
    'build-pipeline',
    'concurrency',
    'secrets',
    'build-logs',
    'dashboard',
    'storage',
    'scm',
    'security',
    'container',

    section('PLANS'),
    'github-setup-state',
    'github-roadmap',
    'github-testing',
    'production-deployment',
    'bitbucket',
    'exposer-zrok',
    'exposer-frontdoor',
    'exposer-ziti',
    'zrok-namespacing',
    'multi-exposer',
    'identity-aware-previews',

    section('RESEARCH'),
    'ziti-native-previews',
    'vercel-features',
  ],
};

export default sidebars;
