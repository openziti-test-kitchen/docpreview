package scm

import (
	"strings"
	"testing"
)

func TestCommentHeadingDefaultsAndOverrides(t *testing.T) {
	if got := RenderComment(Report{PreviewID: "abc123"}); !strings.Contains(got, "**NetFoundry Preview**") {
		t.Errorf("an unset title must render the default heading, got:\n%s", got)
	}
	got := RenderComment(Report{PreviewID: "abc123", Title: "Docs preview"})
	if !strings.Contains(got, "**Docs preview**") || strings.Contains(got, DefaultTitle) {
		t.Errorf("a configured title must replace the default heading, got:\n%s", got)
	}
	if !HasMarker(got, "abc123") {
		t.Error("the marker must survive a custom title, or the comment is never found again")
	}
}
