import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import CodeBlock from '@theme/CodeBlock';
import Heading from '@theme/Heading';
import {NetFoundryHorizontalSection} from '@netfoundry/docusaurus-theme/ui';

import styles from './index.module.css';

// @theme/Layout resolves to @netfoundry/docusaurus-theme's Layout override, so
// the navbar, star banner and footer arrive without being wired up here.
// NetFoundryHorizontalSection is the theme's full-bleed band primitive: it
// centres its content and is the thing that reads --nf-section-bg, which is how
// the tinted bands below get their colour.

const REPO_URL = 'https://github.com/openziti-test-kitchen/docpreview';

// Both commands are copied from docs/quickstart.md. They are the shortest path
// that ends in a real URL, and neither needs a GitHub App or an account.
const QUICKSTART = `docpreview init                    # one question: which exposer
docpreview preview -build ./www    # publishes a real URL, holds it until Ctrl-C`;

function Hero(): ReactNode {
  return (
    <div className={styles.hero}>
      <div className={styles.heroInner}>
        <div>
          <Heading as="h1" className={styles.heroTitle}>
            docpreview
          </Heading>
          <p className={styles.heroTagline}>
            Documentation previews for pull requests, running anywhere
          </p>
          <p className={styles.heroBody}>
            Push a commit. A comment appears on the pull request with a link, you click it, and there is
            your documentation site — built from that branch, at its own URL. Push again and the same
            comment updates in place. Close the pull request and the preview disappears.
          </p>
          <p className={styles.heroBody}>
            That is what Vercel does for documentation repositories. The difference is where it runs: one
            Go binary you can start on a laptop, publishing over an OpenZiti overlay rather than through
            somebody else&apos;s cloud.
          </p>
          <div className={styles.heroButtons}>
            <Link className="button button--primary button--lg" to="/docs/quickstart">
              Quickstart
            </Link>
            <Link className="button button--secondary button--outline button--lg" to="/docs/intro">
              What this is
            </Link>
          </div>
        </div>

        <div className={styles.heroAside}>
          <div className={styles.terminal}>
            <div className={styles.terminalBar}>
              <span className={styles.terminalDot} />
              <span className={styles.terminalDot} />
              <span className={styles.terminalDot} />
              <span className={styles.terminalTitle}>powershell</span>
            </div>
            <CodeBlock language="powershell">{QUICKSTART}</CodeBlock>
          </div>
          <p className={styles.heroAsideNote}>
            No GitHub App, no accounts, no control plane. Point it at any directory that builds a static
            site and it publishes one.
          </p>
        </div>
      </div>
    </div>
  );
}

const STEPS = [
  {
    title: 'A push arrives',
    body:
      'The webhook lands, docpreview checks whether the pull request actually touched documentation, ' +
      'and says so on the pull request when it did not.',
  },
  {
    title: 'The branch is built',
    body:
      'A clone at the exact commit, then npm run build — locally or inside a container. Output is ' +
      'streamed to a redacted, stored build log.',
  },
  {
    title: 'A URL is published',
    body:
      'The built directory is served through whichever exposer you configured. The site is verified ' +
      'against its mount point before anyone sees it.',
  },
  {
    title: 'One comment, edited',
    body:
      'The bot upserts a single comment. Push again and the commit and timestamp change while the ' +
      'link stays put, so a bookmark keeps working.',
  },
];

function HowItWorks(): ReactNode {
  return (
    <NetFoundryHorizontalSection className={styles.sectionTinted}>
      <div className={styles.section}>
        <Heading as="h2" className={styles.sectionTitle}>
          Four steps, one process
        </Heading>
        <p className={styles.sectionLead}>
          One binary, one sqlite file. No Kubernetes, no control plane. Docker is optional and only used
          to sandbox the build.
        </p>
        <ol className={styles.steps}>
          {STEPS.map((step) => (
            <li key={step.title} className={styles.step}>
              <div className={styles.stepTitle}>{step.title}</div>
              <p className={styles.stepBody}>{step.body}</p>
            </li>
          ))}
        </ol>
      </div>
    </NetFoundryHorizontalSection>
  );
}

// Adapted from the sample comment in the repository README, which is in turn a
// copy of what the daemon actually posts.
const COMMENT_ROWS: [string, ReactNode][] = [
  ['Status', '✅ Ready'],
  ['Preview', <code key="url">https://docs-quickstart-test.share.zrok.io/</code>],
  ['Name', <code key="name">docs-quickstart-test</code>],
  ['Commit', <code key="commit">a1b2c3d</code>],
  ['Built in', '41s'],
  ['Updated', '2026-07-27 14:22:07 UTC'],
];

function TheComment(): ReactNode {
  return (
    <NetFoundryHorizontalSection>
      <div className={styles.section}>
        <div className={styles.commentLayout}>
          <div>
            <Heading as="h2" className={styles.sectionTitle}>
              What a reviewer sees
            </Heading>
            <p className={styles.sectionLead}>
              One comment per pull request, edited in place for the life of the branch. The status line
              carries a failed build too — with a link to the log rather than a shrug — and the comment is
              retracted when the pull request closes.
            </p>
            <Link to="/docs/quickstart">Walk through it →</Link>
          </div>

          <div className={styles.comment}>
            <div className={styles.commentHeader}>
              <span className={styles.commentAuthor}>docpreview</span>
              <span className={styles.commentBot}>bot</span>
              <span>commented just now</span>
            </div>
            <div className={styles.commentBodyTitle}>Documentation preview</div>
            <div className={styles.commentTableWrap}>
              <table className={styles.commentTable}>
                <tbody>
                  {COMMENT_ROWS.map(([label, value]) => (
                    <tr key={label}>
                      <th scope="row">{label}</th>
                      <td>{value}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </div>
    </NetFoundryHorizontalSection>
  );
}

const DIFFERENCES = [
  {
    title: 'Docs that cannot go to a SaaS',
    body:
      'Unreleased, internal, embargoed. A hosted preview service means handing that content, and a ' +
      'repository token, to a third party. Here the build runs on your hardware.',
  },
  {
    title: 'One binary, runs on a laptop',
    body:
      'go build, then one process. A documentation site is a directory of static files; serving one ' +
      'behind a URL is not a distributed systems problem.',
  },
  {
    title: 'The address is yours',
    body:
      'Public URLs come from zrok or NetFoundry Frontdoor over an OpenZiti overlay. A zrok-published ' +
      'preview binds no local TCP port at all.',
  },
];

function WhyDifferent(): ReactNode {
  return (
    <NetFoundryHorizontalSection className={styles.sectionTinted}>
      <div className={styles.section}>
        <Heading as="h2" className={styles.sectionTitle}>
          Not somebody else&apos;s cloud
        </Heading>
        <p className={styles.sectionLead}>
          The behaviour is the familiar one. What changes is who runs the build and who owns the address
          it appears at.
        </p>
        <div className={styles.cards}>
          {DIFFERENCES.map((item) => (
            <div key={item.title} className={styles.card}>
              <div className={styles.cardTitle}>{item.title}</div>
              <p className={styles.cardBody}>{item.body}</p>
            </div>
          ))}
        </div>
      </div>
    </NetFoundryHorizontalSection>
  );
}

// `surface` doubles as the badge label and the CSS-module class suffix, so the
// four cards cannot drift apart from their colours.
const EXPOSERS: {name: string; surface: string; surfaceClass: string; body: string}[] = [
  {
    name: 'zrok2',
    surface: 'public URL',
    surfaceClass: styles.surfacePublic,
    body:
      'The default. A public URL over an OpenZiti overlay, served straight into the mesh through the ' +
      'zrok Go SDK — no local port bound, no reverse proxy.',
  },
  {
    name: 'frontdoor',
    surface: 'public URL',
    surfaceClass: styles.surfacePublic,
    body:
      'NetFoundry Frontdoor, which adds a WAF in front of the preview and access enforced by your ' +
      'identity provider rather than by the link being unguessable.',
  },
  {
    name: 'ziti',
    surface: 'no public surface',
    surfaceClass: styles.surfaceNone,
    body:
      'Reachable only from a machine running a tunneler with an enrolled identity. The hostname does ' +
      'not resolve and the address is not routable. Nothing is exposed.',
  },
  {
    name: 'local',
    surface: 'loopback',
    surfaceClass: styles.surfaceLoopback,
    body:
      'An ephemeral loopback port. Useless for sharing, ideal for trying it: the whole clone, build ' +
      'and comment path runs without an account anywhere.',
  },
];

function Exposers(): ReactNode {
  return (
    <NetFoundryHorizontalSection>
      <div className={styles.section}>
        <Heading as="h2" className={styles.sectionTitle}>
          Four ways to reach a preview
        </Heading>
        <p className={styles.sectionLead}>
          An exposer turns a built preview into a URL somebody can open, and it is the only part of
          docpreview that knows how traffic reaches you. Switching between them is one line of config —
          but the tradeoff is real, and it is about who can read your unreleased documentation.
        </p>
        <div className={styles.exposers}>
          {EXPOSERS.map((exposer) => (
            <div key={exposer.name} className={styles.exposer}>
              <p className={styles.exposerName}>{exposer.name}</p>
              <span className={clsx(styles.exposerBadge, exposer.surfaceClass)}>{exposer.surface}</span>
              <p className={styles.exposerBody}>{exposer.body}</p>
            </div>
          ))}
        </div>
        <p style={{marginTop: '1.5rem'}}>
          <Link to="/docs/exposers">Compare them properly →</Link>
        </p>
      </div>
    </NetFoundryHorizontalSection>
  );
}

const VAULT = `docpreview vault set github.private_key -file .\\app-key.pem
"webhook-secret" | docpreview vault set github.webhook_secret`;

function Credentials(): ReactNode {
  return (
    <NetFoundryHorizontalSection className={styles.sectionTinted}>
      <div className={styles.section}>
        <div className={styles.commentLayout}>
          <div>
            <Heading as="h2" className={styles.sectionTitle}>
              Credentials in one file
            </Heading>
            <p className={styles.sectionLead}>
              Every credential docpreview needs lives in <code>~/.docpreview/vault.age</code>, encrypted
              with age. Not a service, nothing to install — the library is linked into the binary. Where
              the master key comes from is a choice you make, not a default you inherit.
            </p>
            <p className={styles.sectionLead}>
              A secret injected into a build is redacted from every log line and every pull request
              comment, in each encoding a build tool might emit it in.
            </p>
            <p>
              <Link to="/docs/reference/security">Read the security model →</Link>
            </p>
          </div>
          <div className={styles.terminal}>
            <div className={styles.terminalBar}>
              <span className={styles.terminalDot} />
              <span className={styles.terminalDot} />
              <span className={styles.terminalDot} />
              <span className={styles.terminalTitle}>powershell</span>
            </div>
            <CodeBlock language="powershell">{VAULT}</CodeBlock>
          </div>
        </div>
      </div>
    </NetFoundryHorizontalSection>
  );
}

function Closing(): ReactNode {
  return (
    <NetFoundryHorizontalSection>
      <div className={clsx(styles.section, styles.closing)}>
        <Heading as="h2" className={styles.sectionTitle}>
          Try it on one repository
        </Heading>
        <p className={styles.sectionLead} style={{margin: '0 auto'}}>
          Ten minutes to a preview URL on a pull request, and nothing to undo if you stop there: one
          binary, one config file, and a source-control app you can uninstall. Run it on a laptop first
          — the quickstart needs no account and no inbound port.
        </p>
        <div className={styles.closingButtons}>
          <Link className="button button--primary button--lg" to="/docs/quickstart">
            Start here
          </Link>
          <Link className="button button--secondary button--outline button--lg" to="/docs/architecture">
            How it is built
          </Link>
          <Link className="button button--secondary button--outline button--lg" href={REPO_URL}>
            Source
          </Link>
        </div>
      </div>
    </NetFoundryHorizontalSection>
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="Documentation previews for pull requests"
      description={
        'A single Go binary that builds a documentation preview for every pull request and posts the ' +
        'URL back, using zrok or NetFoundry Frontdoor for public access.'
      }>
      <Hero />
      <main>
        <HowItWorks />
        <TheComment />
        <WhyDifferent />
        <Exposers />
        <Credentials />
        <Closing />
      </main>
    </Layout>
  );
}
