package expose

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestParseTagReadsEverySpelling pins which share tags count as this program's and whose they are.
//
// The docpreview: spelling is what v0.4.9 and earlier wrote, and the bare nfpreview:<key> is what
// v0.5.1 and earlier wrote. Anything else, including a tag that only contains the prefix,
// belongs to somebody else.
func TestParseTagReadsEverySpelling(t *testing.T) {
	cases := []struct {
		tag      string
		instance string
		key      string
		ours     bool
	}{
		{"nfpreview:abc123", "", "abc123", true},
		{"docpreview:abc123", "", "abc123", true},
		{"nfpreview:abc123/20261105-145219-bbd9551", "", "abc123/20261105-145219-bbd9551", true},
		{"docpreview:abc123/20261105-145219-bbd9551", "", "abc123/20261105-145219-bbd9551", true},
		{"nfpreview:0a1b2c3d4e5f:abc123", "0a1b2c3d4e5f", "abc123", true},
		{"nfpreview:0a1b2c3d4e5f:abc123/20261105-145219-bbd9551", "0a1b2c3d4e5f", "abc123/20261105-145219-bbd9551", true},
		{"nfpreview::abc123", "", "", false},
		{"nfpreview:0a1b2c3d4e5f:", "", "", false},
		{"nfpreview:a:b:c", "", "", false},
		{"nfpreview:a/b:c", "", "", false},
		{"docpreview:0a1b2c3d4e5f:abc123", "", "", false},
		{"", "", "", false},
		{"-", "", "", false},
		{"atrium-hub", "", "", false},
		{"mypreview:abc123", "", "", false},
		{"x-nfpreview:abc123", "", "", false},
		{"http://127.0.0.1:8080/docpreview:abc123", "", "", false},
	}
	for _, c := range cases {
		instance, key, ours := parseTag(c.tag)
		if ours != c.ours || key != c.key || instance != c.instance {
			t.Errorf("parseTag(%q) = %q, %q, %v; want %q, %q, %v",
				c.tag, instance, key, ours, c.instance, c.key, c.ours)
		}
	}
}

// TestStampedTagRoundTrips: what Publish writes is what Reap and Adoptable read back.
func TestStampedTagRoundTrips(t *testing.T) {
	id, err := NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidInstanceID(id) {
		t.Fatalf("NewInstanceID returned %q, which ValidInstanceID refuses", id)
	}
	for _, key := range []string{"abc123", "abc123/20261105-145219-bbd9551"} {
		for _, inst := range []string{"", id} {
			tag := stampTag(inst, key)
			gotInst, gotKey, ours := parseTag(tag)
			if !ours || gotInst != inst || gotKey != key {
				t.Errorf("stampTag(%q, %q) = %q, read back as %q, %q, %v", inst, key, tag, gotInst, gotKey, ours)
			}
		}
	}
}

// TestListFilterMatchesEverySpelling: ListShares filters Target by substring on the controller,
// so the one filter has to be a substring of every spelling or one of them is never listed.
func TestListFilterMatchesEverySpelling(t *testing.T) {
	for _, tag := range []string{
		stampTag("", "k"), stampTag("0a1b2c3d4e5f", "k"), legacyTargetPrefix + "k",
	} {
		if !strings.Contains(tag, listTargetFilter) {
			t.Errorf("listTargetFilter %q is not a substring of %q", listTargetFilter, tag)
		}
	}
}

// listedShare is one share as a controller lists it.
type listedShare struct{ token, tag string }

// reapOf returns the tokens the installation `mine` would delete from a listing, which is the
// decision Zrok.Reap and Frontdoor.Reap both make before they delete anything.
func reapOf(mine string, listing []listedShare, keep map[string]bool) []string {
	var doomed []string
	for _, s := range listing {
		if doomedShare(mine, s.tag, keep) {
			doomed = append(doomed, s.token)
		}
	}
	sort.Strings(doomed)
	return doomed
}

// TestASecondInstallationWithAnEmptyDatabaseReapsNothingOfTheFirst is the failure this stamp
// exists for: two daemons on one zrok environment, the second started on a fresh database.
//
// Its keep-set is empty, and before the stamp every tagged share absent from keep was an orphan,
// so the second daemon's startup deleted every live preview of the first.
func TestASecondInstallationWithAnEmptyDatabaseReapsNothingOfTheFirst(t *testing.T) {
	const first, second = "aaaaaaaaaaaa", "bbbbbbbbbbbb"

	// What the shared environment holds: the first installation's live shares, in all three
	// spellings it may have written over its life, and somebody's hand-made share.
	listing := []listedShare{
		{"t-stamped", stampTag(first, "p1")},
		{"t-stamped-build", stampTag(first, "p1/build-7")},
		{"t-v051", "nfpreview:p2"},
		{"t-v049", "docpreview:p3"},
		{"t-by-hand", "http://127.0.0.1:8080"},
	}

	if got := reapOf(second, listing, nil); len(got) != 0 {
		t.Fatalf("a second installation with an empty database would delete %v", got)
	}
	if got := reapOf(second, listing, map[string]bool{}); len(got) != 0 {
		t.Fatalf("a second installation with an empty keep-set would delete %v", got)
	}

	// And it still cleans up after itself: its own orphan goes, the first's shares stay.
	listing = append(listing, listedShare{"t-second-orphan", stampTag(second, "p9")},
		listedShare{"t-second-kept", stampTag(second, "p8")})
	got := reapOf(second, listing, map[string]bool{"p8": true})
	if len(got) != 1 || got[0] != "t-second-orphan" {
		t.Fatalf("second installation reaps %v, want exactly [t-second-orphan]", got)
	}

	// The first, with its own database, reaps only its own stamped orphan. The unstamped
	// shares are not attributable and survive even though nothing claims them.
	got = reapOf(first, listing, map[string]bool{"p1": true})
	if len(got) != 1 || got[0] != "t-stamped-build" {
		t.Fatalf("first installation reaps %v, want exactly [t-stamped-build]", got)
	}
}

// TestAdoptionOffersOwnAndUnstampedSharesOnly: an upgraded daemon has to take over the shares
// the previous release left, which carry no stamp, and must never be offered another
// installation's.
func TestAdoptionOffersOwnAndUnstampedSharesOnly(t *testing.T) {
	const mine, other = "aaaaaaaaaaaa", "bbbbbbbbbbbb"
	out := map[string]Adoptable{}

	offerAdoption(out, mine, stampTag(mine, "p1"), "t-mine", "p1.example.test")
	offerAdoption(out, mine, "nfpreview:p2", "t-v051", "https://p2.example.test")
	offerAdoption(out, mine, "docpreview:p3", "t-v049", "p3.example.test")
	offerAdoption(out, mine, stampTag(other, "p4"), "t-other", "p4.example.test")
	offerAdoption(out, mine, stampTag(mine, "p5"), "t-no-endpoint", "")
	offerAdoption(out, mine, "someone-elses-thing", "t-by-hand", "x.example.test")

	want := map[string]Adoptable{
		"p1": {Handle: "t-mine", Origin: "https://p1.example.test"},
		"p2": {Handle: "t-v051", Origin: "https://p2.example.test", Unstamped: true},
		"p3": {Handle: "t-v049", Origin: "https://p3.example.test", Unstamped: true},
	}
	if len(out) != len(want) {
		t.Fatalf("offered %v, want %v", out, want)
	}
	for k, w := range want {
		if out[k] != w {
			t.Errorf("offered[%q] = %+v, want %+v", k, out[k], w)
		}
	}

	// A key held twice: the stamped share wins whichever order the controller lists them in.
	for _, order := range [][2]listedShare{
		{{"t-old", "nfpreview:p6"}, {"t-new", stampTag(mine, "p6")}},
		{{"t-new", stampTag(mine, "p6")}, {"t-old", "nfpreview:p6"}},
	} {
		out := map[string]Adoptable{}
		for _, s := range order {
			offerAdoption(out, mine, s.tag, s.token, "p6.example.test")
		}
		if out["p6"].Handle != "t-new" || out["p6"].Unstamped {
			t.Errorf("listed as %v, offered %+v; want the stamped share", order, out["p6"])
		}
	}
}

// TestANameIsReclaimedOnlyFromOwnOrUnstampedShares: Publish deletes a share that holds the name
// it needs. That must never be a share another installation stamped.
func TestANameIsReclaimedOnlyFromOwnOrUnstampedShares(t *testing.T) {
	const mine, other = "aaaaaaaaaaaa", "bbbbbbbbbbbb"
	cases := []struct {
		tag  string
		want bool
	}{
		{stampTag(mine, "p1"), true},
		{"nfpreview:p1", true},
		{"docpreview:p1", true},
		{stampTag(other, "p1"), false},
		{"someone-elses-thing", false},
		{"", false},
	}
	for _, c := range cases {
		if got := reclaimable(mine, c.tag); got != c.want {
			t.Errorf("reclaimable(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

// TestFrontdoorSecondInstallationReapsNothingOfTheFirst is the same failure end to end against
// the fake gateway: two exposers on one tenant, the second with nothing in its database.
func TestFrontdoorSecondInstallationReapsNothingOfTheFirst(t *testing.T) {
	fake := &fakeFrontdoor{
		listPages: []string{`{"content":[
			{"id":"first-live",   "tag":"nfpreview:aaaaaaaaaaaa:p1"},
			{"id":"first-build",  "tag":"nfpreview:aaaaaaaaaaaa:p1/build-7"},
			{"id":"unstamped",    "tag":"nfpreview:p2"},
			{"id":"second-orphan","tag":"nfpreview:bbbbbbbbbbbb:p9"},
			{"id":"not-ours",     "tag":"someone-elses-thing"}
		]}`},
	}
	second, _ := newTestFrontdoor(t, fake)
	second.SetInstance("bbbbbbbbbbbb")

	if err := second.Reap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := fake.deletions(); len(got) != 1 || got[0] != "second-orphan" {
		t.Errorf("deletions = %v, want exactly [second-orphan]", got)
	}
}

// TestFrontdoorPublishStampsTheTag: the tag a share is created with carries the installation id,
// or the reap above could never recognise this installation's own orphans.
func TestFrontdoorPublishStampsTheTag(t *testing.T) {
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)
	fd.SetInstance("bbbbbbbbbbbb")

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "stamped", BaseURL: "/"}, nil); err != nil {
		t.Fatal(err)
	}
	creates := fake.creates()
	if len(creates) != 1 || !strings.Contains(creates[0], `"tag":"nfpreview:bbbbbbbbbbbb:p1"`) {
		t.Errorf("create bodies = %v, want one tagged nfpreview:bbbbbbbbbbbb:p1", creates)
	}
}
