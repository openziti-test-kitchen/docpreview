# Identity-aware previews

A plan, not a description. Under the `ziti` exposer every request already arrives with the id and name of the
identity that dialed it. Today that is a log line. This document is about making it a feature: a pull request
comment that says who has looked at the preview, and a preview only the pull request's own people can open.

No other exposer can do this. A zrok or Frontdoor preview is a public URL, and the best it can learn about a
visitor is an OAuth email after a login page. On the overlay the identity is established before the first byte of
HTTP, with no login page at all, by the same mutual TLS that carries the traffic.

## What exists today

| Piece | Where | State |
|---|---|---|
| The dialer on every preview request | `zitiConnContext` in `internal/expose/ziti.go` | Built. Id and name land on the request context |
| A log line per identity per preview | `route` in `internal/expose/ziti.go` | Built. Info the first time an identity reaches a preview |
| The dialer on every admin request | `ConnContext` in `internal/daemon/listen.go` | Built, and gates writes through `admin_identities` |
| Proof the router sends the real id | `TestOverlayAdminGrantFollowsTheRealDialer` | Passed 2026-11-06 against a v2.0.6 quickstart |

The last row closes an item [18-exposer-ziti.md](18-exposer-ziti.md) lists as not verified: that `http.Server`
hands `ConnContext` the edge connection unwrapped, and that the id it carries matches the controller's. The test ran
on the ingress listener, not the exposer, but both bind with `Context.Listen` and both assert `edge.ServiceConn`, so
the result carries over.

## The missing input: who is this identity on the SCM?

Every feature below needs one mapping: overlay identity to SCM user. An identity id like `TeiDKUIAN` means nothing to
a pull request. A GitHub login like `alice` means nothing to the overlay. 18-exposer-ziti.md rejects a server-side
allowlist ("Option B") because docpreview would be inventing that mapping. That objection stands. The question is
where a mapping can come from that docpreview does not invent.

| Source | How | Verdict |
|---|---|---|
| The identity's name | Enroll reviewers as `gh-alice`, and docpreview strips the prefix | Cheapest. Anyone who can rename identities on the controller can impersonate. Fine for a network the operator owns outright |
| A role attribute | `#gh-alice` on the identity | Same trust as the name, and survives a rename. Several attributes per identity cover several SCMs |
| The identity's `externalId` | Set when the identity is created | Meant for this. **Not verified:** whether `GetDialerIdentityName` has a sibling that returns it, or docpreview needs a management-API lookup per new dialer |
| An external JWT signer | The reviewer signs in through an IdP the controller trusts, and the IdP subject is the identity | Strongest, and no enrollment ceremony. GitHub is not an OIDC provider for users, so the IdP subject still has to be joined to a GitHub login somewhere |

**Recommended: role attributes, `#<scm>-<login>`.** They are set by whoever administers the network, which is
the same person who already decides who may dial the service at all. They need no new API. Reading them back takes management-API
credentials, which `configure ziti` uses and `serve` does not hold today. An identity name can be changed by the same people,
but nobody expects a rename to change a grant, so the attribute is the less surprising place to keep it.

`docpreview identity add` ([18-exposer-ziti.md](18-exposer-ziti.md), step 9) is the natural place to set it:
`docpreview identity add alice -github alice` creates the identity, adds `#docpreview-reader` and `#github-alice`,
and prints the enrollment token.

## Three features, in the order to build them

### 1. "Seen by" in the pull request comment

docpreview edits one comment per pull request in place. Add a line under the preview link:

> Seen by alice, bob

The data is already there. `route` knows the first time each identity reaches each preview. The work is carrying
that event from the exposer to the daemon, which owns the comment, and debouncing the comment edit so twenty
reviewers opening the link in one minute is one edit, not twenty.

This needs no mapping to be useful. Without one, it says "Seen by 2 identities". With one, it says names. It is
also the feature that makes the overlay *visible*: on a public preview nobody can say who has looked.

Cost: an event from exposer to daemon (the exposer has no way to reach the daemon today), a debounced comment
edit, and a column or table for viewers per preview. **Privacy:** a reviewer who opens a link did not agree to be
named on the pull request. Make it a project setting, off by default.

### 2. A preview only the pull request's people can open

This is 18-exposer-ziti.md's Option C, step 7. The rule it asks for, and could not name, is: the pull request's
author, its requested reviewers, and anyone carrying an operator attribute such as `#docpreview-operator`.

With the mapping above, `route` checks the dialer's `#github-<login>` attribute against that set before
serving. A dialer outside it gets a 403 that names the pull request and the identity, the same shape as the
`admin_identities` refusal.

Cost: docpreview must fetch the reviewer list, which it does not do today, and refresh it when a reviewer is
added. GitHub sends `pull_request.review_requested` and Bitbucket sends `pullrequest:updated` on a reviewer change, so
this can ride webhooks rather than a poll. The attributes have to be read from the controller,
and cached, because the dialer's attributes are not on the connection. **Not verified:** whether the SDK exposes
the dialer's attributes on the hosting side, or only its id and name.

The boundary lives in docpreview's process, not on the controller. That is the trade 18-exposer-ziti.md already
accepts for Option C, and `docs/design/10-security.md` must say so the day this ships.

### 3. A real domain instead of `.ziti`

Intercept `*.preview.example.com`, a zone the organization owns, instead of `*.docpreview.ziti`. On a machine
without the tunneler the link then fails as NXDOMAIN on a real domain, rather than as an invalid TLD that chat
clients and link checkers flag. [ziti-native-previews.md](ziti-native-previews.md) lists this as an open question.

This is configuration, not code: `configure ziti -domain preview.example.com` already sets both the intercept
and `exposer.ziti.domain`. The work is documenting it and checking that public DNS has no wildcard record for the
zone, because a public answer would beat the tunneler on any machine where the tunneler is off. **Not verified:**
how Ziti Desktop Edge orders its NRPT rule against a public resolver for a real zone.

## Public and private at once

A team may want both: a zrok link for outside contributors and a ziti link for the team. That is the
multi-exposer work in [21-multi-exposer.md](21-multi-exposer.md), and nothing here depends on it. Once it exists,
the comment carries two links, and "Seen by" counts only the overlay one, since zrok cannot say who opened it.

## Not verified

- Whether the hosting side can read a dialer's `externalId` or role attributes without a management-API call.
- How Ziti Desktop Edge's NRPT rule for a real zone interacts with a public resolver.
- The webhook events named under feature 2 are the expected ones, not checked against what docpreview subscribes
  to today.
