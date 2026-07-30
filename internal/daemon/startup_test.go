package daemon

import (
	"regexp"
	"testing"
)

// dashboardStages reads the stage keys out of the embedded page's STAGES table, in
// order. Parsed from the real file rather than restated here, because a copy would
// agree with this test and disagree with the page.
func dashboardStages() []string {
	block := regexp.MustCompile(`(?s)const STAGES = \[(.*?)\];`).FindSubmatch(dashboardHTML)
	if block == nil {
		return nil
	}
	var out []string
	for _, m := range regexp.MustCompile(`\["([a-z]+)",`).FindAllSubmatch(block[1], -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// TestStartupProgressIsReportedAndThenGone.
//
// The banner is the only thing on screen during recovery — the page hides the previews
// list and the activity feed while it runs, because their content contradicts it — so
// what it says is the whole of what an operator has. Two properties matter and neither
// is visible from reading the render: a stage must reach /status, and it must stop
// reaching it once recovery is over, or a started daemon serves a page that says it is
// still starting.
func TestStartupProgressIsReportedAndThenGone(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	if got := d.startup.Load(); got != nil {
		t.Fatalf("a fresh daemon already reports a startup stage: %+v", got)
	}

	d.setStartup(StageReaping, "Clearing shares left by the previous run.", 0, 0)
	st, err := d.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Startup == nil {
		t.Fatal("the reaping stage did not reach /status")
	}
	if st.Startup.Stage != StageReaping {
		t.Errorf("stage = %q, want %q", st.Startup.Stage, StageReaping)
	}
	// Total zero is what the page renders as an indeterminate bar. The reap deletes
	// what it finds behind one call and reports a count only when it returns, so a
	// denominator here would be invented.
	if st.Startup.Total != 0 {
		t.Errorf("the reaping stage claims a total of %d", st.Startup.Total)
	}

	d.setStartup(StageRestoring, "Republishing preview URLs.", 3, 11)
	st, err = d.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Startup.Done != 3 || st.Startup.Total != 11 {
		t.Errorf("progress = %d of %d, want 3 of 11", st.Startup.Done, st.Startup.Total)
	}

	// What Run does when recovery finishes.
	d.startup.Store(nil)
	st, err = d.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Startup != nil {
		t.Errorf("a started daemon still reports a stage: %+v", st.Startup)
	}
	if st.Starting {
		t.Error("Starting is true on a daemon that never started recovery")
	}
}

// TestEveryStageTheDashboardKnowsIsOneTheDaemonSets. The page draws a step per stage
// and marks the current one, matching on the string — so a stage renamed on one side
// only leaves the banner with no step highlighted and no error anywhere.
func TestEveryStageTheDashboardKnowsIsOneTheDaemonSets(t *testing.T) {
	// The names in dashboard.html's STAGES table, in its order.
	want := []string{StageReaping, StageRestoring, StageHistory}
	for _, s := range want {
		if s == "" {
			t.Fatal("a stage constant is empty")
		}
	}
	if len(dashboardStages()) != len(want) {
		t.Fatalf("the dashboard knows %d stages, the daemon has %d",
			len(dashboardStages()), len(want))
	}
	for i, s := range dashboardStages() {
		if s != want[i] {
			t.Errorf("stage %d: dashboard says %q, daemon says %q", i, s, want[i])
		}
	}
}
