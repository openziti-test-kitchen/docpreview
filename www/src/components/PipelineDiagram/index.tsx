import type {ReactNode} from 'react';

import styles from './styles.module.css';

// The path one push takes, drawn inline rather than shipped as an image so it can take its colours from
// the theme's variables and follow the light/dark switch.
//
// Every box is a stage the code has: the webhook is verified and queued in sqlite (internal/store), the
// build clones the exact commit, runs detection and the build in a container by default
// (internal/pipeline), the output's baseUrl is checked before anything is published, an exposer
// publishes it (internal/expose), and one comment is edited in place (internal/scm/comment.go). Keep
// it that way: a box for a stage that does not exist is a feature the page is inventing.

type BoxProps = {
  x: number;
  y: number;
  w: number;
  h: number;
  title: string;
  lines?: string[];
  accent?: boolean;
};

function Box({x, y, w, h, title, lines = [], accent}: BoxProps): ReactNode {
  const cx = x + w / 2;
  return (
    <g>
      <rect x={x} y={y} width={w} height={h} rx={12} className={accent ? styles.boxAccent : styles.box} />
      <text x={cx} y={y + 30} className={styles.title} textAnchor="middle">
        {title}
      </text>
      {lines.map((line, i) => (
        <text key={line} x={cx} y={y + 52 + i * 18} className={styles.sub} textAnchor="middle">
          {line}
        </text>
      ))}
    </g>
  );
}

function Arrow({d}: {d: string}): ReactNode {
  return (
    <>
      <path d={d} className={styles.wire} markerEnd="url(#nfp-arrow)" />
      <path d={d} className={styles.flow} />
    </>
  );
}

const EXPOSERS = ['zrok2', 'frontdoor', 'ziti', 'local'];

export default function PipelineDiagram(): ReactNode {
  return (
    <figure className={styles.figure}>
      <svg
        className={styles.svg}
        viewBox="0 0 1120 330"
        role="img"
        aria-labelledby="nfp-pipeline-title nfp-pipeline-desc">
        <title id="nfp-pipeline-title">How a push becomes a preview</title>
        <desc id="nfp-pipeline-desc">
          A pull request on GitHub or Bitbucket sends a signed webhook. nfpreview queues it in sqlite,
          clones the exact commit, decides whether the site changed and builds it in a throwaway container,
          checks the output against the path it will be served at, publishes it through one of four
          exposers, and edits a single comment on the pull request with the link.
        </desc>
        <defs>
          <marker
            id="nfp-arrow"
            viewBox="0 0 10 10"
            refX="9"
            refY="5"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" className={styles.arrowHead} />
          </marker>
          <linearGradient id="nfp-band" x1="0" x2="1" y1="0" y2="0">
            <stop offset="0%" className={styles.bandStart} />
            <stop offset="100%" className={styles.bandEnd} />
          </linearGradient>
        </defs>

        {/* 1. The pull request */}
        <Box x={10} y={60} w={150} h={110} title="Pull request" lines={['GitHub App', 'or Bitbucket']} />

        {/* 2. Ingress and the queue */}
        <Box x={200} y={60} w={150} h={110} title="Webhook" lines={['signature checked', 'queued in sqlite']} />

        {/* 3. The build, in its container */}
        <rect x={390} y={20} width={330} height={190} rx={16} className={styles.container} />
        <text x={555} y={44} className={styles.containerLabel} textAnchor="middle">
          throwaway docker container (default)
        </text>
        <Box x={405} y={60} w={95} h={110} title="Clone" lines={['exact', 'commit']} />
        <Box x={507} y={60} w={95} h={110} title="Detect" lines={['did the', 'site change?']} />
        <Box x={609} y={60} w={95} h={110} title="Build" lines={['npm run', 'build']} />
        <text x={555} y={196} className={styles.note} textAnchor="middle">
          secrets injected from the vault, redacted from the log
        </text>

        {/* 4. The baseUrl check */}
        <Box x={760} y={60} w={150} h={110} title="Verify" lines={['asset URLs match', 'the serve path']} />

        {/* 5. The exposer */}
        <rect x={950} y={30} width={160} height={170} rx={12} className={styles.boxAccent} />
        <text x={1030} y={58} className={styles.title} textAnchor="middle">
          Publish
        </text>
        {EXPOSERS.map((name, i) => (
          <g key={name}>
            <rect x={968} y={72 + i * 30} width={124} height={24} rx={12} className={styles.pill} />
            <text x={1030} y={89 + i * 30} className={styles.pillText} textAnchor="middle">
              {name}
            </text>
          </g>
        ))}

        <Arrow d="M160,115 L196,115" />
        <Arrow d="M350,115 L401,115" />
        <Arrow d="M500,115 L503,115" />
        <Arrow d="M602,115 L605,115" />
        <Arrow d="M704,115 L756,115" />
        <Arrow d="M910,115 L946,115" />

        {/* 6. Back to the pull request: one comment, edited in place */}
        <rect x={10} y={250} width={1100} height={64} rx={14} fill="url(#nfp-band)" className={styles.band} />
        <text x={560} y={278} className={styles.bandTitle} textAnchor="middle">
          One comment on the pull request, edited in place on every push
        </text>
        <text x={560} y={300} className={styles.bandSub} textAnchor="middle">
          status, preview link, commit and time. A skipped change says why. Closing the pull request retracts it.
        </text>
        <Arrow d="M1030,200 L1030,246" />
        <Arrow d="M85,250 L85,174" />
      </svg>
    </figure>
  );
}
