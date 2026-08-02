package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// The four states a publication can be in, once the account and the database are
// laid side by side. They are the whole point of the command: a flat list of what
// the account holds is what `zrok2 list shares` already prints, and it cannot tell
// an orphan from a preview that is working exactly as intended.
//
// An orphan and a missing share are opposite problems. An orphan is a share and a
// reserved name being paid for with nothing pointing at them. A missing share is a
// pull request comment advertising a URL that now 404s. Both are invisible today,
// and a report that lumped them together as "mismatch" would be useless for either.
const (
	// stateOK — the account holds it and the database claims it.
	stateOK = "ok"

	// stateOrphan — the account holds it and nothing in the database claims it.
	stateOrphan = "orphan"

	// stateMissing — the database recorded a share that the account does not hold.
	stateMissing = "missing"

	// stateNever — a recorded preview that has never published anything. Not a
	// fault: a queued preview, or one whose first build failed, looks exactly like
	// this. It is here because "which recorded previews have no share" is one of the
	// three questions, and answering it with only the alarming half would make the
	// normal state of a fresh preview look like data loss.
	stateNever = "never"
)

// stateRank puts the two problems above the two non-problems.
//
// Sorted rather than left in database order because the operator running this has
// either "what am I paying for" or "did something leak" in mind, and both answers
// are in the first few lines or the command has not helped.
var stateRank = map[string]int{
	stateOrphan:  0,
	stateMissing: 1,
	stateOK:      2,
	stateNever:   3,
}

// shareRow is one line of the report: one publication, from one or both sides.
type shareRow struct {
	State string `json:"state"`

	// Key is the publication key — expose.Spec.Key(), so "<preview>" for a branch
	// share and "<preview>/<build>" for a build's own. It is the only identifier the
	// two sides share, which is what makes the comparison possible at all.
	Key string `json:"key"`

	// PR is the pull request, from the database. Empty for an orphan, because the
	// account does not know what a share was for — only what it was tagged with.
	PR string `json:"pull_request,omitempty"`

	// Share is the exposer's own handle for the publication, a zrok share token.
	// Empty for a missing or never-published one, there being nothing to name.
	Share string `json:"share,omitempty"`

	URL string `json:"url,omitempty"`
}

// shareTally is the summary line, and the JSON payload's counts.
type shareTally struct {
	Held        int `json:"held"`
	Matched     int `json:"matched"`
	Orphaned    int `json:"orphaned"`
	Missing     int `json:"missing"`
	Unpublished int `json:"unpublished"`
}

// recordedShare is one publication the database claims exists.
//
// Flattened out of previews and builds before the comparison, because both are
// publications of the same kind as far as the account is concerned — one share
// each, one reserved name each — and a comparison that only knew about previews
// would report every build share as an orphan.
type recordedShare struct {
	key string
	pr  string
	url string

	// published is false for a preview row that has never had a share. Carried
	// rather than inferred from an empty url, so that the "never" state is a
	// decision made once, here, where the reason can be written down.
	published bool
}

func cmdShares(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("shares: no subcommand")
	}

	switch args[0] {
	case "list":
		return cmdSharesList(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown shares subcommand %q", args[0])
	}
}

// cmdSharesList compares what the exposer's account holds against what the
// database claims, and prints the difference.
//
// **Read-only, and that is a design decision rather than an omission.** Reap is the
// thing that deletes, and it deletes from the daemon with the database's claim as
// its keep-set. An audit command that also deleted would be one nobody dares run on
// a live account — which would leave the account unauditable, which is the gap this
// closes. So this command answers the question and names what would act on the
// answer; it never acts.
//
// Safe to run against a running daemon, and this is the one command where that needs
// saying. `internal/expose/CLAUDE.md` records that two processes sharing one exposer
// account delete each other's live shares — that hazard is entirely in Reap. Listing
// is one GET, and it takes nothing away.
func cmdSharesList(args []string) error {
	fs := flag.NewFlagSet("shares list", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	// warn, not info: the output is a table meant to be read, and the exposer logs a
	// paragraph about the environment it validated at info level. Interleaved with the
	// rows it makes the table unreadable — but a listing that fails needs those lines,
	// hence the flag.
	logLevel := fs.String("log-level", "warn", "debug, info, warn, or error")
	asJSON := fs.Bool("json", false, "emit the report as JSON instead of a table")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	w, err := setup(*configPath, *logLevel)
	if err != nil {
		return err
	}
	defer w.Close()

	// Not every exposer can answer this, and the honest failure names which can.
	// Adopter rather than a new interface method, because "list what this account
	// holds, keyed by publication key" is exactly what Adoptable already does for
	// startup — and an audit that read from a second, separately-maintained listing
	// could disagree with the one the daemon acts on, which is worse than no audit.
	ad, ok := w.exposer.(expose.Adopter)
	if !ok {
		return fmt.Errorf("the %s exposer cannot report what it holds remotely, so there is "+
			"nothing to compare the database against.\n\nOnly zrok2 can, today. "+
			"For what the database records, see the dashboard's preview list, or run:\n"+
			"    docpreview doctor -config %s", w.exposer.Kind(), *configPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Validate first, because the two ways the listing fails — an environment that was
	// never enabled and an account token revoked server-side — both surface from the
	// raw listing as an opaque 401 with no fix in it, and Validate's errors name the
	// command that repairs each.
	if err := w.exposer.Validate(ctx); err != nil {
		if errors.Is(err, vault.ErrLocked) {
			// serve downgrades this to a warning and re-checks after unlock, because
			// the page that unlocks the vault is served by that daemon. Here there is
			// no later: a one-shot command with no credential cannot list anything, so
			// it fails — with the fix, not with a nil dereference further down.
			return fmt.Errorf("the %s exposer needs a credential from the vault, and the "+
				"vault is locked (%s).\n\nUnlock it by starting the daemon and using the "+
				"dashboard, or set vault.key_source in %s",
				w.exposer.Kind(), w.cfg.VaultPath(), *configPath)
		}
		return fmt.Errorf("exposer %s: %w", w.exposer.Kind(), err)
	}

	held, err := ad.Adoptable(ctx)
	if err != nil {
		return err
	}

	recorded, err := recordedShares(ctx, w.store)
	if err != nil {
		return err
	}

	rows, tally := auditShares(held, recorded)

	if *asJSON {
		return writeSharesJSON(os.Stdout, w.exposer.Kind(), rows, tally)
	}
	writeSharesTable(os.Stdout, rows, tally)
	return nil
}

// recordedShares flattens the database's claim into one entry per publication.
//
// Two tables, because a preview and each of its builds publish separately and each
// one is a share plus a reserved name on the account. BuildsFor per preview rather
// than RecentBuilds, deliberately: RecentBuilds is bounded, and an audit that
// silently stopped looking after fifty rows would report older build shares as
// orphans — the exact false alarm this command exists to remove.
func recordedShares(ctx context.Context, st *store.Store) ([]recordedShare, error) {
	previews, err := st.ListPreviews(ctx)
	if err != nil {
		return nil, err
	}

	var out []recordedShare
	for _, p := range previews {
		// Name, not URL. The name is the object the account's quota counts and the
		// thing a share is bound to; a row with a name and no URL is a publish that
		// got partway, which is worth reporting rather than skipping.
		out = append(out, recordedShare{
			key:       expose.Spec{PreviewID: p.PreviewID}.Key(),
			pr:        p.PR.String(),
			url:       p.URL,
			published: p.Name != "",
		})

		builds, err := st.BuildsFor(ctx, p.PreviewID)
		if err != nil {
			return nil, err
		}
		for _, b := range builds {
			// Most builds have no share of their own — a failed one, or a daemon with
			// per-build publishing off — and an unpublished build is not a preview
			// waiting to publish. There is nothing to report about it.
			if b.Name == "" {
				continue
			}
			// Spec.Key rather than the "/" join written out again. One definition, or
			// the audit and the daemon key the same publication two ways and every
			// build share reads as an orphan.
			out = append(out, recordedShare{
				key:       expose.Spec{PreviewID: b.PreviewID, BuildID: b.BuildID}.Key(),
				pr:        b.PR.String(),
				url:       b.URL,
				published: true,
			})
		}
	}
	return out, nil
}

// auditShares is the comparison, and it is pure so that it can be tested without an
// account or a database.
//
// held is keyed by publication key, as the exposer reports it; recorded is the
// database's claim. Every key from either side produces exactly one row, which is
// the invariant that makes the summary line add up.
func auditShares(held map[string]expose.Adoptable, recorded []recordedShare) ([]shareRow, shareTally) {
	tally := shareTally{Held: len(held)}

	var rows []shareRow
	claimed := make(map[string]bool, len(recorded))

	for _, r := range recorded {
		claimed[r.key] = true

		switch a, ok := held[r.key]; {
		case ok:
			tally.Matched++
			// The database's URL, not the share's origin: the recorded one carries the
			// baseURL the site was built for, and it is the string that actually went
			// into the pull request comment. The origin is the fallback for a row that
			// has a name but no URL.
			url := r.url
			if url == "" {
				url = expose.JoinURL(a.Origin, "")
			}
			rows = append(rows, shareRow{State: stateOK, Key: r.key, PR: r.pr,
				Share: a.Handle, URL: url})
		case !r.published:
			// Never published, so nothing is missing. Counted apart from the missing
			// ones because the fix is different: this one is waiting for a build, that
			// one has lost a share somebody has the URL of.
			tally.Unpublished++
			rows = append(rows, shareRow{State: stateNever, Key: r.key, PR: r.pr})
		default:
			tally.Missing++
			rows = append(rows, shareRow{State: stateMissing, Key: r.key, PR: r.pr, URL: r.url})
		}
	}

	for key, a := range held {
		if claimed[key] {
			continue
		}
		tally.Orphaned++
		rows = append(rows, shareRow{State: stateOrphan, Key: key,
			Share: a.Handle, URL: expose.JoinURL(a.Origin, "")})
	}

	// Problems first, then stable by key — a map was iterated to build part of this,
	// and a report whose lines move between runs cannot be diffed against yesterday's.
	sort.SliceStable(rows, func(i, j int) bool {
		if stateRank[rows[i].State] != stateRank[rows[j].State] {
			return stateRank[rows[i].State] < stateRank[rows[j].State]
		}
		return rows[i].Key < rows[j].Key
	})

	return rows, tally
}

// writeSharesTable prints the report for a human, which is the default because the
// question being asked is one somebody is asking themselves.
func writeSharesTable(out io.Writer, rows []shareRow, tally shareTally) {
	header := []string{"STATE", "PUBLICATION", "PULL REQUEST", "SHARE", "URL"}
	cells := make([][]string, 0, len(rows)+1)
	cells = append(cells, header)
	for _, r := range rows {
		cells = append(cells, []string{r.State, r.Key, dash(r.PR), dash(r.Share), dash(r.URL)})
	}

	// Widths from the data. Preview keys are twelve characters and build keys
	// thirty-six, so a fixed width is either truncating or mostly whitespace.
	width := make([]int, len(header))
	for _, row := range cells {
		for i, c := range row {
			if len(c) > width[i] {
				width[i] = len(c)
			}
		}
	}

	for _, row := range cells {
		var sb strings.Builder
		for i, c := range row {
			// The last column is never padded: it is the URL, it is the widest thing
			// here, and trailing spaces to no purpose defeat copying a line out.
			if i == len(row)-1 {
				sb.WriteString(c)
				break
			}
			fmt.Fprintf(&sb, "%-*s  ", width[i], c)
		}
		fmt.Fprintln(out, strings.TrimRight(sb.String(), " "))
	}

	// The counts, in one sentence, because the table runs to dozens of rows on a
	// modest account and the answer to "did something leak" is a number.
	//
	// "publication" rather than "preview" in the middle clause: a build share is one
	// too, and on a busy repository most of them are — calling a missing build share
	// a missing preview sends the reader looking at the wrong row.
	fmt.Fprintf(out, "\n%s held: %d matched the database, %d orphaned. "+
		"%s no longer on the account; %s never published.\n",
		plural(tally.Held, "share", "shares"), tally.Matched, tally.Orphaned,
		plural(tally.Missing, "recorded publication is", "recorded publications are"),
		plural(tally.Unpublished, "preview has", "previews have"))

	// Guidance only for what was actually found. A clean account should not be told
	// how to fix two problems it does not have.
	if tally.Orphaned > 0 {
		fmt.Fprintf(out, "\nAn orphan costs a share and a reserved name against the account's quota "+
			"and serves\na URL nothing links to. The daemon's Reap deletes them at its next sweep, "+
			"keeping\nwhat the database claims; nothing in this command deletes anything.\n")
	}
	if tally.Missing > 0 {
		// Rebuilding a commit produces a new build id, while the build share's name embeds
		// the commit and so is unchanged — see expose.Collides. Both build rows then carry
		// one URL: one holds the share, and the other is a record that has lost its share
		// rather than a preview a reviewer cannot open.
		fmt.Fprintf(out, "\nA recorded publication with no share is a URL that a pull request comment "+
			"or the\ndashboard still offers and that now 404s. Rebuild the preview from the "+
			"dashboard\nto republish it.\n\nOne exception: two builds of the same commit render to "+
			"the same name, so if the\nURL above also appears on an `ok` row, it still resolves — "+
			"through the sibling\nbuild's share rather than through this one.\n")
	}
}

// writeSharesJSON emits the same report machine-readably.
//
// It earns its place because the whole value here is a comparison somebody wants to
// keep running — "alert me when this account grows an orphan" is a cron job, and jq
// over a padded table is how that goes wrong.
//
// The exit status is deliberately zero either way. A non-zero exit for "found an
// orphan" would conflate a finding with a failure, and a monitoring check wants to
// distinguish "three orphans" from "could not reach the controller".
func writeSharesJSON(out io.Writer, kind string, rows []shareRow, tally shareTally) error {
	if rows == nil {
		// So the field is [] rather than null. A consumer iterating it should not
		// have to special-case an empty account.
		rows = []shareRow{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Exposer string     `json:"exposer"`
		Counts  shareTally `json:"counts"`
		Shares  []shareRow `json:"shares"`
	}{Exposer: kind, Counts: tally, Shares: rows})
}

// plural renders a count with the right noun, because "1 recorded publications are
// no longer on the account" reads as a bug in the tool and makes a reader doubt the
// number next to it.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// dash renders an empty cell as something visible, so a blank column reads as "the
// report has no answer here" rather than as a rendering fault.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
