import type {ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type FeatureItem = {
  title: string;
  description: ReactNode;
};

const FeatureList: FeatureItem[] = [
  {
    title: 'Runs anywhere',
    description: (
      <>
        One Go binary and one sqlite file. Start it on a laptop, a VM, or a
        container. Docker is optional and only sandboxes the build; Kubernetes
        is not involved.
      </>
    ),
  },
  {
    title: 'Your ingress, your rules',
    description: (
      <>
        Public URLs come from zrok or NetFoundry Frontdoor behind one interface,
        so the content never leaves your network except through an ingress you
        control — with an IdP in front of it if you want one.
      </>
    ),
  },
  {
    title: 'One comment, kept current',
    description: (
      <>
        The preview URL is derived from the branch and stays put across
        rebuilds, so the pull request gets a single comment that updates in
        place instead of a new one on every push.
      </>
    ),
  },
];

function Feature({title, description}: FeatureItem) {
  return (
    <div className={clsx('col col--4')}>
      <div className="padding-horiz--md">
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
