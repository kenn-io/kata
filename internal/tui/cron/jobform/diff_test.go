package jobform

import (
	"testing"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
)

func TestDiffKeepsUnresolvedReferenceChangesVisible(t *testing.T) {
	a := defs.JobDef{Action: defs.Action{Kind: "execute", Prompt: "work"}, Issue: &defs.IssuePolicy{Kind: "existing", UID: "01K00000000000000000000001"}}
	b := a
	b.Issue = &defs.IssuePolicy{Kind: "existing", UID: "01K00000000000000000000002"}
	got := Diff(a, b, "Example", "Example")
	if len(got) != 1 || got[0].Field != "existing issue" || got[0].Yours != "previous reference" || got[0].Kata != "different reference" {
		t.Fatalf("reference change: %+v", got)
	}
}
