import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import CodeBlock from '@theme/CodeBlock';
import Heading from '@theme/Heading';

import PipelineDiagram from '@site/src/components/PipelineDiagram';
import styles from './index.module.css';

// @theme/Layout resolves to @netfoundry/docusaurus-theme's Layout override, so the navbar, star banner
// and footer arrive without being wired up here.
//
// Every claim on this page has to be true of the code. Where a sentence names a behaviour, the comment
// beside it says where that behaviour lives, so the next person to change one can find the other.

const REPO_URL = 'https://github.com/openziti-test-kitchen/docpreview';

// ── Hero ──────────────────────────────────────────────────────────────────────────────────────────────

// The shape internal/scm/comment.go RenderComment posts: a bold title, then one row under five headings.
// Keep the two in step.
const COMMENT_HEADINGS = ['Project', 'Status', 'Preview', 'Commit', 'Updated (UTC)'];

function CommentCard(): ReactNode {
  return (
    <div className={styles.comment} aria-label="Example pull request comment">
      <div className={styles.commentHeader}>
        <span className={styles.avatar} aria-hidden="true" />
        <span className={styles.commentAuthor}>netfoundry-preview</span>
        <span className={styles.commentBot}>bot</span>
        <span className={styles.commentWhen}>edited just now</span>
      </div>
      <div className={styles.commentBody}>
        <div className={styles.commentBodyTitle}>NetFoundry Preview</div>
        <div className={styles.commentTableWrap}>
          <table className={styles.commentTable}>
            <thead>
              <tr>
                {COMMENT_HEADINGS.map((heading) => (
                  <th key={heading} scope="col">
                    {heading}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              <tr>
                <td>
                  <span className={styles.fakeLink}>acme/docs</span>
                </td>
                <td>
                  ✅ <span className={styles.fakeLink}>Ready</span> · 41s
                </td>
                <td>
                  <span className={styles.fakeLink}>Preview</span>
                </td>
                <td>
                  <code>a1b2c3d</code>
                </td>
                <td>Jul 27, 2026 2:22pm</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}

function Hero(): ReactNode {
  return (
    <header className={styles.hero}>
      <div className={styles.heroGlow} aria-hidden="true" />
      <div className={styles.heroInner}>
        <div className={styles.heroCopy}>
          <p className={styles.eyebrow}>Self-hosted pull request previews</p>
          <Heading as="h1" className={styles.heroTitle}>
            Every pull request, <span className={styles.gradientText}>a live preview</span>. Built on
            hardware you run.
          </Heading>
          <p className={styles.heroLead}>
            Push a commit and a bot comment appears on the pull request with a link to your site, built from
            that branch. Push again and the same comment updates. Close the pull request and the preview
            goes away. One Go binary, publishing over zrok, NetFoundry Frontdoor or a private OpenZiti
            network.
          </p>
          <div className={styles.heroButtons}>
            <Link className="button button--primary button--lg" to="/docs/quickstart">
              Get started
            </Link>
            <Link className="button button--secondary button--outline button--lg" to="/docs/intro">
              What this is
            </Link>
          </div>
          <ul className={styles.heroFacts}>
            <li>GitHub and Bitbucket</li>
            <li>Builds in a throwaway container</li>
            <li>One process, one sqlite file</li>
          </ul>
        </div>

        <div className={styles.heroVisual}>
          <div className={styles.prChrome}>
            <div className={styles.prTitle}>
              <span className={styles.prState}>Open</span>
              <span>Rewrite the install guide</span>
              <span className={styles.prNumber}>#128</span>
            </div>
            <CommentCard />
          </div>
          <p className={styles.heroVisualNote}>
            The comment it posts, and edits on every push. Bitbucket gets the same table.
          </p>
        </div>
      </div>
    </header>
  );
}

// ── How it works ─────────────────────────────────────────────────────────────────────────────────────

const STEPS = [
  {
    title: 'A push arrives',
    body:
      'The webhook signature is checked and the job is queued in sqlite. Pull requests from forks are ' +
      'refused before they are queued.',
  },
  {
    title: 'The branch is built',
    body:
      'The exact commit is cloned. If nothing the site is built from changed, it stops and says why. ' +
      'Otherwise npm run build runs inside a throwaway container.',
  },
  {
    title: 'The output is checked',
    body:
      'Docusaurus bakes the base URL into every asset path. A build whose asset URLs disagree with the ' +
      'path it is about to be served at is refused, not published.',
  },
  {
    title: 'A URL, one comment',
    body:
      'The exposer you configured publishes it, and a single comment is edited in place. Push again and ' +
      'the link stays put while the commit and time change.',
  },
];

function HowItWorks(): ReactNode {
  return (
    <section id="how-it-works" className={clsx(styles.section, styles.sectionTinted)}>
      <div className={styles.container}>
        <p className={styles.kicker}>How it works</p>
        <Heading as="h2" className={styles.sectionTitle}>
          From push to preview in one process
        </Heading>
        <p className={styles.sectionLead}>
          No Kubernetes and no control plane. The daemon takes the webhook, runs the build, serves the output
          and talks back to the pull request.
        </p>
        <div className={styles.diagram}>
          <PipelineDiagram />
        </div>
        <ol className={styles.steps}>
          {STEPS.map((step) => (
            <li key={step.title} className={styles.step}>
              <div className={styles.stepTitle}>{step.title}</div>
              <p className={styles.stepBody}>{step.body}</p>
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}

// ── Features ─────────────────────────────────────────────────────────────────────────────────────────

type Feature = {icon: ReactNode; title: string; body: ReactNode; to: string};

// Small line icons, drawn here so they take currentColor and need no icon package.
const Icon = ({children}: {children: ReactNode}) => (
  <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="1.8"
    strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    {children}
  </svg>
);

const FEATURES: Feature[] = [
  {
    // internal/scm/comment.go: the marker, the five columns, edited in place.
    icon: (
      <Icon>
        <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
      </Icon>
    ),
    title: 'One comment, never a pile',
    body:
      'The bot finds its own comment again and edits it, so a busy pull request does not fill with ' +
      'bot noise. A failed build links to its log from the same status cell.',
    to: '/docs/architecture',
  },
  {
    // internal/config: build.driver defaults to docker, allow_local_driver is off by default.
    icon: (
      <Icon>
        <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
        <path d="M3.3 7 12 12l8.7-5M12 22V12" />
      </Icon>
    ),
    title: 'Builds in a throwaway container',
    body:
      'A pull request build is somebody else’s code, so by default it runs in Docker with CPU and ' +
      'memory caps. Building on the host is an opt-in you have to write down.',
    to: '/docs/reference/configuration',
  },
  {
    // internal/vault (age) and internal/redact (variants: raw, URL, JSON, base64).
    icon: (
      <Icon>
        <rect x="3" y="11" width="18" height="11" rx="2" />
        <path d="M7 11V7a5 5 0 0 1 10 0v4" />
      </Icon>
    ),
    title: 'Credentials in one encrypted file',
    body:
      'Every credential lives in an age-encrypted vault linked into the binary. Secrets injected into a ' +
      'build are redacted from its log, including URL, JSON and base64 encoded forms.',
    to: '/docs/reference/security',
  },
  {
    // internal/pipeline: the base URL mismatch detector.
    icon: (
      <Icon>
        <path d="M9 11l3 3L22 4" />
        <path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11" />
      </Icon>
    ),
    title: 'Never an unstyled preview',
    body:
      'The base URL is passed into the build and used as the serve path, then the built index.html is ' +
      'checked against it before a reviewer ever sees the link.',
    to: '/docs/troubleshooting',
  },
  {
    // internal/scm: github and bitbucket clients.
    icon: (
      <Icon>
        <circle cx="6" cy="6" r="2.5" />
        <circle cx="6" cy="18" r="2.5" />
        <circle cx="18" cy="9" r="2.5" />
        <path d="M6 8.5v7M18 11.5c0 3-3 4.5-9.5 5" />
      </Icon>
    ),
    title: 'GitHub and Bitbucket',
    body:
      'A GitHub App or Bitbucket Cloud, or both from the same daemon. Comments and build statuses on ' +
      'each, behind one interface that normalises the two.',
    to: '/docs/guides/bitbucket',
  },
  {
    // internal/daemon: dashboard, stream.go (server-sent events).
    icon: (
      <Icon>
        <rect x="3" y="4" width="18" height="16" rx="2" />
        <path d="M7 9l3 3-3 3M13 15h4" />
      </Icon>
    ),
    title: 'A dashboard with live logs',
    body:
      'Builds and previews in one operator page, with each build’s log streamed as it runs. Admin ' +
      'only counts over a private path: OpenZiti, a zrok private share, or the machine itself.',
    to: '/docs/reference/build-logs',
  },
];

function Features(): ReactNode {
  return (
    <section className={styles.section}>
      <div className={styles.container}>
        <p className={styles.kicker}>What you get</p>
        <Heading as="h2" className={styles.sectionTitle}>
          The preview loop, without handing over the repository
        </Heading>
        <p className={styles.sectionLead}>
          The workflow is the one Vercel made familiar. What changes is who runs the build and who owns the
          address it shows up at.
        </p>
        <div className={styles.features}>
          {FEATURES.map((f) => (
            <Link key={f.title} to={f.to} className={styles.feature}>
              <span className={styles.featureIcon}>{f.icon}</span>
              <span className={styles.featureTitle}>{f.title}</span>
              <span className={styles.featureBody}>{f.body}</span>
            </Link>
          ))}
        </div>
      </div>
    </section>
  );
}

// ── Exposers ─────────────────────────────────────────────────────────────────────────────────────────

// The table in www/docs/exposers.md, one card per row. `reach` is how far the preview travels: it drives
// the meter, so 0 is loopback and 3 is the public internet.
const EXPOSERS: {name: string; who: string; surface: string; reach: number; body: string}[] = [
  {
    name: 'local',
    who: 'You, on loopback',
    surface: 'No public surface',
    reach: 0,
    body: 'Try the whole clone, build and comment path with no account anywhere.',
  },
  {
    name: 'ziti',
    who: 'An enrolled OpenZiti identity',
    surface: 'No public surface at all',
    reach: 1,
    body:
      'The hostname does not resolve and nothing is routable except from a machine running a tunneler ' +
      'with an identity. For unreleased and internal sites.',
  },
  {
    name: 'frontdoor',
    who: 'Whoever your identity provider admits',
    surface: 'Public URL',
    reach: 2,
    body: 'NetFoundry Frontdoor puts a WAF and IdP-enforced access in front of the preview.',
  },
  {
    name: 'zrok2',
    who: 'Anyone with the link, or an OAuth-gated subset',
    surface: 'Public URL',
    reach: 3,
    body:
      'The default. Served into the overlay through the zrok Go SDK, so the preview binds no local TCP ' +
      'port at all.',
  },
];

function Exposers(): ReactNode {
  return (
    <section className={clsx(styles.section, styles.sectionTinted)}>
      <div className={styles.container}>
        <div className={styles.split}>
          <div>
            <p className={styles.kicker}>Exposers</p>
            <Heading as="h2" className={styles.sectionTitle}>
              Your preview URL is a security decision
            </Heading>
            <p className={styles.sectionLead}>
              An exposer turns a built preview into an address somebody can open. Switching between them is
              one line of config and nothing above it changes: not the queue, not the build, not the comment.
              What changes is who can read your unreleased site.
            </p>
            <Link className={styles.textLink} to="/docs/exposers">
              Compare the exposers →
            </Link>
          </div>
          <div className={styles.exposers}>
            {EXPOSERS.map((e) => (
              <div key={e.name} className={styles.exposer}>
                <div className={styles.exposerHead}>
                  <code className={styles.exposerName}>{e.name}</code>
                  <span className={styles.meter} aria-label={`reach ${e.reach + 1} of 4`}>
                    {[0, 1, 2, 3].map((i) => (
                      <span key={i} className={clsx(styles.meterDot, i <= e.reach && styles.meterOn)} />
                    ))}
                  </span>
                </div>
                <div className={styles.exposerWho}>{e.who}</div>
                <div className={styles.exposerSurface}>{e.surface}</div>
                <p className={styles.exposerBody}>{e.body}</p>
              </div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}

// ── Quick start ──────────────────────────────────────────────────────────────────────────────────────

// Copied from www/docs/quickstart.md and README.md. Part 1 needs no GitHub App; part 2 wires up pull
// requests.
const TRY_IT = `go build -o bin/ ./cmd/nfpreview

nfpreview init                    # one question: which exposer
nfpreview preview -build ./www    # publishes a real URL, holds it until Ctrl-C`;

const WIRE_IT = `nfpreview vault set github.private_key -file .\\app-key.pem
"webhook-secret" | nfpreview vault set github.webhook_secret

nfpreview doctor                  # real calls, not a config lint
nfpreview serve`;

function QuickStart(): ReactNode {
  return (
    <section className={styles.section}>
      <div className={styles.container}>
        <p className={styles.kicker}>Quick start</p>
        <Heading as="h2" className={styles.sectionTitle}>
          A URL first, pull requests second
        </Heading>
        <p className={styles.sectionLead}>
          Part one builds a site and publishes it with no GitHub App and no source control at all. Pick the{' '}
          <code>local</code> exposer and it needs no account either. Part two puts it on pull requests.
        </p>
        <div className={styles.quick}>
          <div className={styles.quickStep}>
            <div className={styles.quickHead}>
              <span className={styles.quickNum}>1</span>
              <span>See it work</span>
            </div>
            <div className={styles.terminal}>
              <CodeBlock language="powershell">{TRY_IT}</CodeBlock>
            </div>
            <p className={styles.quickNote}>
              Needs Go 1.26+ and Docker, or Node 20+ if you opt into building on the host.
            </p>
          </div>
          <div className={styles.quickStep}>
            <div className={styles.quickHead}>
              <span className={styles.quickNum}>2</span>
              <span>Wire it to pull requests</span>
            </div>
            <div className={styles.terminal}>
              <CodeBlock language="powershell">{WIRE_IT}</CodeBlock>
            </div>
            <p className={styles.quickNote}>
              Then expose the webhook and open a pull request. On Bitbucket, follow the{' '}
              <Link to="/docs/guides/bitbucket">Bitbucket guide</Link> instead.
            </p>
          </div>
        </div>
        <Link className={styles.textLink} to="/docs/quickstart">
          The full quickstart →
        </Link>
      </div>
    </section>
  );
}

// ── Explore ──────────────────────────────────────────────────────────────────────────────────────────

const EXPLORE: {title: string; body: string; to?: string; href?: string; label: string}[] = [
  {
    title: 'Get started',
    body: 'What it is, what it deliberately does not do, and ten minutes to a preview URL.',
    to: '/docs/intro',
    label: 'Introduction',
  },
  {
    title: 'Set it up',
    body: 'The GitHub App, Bitbucket, the webhook tunnel, zrok and Frontdoor, a Linux service, a container.',
    to: '/docs/guides/github-app',
    label: 'How-to guides',
  },
  {
    title: 'Configure it',
    body: 'Every key in the server config and in .nfpreview.yml, the CLI, and the build logs.',
    to: '/docs/reference/configuration',
    label: 'Reference',
  },
  {
    title: 'Trust it',
    body: 'The threat model, credentials at rest and in memory, webhooks, forks, and what is not defended.',
    to: '/docs/reference/security',
    label: 'Security model',
  },
  {
    title: 'Change it',
    body: 'How it works and why, written for someone changing the code: invariants, plans and research.',
    to: '/design/',
    label: 'Design documents',
  },
  {
    title: 'Read why',
    body: 'Why previews matter for docs, why the URL is a security decision, and what it actually took.',
    href: `${REPO_URL}/tree/main/docs/blog`,
    label: 'Blog drafts on GitHub',
  },
];

function Explore(): ReactNode {
  return (
    <section className={clsx(styles.section, styles.sectionTinted)}>
      <div className={styles.container}>
        <p className={styles.kicker}>Documentation</p>
        <Heading as="h2" className={styles.sectionTitle}>
          Everything about it, in one place
        </Heading>
        <div className={styles.explore}>
          {EXPLORE.map((e) => (
            <Link key={e.title} className={styles.exploreCard} to={e.to} href={e.href}>
              <span className={styles.exploreLabel}>{e.label}</span>
              <span className={styles.exploreTitle}>{e.title}</span>
              <span className={styles.exploreBody}>{e.body}</span>
            </Link>
          ))}
        </div>
      </div>
    </section>
  );
}

// ── Closing ──────────────────────────────────────────────────────────────────────────────────────────

function Closing(): ReactNode {
  return (
    <section className={styles.closingWrap}>
      <div className={styles.closing}>
        <Heading as="h2" className={styles.closingTitle}>
          Try it on one repository
        </Heading>
        <p className={styles.closingLead}>
          One binary, one config file, and a source-control app you can uninstall. Run it on a laptop first.
          This site is built by it too: every pull request against it gets a preview.
        </p>
        <div className={styles.closingButtons}>
          <Link className="button button--primary button--lg" to="/docs/quickstart">
            Start the quickstart
          </Link>
          <Link className="button button--secondary button--outline button--lg" href={REPO_URL}>
            View on GitHub
          </Link>
        </div>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="Self-hosted pull request previews"
      description={
        'NetFoundry Preview builds a preview of any static site for every pull request and posts the URL ' +
        'back. One Go binary on hardware you run, publishing over zrok, NetFoundry Frontdoor or OpenZiti.'
      }>
      <div className={styles.landing}>
        <Hero />
        <main>
          <HowItWorks />
          <Features />
          <Exposers />
          <QuickStart />
          <Explore />
          <Closing />
        </main>
      </div>
    </Layout>
  );
}
