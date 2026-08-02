# Blog drafts

Posts about docpreview, aimed outward rather than at somebody working on the code. Drafts live here until
they are published somewhere.

`www/` is the product's documentation and answers "how do I use this". These answer "why would I want it",
which is a different job and a different voice. Nothing here is linked from the docs site.

## The series

Three posts, because they have three different readers and one post that tries to serve all three ends up
serving none of them.

| | Post | Reader | Argument |
|---|---|---|---|
| 1 | `01-nobody-clones-the-repo.md` | anyone who reviews a documentation pull request | Reviewing docs properly means building them, so nobody does it, so the diff is what gets reviewed — and a diff hides the failures that matter |
| 2 | `02-bitbucket-private-repos.md` | teams on Bitbucket Cloud with private source | The hosted preview services are thin here, and repository-scoped access tokens make a self-hosted answer practical |
| 3 | `03-preview-url-is-a-security-decision.md` | someone who has decided they want previews | Who can open the link is a spectrum, from a public URL to no public surface at all |

Post 1 has to argue that previews are worth having. Post 3 assumes that argument is won and asks who
should see them. Merged, the second half reads as a feature list stapled to a pitch.

## The two that stand on their own

Neither needs the series, and both reach an audience the first three do not.

| | Post | Reader | Argument |
|---|---|---|---|
| 4 | `04-loopback-is-not-local.md` | anyone who has written a loopback check | A tunnel publishes every route while the listener is still bound to `127.0.0.1` and `RemoteAddr` still says so, so the usual check returns true with the endpoint on the internet |
| 5 | `05-what-it-took.md` | engineers | The problems between a working demo and something safe to leave running, nearly all of them from treating a network resource as though it were local |

Post 4 barely mentions docpreview and is the better one to lead with anywhere that dislikes a product post.
Post 5 is the one to submit somewhere that rewards specifics.

## Order and reuse

Publish 1, 2, 3 in order. 4 and 5 go whenever, and 4 works as a standalone that links back to 1.

Post 1 and post 3 both carry the four-exposer table. Keep it in both — most readers arrive at one post from a
search and never see the other — but keep them in step when the table changes.

## The seat-pricing argument

Post 1 carries it and it may deserve its own post. The shape: documentation contributors are the widest
and most occasional group in a company, per-member billing is built for the opposite shape, and the cheapest
way to control that bill is to reduce who may open a documentation pull request. That prices out the person
who spotted the mistake.

The strongest part is not the money. A contributor without a seat gets a **red check** on the pull request,
in the same list and the same colour as a broken build, plus the mail that goes with it, and cannot clear
it from the branch. A signal that means "this change is wrong" is being spent on "this person is
unlicensed", and once red routinely means nothing, people stop reading the checks that do mean something.

Be accurate about the blast radius: it does **not** block the merge on its own. It is noise and an email.
It only holds a pull request shut where the repository is configured to require every check. The post says
this. Overstating it invites a correction that costs the whole argument.

That observation is about incentives rather than about docpreview, which makes it the most linkable thing
in the series and the best candidate for standing alone.

Against all of it: one VM serves any number of contributors, and the marginal contributor costs nothing.

## Accuracy

Two claims need checking before anything is published.

**Per-seat pricing.** Post 1 describes the model and gives arithmetic rather than quoting a figure, because
published rates change. Confirm the current number and whether reviewers need a paid seat or only editors,
since that changes the total by a lot.

**Frontdoor.** It is implemented and its wire format has not been exercised against a live tenant, which
`www/docs/exposers.md` states plainly. A post that implies otherwise is a support conversation later.
