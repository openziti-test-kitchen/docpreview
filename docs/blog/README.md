# Blog drafts

Posts about nfpreview, aimed outward rather than at somebody working on the code. Drafts live here until
they are published somewhere.

`www/` is the product's documentation and answers "how do I use this". These answer "why would I want it",
which is a different job and a different voice. Nothing here is linked from the docs site.

## The overview

`00-netfoundry-preview.md` comes before the series. It credits Vercel for the preview loop and offers another way
for anyone who wants one. The reason given is contributor access on private repositories, with the build on our
own VM and zrok publishing the preview. Its reader is outside NetFoundry. It summarises 01, 02, 03 and 05 and
links to them rather than repeating them, and it links public NetFoundry and OpenZiti projects wherever they fit.

## The series

Three posts, because they have three different readers and one post that tries to serve all three ends up
serving none of them.

| | Post | Reader | Argument |
|---|---|---|---|
| 1 | `01-nobody-clones-the-repo.md` | anyone who reviews a documentation pull request | Reviewing docs properly means building them, so nobody does it, so the diff is what gets reviewed — and a diff hides the failures that matter |
| 2 | `02-bitbucket-private-repos.md` | teams on Bitbucket Cloud with private source | Vercel works on Bitbucket too and was our reference; repository-scoped access tokens make a self-hosted answer practical |
| 3 | `03-preview-url-is-a-security-decision.md` | someone who has decided they want previews | Who can open the link is a spectrum, from a public URL to no public surface at all |

Post 1 has to argue that previews are worth having. Post 3 assumes that argument is won and asks who
should see them. Merged, the second half reads as a feature list stapled to a pitch.

## The two that stand on their own

Neither needs the series, and both reach an audience the first three do not.

| | Post | Reader | Argument |
|---|---|---|---|
| 4 | `04-loopback-is-not-local.md` | anyone who has written a loopback check | A tunnel publishes every route while the listener is still bound to `127.0.0.1` and `RemoteAddr` still says so, so the usual check returns true with the endpoint on the internet |
| 5 | `05-what-it-took.md` | engineers | The problems between a working demo and something safe to leave running, nearly all of them from treating a network resource as though it were local |

Post 4 barely mentions nfpreview and is the better one to lead with anywhere that dislikes a product post.
Post 5 is the one to submit somewhere that rewards specifics.

## Order and reuse

Publish 1, 2, 3 in order. 4 and 5 go whenever, and 4 works as a standalone that links back to 1.

Post 1 and post 3 both carry the four-exposer table. Keep it in both — most readers arrive at one post from a
search and never see the other — but keep them in step when the table changes.

## Tone toward Vercel

Every mention of Vercel is complimentary. The framing is "if you like Vercel, keep using it; if not, here is
another way". Vercel defined the loop nfpreview copies, and the posts say so. The problem the posts name is not
price. In our setup, a pull request on a private repository was only built if its author was on our Vercel
team, so contributors outside it were blocked until somebody added them. That is reasonable on Vercel's side and
was friction on ours. nfpreview builds on our own VM and zrok publishes the preview, so nobody has to join
anything. Posts 00, 01 and 02 carry this argument. The earlier per-seat and red-check argument has been removed.

## Accuracy

Two claims need checking before anything is published.

**Contributor access.** Posts 00, 01 and 02 say that, on our private repositories, a pull request was only built
if its author was on our Vercel team. That is our experience, and the posts present it as ours, not as a general
statement about Vercel's product. It was the pull request author, not the viewer, who needed adding.

**Frontdoor.** It is implemented and its wire format has not been exercised against a live tenant, which
`www/docs/exposers.md` states plainly. A post that implies otherwise is a support conversation later.
