# Previews over OpenZiti

Today every preview, `main` and each pull request branch, gets a **public zrok link**. Anyone with the link can read
it. That's right for public docs, and it's the same thing Vercel does. For unreleased or internal docs, a link
anyone can open is the wrong default.

## The OpenZiti option

NetFoundry Preview can also publish previews onto an **OpenZiti overlay**. A preview then has no public address at
all. `http://my-branch.nfpreview.ziti/` works only on a machine running a Ziti tunneler (Ziti Desktop Edge or
`ziti-edge-tunnel`) with an enrolled identity. Everywhere else the name doesn't resolve.

- **One service covers every preview.** A single wildcard service, `*.nfpreview.ziti`, carries them all, and the
  daemon routes by hostname. A new pull request creates nothing on the network.
- **Sites work unchanged.** Each preview is served at `/` on its own hostname, so docs that hardcode their base URL
  still work.
- **Access is identity, not a secret link.** Whoever holds an enrolled identity with the reader role can read; nobody
  else can.

This exposer is built and proven against a real controller, router and Ziti Desktop Edge.

## What's missing

**One install can only publish one way today.** You pick zrok public *or* OpenZiti. The goal is both from one build:
the pull request comment carries a zrok link for everyone and a ziti link for the team. That's the multi-exposer work
in `docs/design/21-multi-exposer.md`, and a spike for it is running now.

The rest:

- **Every reader sees every preview.** Limiting a preview to the pull request's own people is the next step
  (`docs/design/22-identity-aware-previews.md`).
- **Adding and removing reviewers is manual.** There's no `nfpreview` command for it yet; it's the `ziti` CLI.
- **Every reviewer has to enroll.** That's the trade for having no public surface.

## Why not zrok private for previews?

A zrok private share has no stable name, so the "one comment, edited in place" design breaks. And every reviewer
would run `zrok2 access private` once per preview. zrok private is the right tool for operators reaching the admin
pages, not for reviewers clicking a link.
