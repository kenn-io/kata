package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func predicateFactEvent(n int, kind, body string) FoldEvent {
	return FoldEvent{UID: closeProofID(n), OriginInstanceUID: closeProofID(900), ProjectUID: closeProofID(2), IssueUID: closeProofID(1), Type: kind, Actor: "member", HLCPhysicalMS: int64(n), Payload: jsontext.Value(body)}
}

func TestScopedFoldPredicateFactsEffectiveABA(t *testing.T) {
	child, b, c := closeProofID(1), closeProofID(3), closeProofID(4)
	baseline := predicateFactEvent(10, "issue.created", `{"uid":"`+child+`","author":"member","status":"open","metadata":{"key":"B"},"links":[{"type":"parent","to_issue_uid":"`+b+`","author":"member"}]}`)
	project := predicateFactEvent(5, "project.federation_enabled", `{"metadata":{"key":"B"}}`)
	parentC := predicateFactEvent(20, "issue.linked", `{"from_uid":"`+child+`","to_uid":"`+c+`","type":"parent"}`)
	parentB := predicateFactEvent(30, "issue.linked", `{"from_uid":"`+child+`","to_uid":"`+b+`","type":"parent"}`)
	issueC := predicateFactEvent(21, "issue.metadata_updated", `{"diff":{"key":{"from":"B","to":"C"}}}`)
	issueB := predicateFactEvent(31, "issue.metadata_updated", `{"diff":{"key":{"from":"C","to":"B"}}}`)
	projectC := predicateFactEvent(22, "project.metadata_updated", `{"diff":{"key":{"from":"B","to":"C"}}}`)
	projectB := predicateFactEvent(32, "project.metadata_updated", `{"diff":{"key":{"from":"C","to":"B"}}}`)
	sameSnapshot := predicateFactEvent(40, "issue.snapshot", `{"uid":"`+child+`","author":"member","status":"open","metadata": {"key": "B"},"links":[{"type":"parent","to_issue_uid":"`+b+`","author":"member"}]}`)
	staleRemoval := predicateFactEvent(45, "issue.unlinked", `{"from_uid":"`+child+`","to_uid":"`+c+`","type":"parent"}`)
	author := predicateFactEvent(50, "project.author_rewritten", `{"from":"member","to":"updated-label"}`)
	events := []FoldEvent{sameSnapshot, projectB, issueC, baseline, project, parentC, issueB, parentB, projectC, staleRemoval, author, issueC, parentB}
	p := FoldEvents(events)
	facts := p.ScopedPredicateFacts()
	require.Equal(t, ScopedParentWriter{ParentUID: b, Clock: clockOf(parentB)}, facts.Parents[child], "same-parent snapshot, duplicate and author-only link-clock changes are not relationship writes")
	require.Equal(t, []FoldClock{clockOf(issueB)}, facts.IssueMetadata[child], "metadata ABA retains last effective writer even when final row equals baseline")
	require.Equal(t, []FoldClock{clockOf(projectB)}, facts.ProjectMetadata[closeProofID(2)])
	require.JSONEq(t, `{"key":"B"}`, string(p.IssueMetadata[child]))
	require.Equal(t, clockOf(author), p.Links[FoldLinkKey{FromUID: child, ToUID: b, Type: "parent"}].Clock, "native author clock remains native; facts do not redefine link conflict semantics")
	// Returned working maps are independent and never expose the accumulator.
	facts.Parents[child] = ScopedParentWriter{}
	delete(facts.IssueMetadata, child)
	delete(facts.ProjectMetadata, closeProofID(2))
	again := p.ScopedPredicateFacts()
	require.Equal(t, b, again.Parents[child].ParentUID)
	require.Equal(t, []FoldClock{clockOf(issueB)}, again.IssueMetadata[child])
	require.Equal(t, []FoldClock{clockOf(projectB)}, again.ProjectMetadata[closeProofID(2)])
	remove := predicateFactEvent(60, "issue.unlinked", `{"from_uid":"`+child+`","to_uid":"`+b+`","type":"parent"}`)
	absent := FoldEvents(append(events, remove)).ScopedPredicateFacts()
	require.Equal(t, ScopedParentWriter{ParentUID: "", Clock: clockOf(remove)}, absent.Parents[child], "effective parent absence keeps its writer")
	restored := predicateFactEvent(80, "issue.linked", `{"from_uid":"`+child+`","to_uid":"`+b+`","type":"parent"}`)
	final := FoldEvents(append(events, remove, restored)).ScopedPredicateFacts()
	require.Equal(t, ScopedParentWriter{ParentUID: b, Clock: clockOf(restored)}, final.Parents[child])
}

func TestScopedFoldPredicateFactsNativeMetadataNormalization(t *testing.T) {
	child := closeProofID(1)
	baseline := predicateFactEvent(10, "issue.created", `{"uid":"`+child+`","metadata":{"bucket":{"left":1,"right":1}}}`)
	left := predicateFactEvent(20, "issue.metadata_updated", `{"diff":{"bucket":{"from":{"left":1,"right":1},"to":{"left":2,"right":1}}}}`)
	right := predicateFactEvent(30, "issue.metadata_updated", `{"diff":{"bucket":{"from":{"left":1,"right":1},"to":{"left":1,"right":3}}}}`)
	noop := predicateFactEvent(40, "issue.snapshot", `{"uid":"`+child+`","metadata":{"bucket":{"right":3,"left":2},"absent":null}}`)
	p := FoldEvents([]FoldEvent{right, baseline, left, noop})
	require.JSONEq(t, `{"bucket":{"left":2,"right":3},"absent":null}`, string(p.IssueMetadata[child]))
	require.Equal(t, []FoldClock{clockOf(left), clockOf(right)}, p.ScopedPredicateFacts().IssueMetadata[child], "native recursive merge changes writer; normalized ordering and absent-null snapshot do not")
	empty := FoldEvents([]FoldEvent{predicateFactEvent(1, "issue.created", `{"uid":"`+child+`","metadata":{}}`), predicateFactEvent(2, "issue.snapshot", `{"uid":"`+child+`","metadata":{"absent":null}}`)})
	require.Empty(t, empty.ScopedPredicateFacts().IssueMetadata, "native absent/null metadata no-op has no effective writer")
}

func TestScopedFoldPredicateFactsRejectJSON(t *testing.T) {
	for _, value := range []any{ScopedFoldPredicateFacts{}, ScopedParentWriter{}} {
		_, err := json.Marshal(value)
		require.Error(t, err, "private working facts are never wire/capture authority")
	}
	var facts ScopedFoldPredicateFacts
	require.Error(t, json.Unmarshal([]byte(`{}`), &facts))
	var parent ScopedParentWriter
	require.Error(t, json.Unmarshal([]byte(`{}`), &parent))
}

func TestScopedFoldPredicateFactsSurvivingMetadataWriters(t *testing.T) {
	child := closeProofID(1)
	for _, nested := range []bool{false, true} {
		name := "top_level"
		base := `{"x":"B","y":0}`
		xC := `{"diff":{"x":{"from":"B","to":"C"}}}`
		xB := `{"diff":{"x":{"from":"C","to":"B"}}}`
		y := `{"diff":{"y":{"from":0,"to":1}}}`
		want := `{"x":"B","y":1}`
		if nested {
			name = "same_nested_bucket"
			base = `{"bucket":{"x":"B","y":0}}`
			xC = `{"diff":{"bucket":{"from":{"x":"B","y":0},"to":{"x":"C","y":0}}}}`
			xB = `{"diff":{"bucket":{"from":{"x":"C","y":0},"to":{"x":"B","y":0}}}}`
			y = `{"diff":{"bucket":{"from":{"x":"B","y":0},"to":{"x":"B","y":1}}}}`
			want = `{"bucket":{"x":"B","y":1}}`
		}
		t.Run(name, func(t *testing.T) {
			baseline := predicateFactEvent(10, "issue.created", `{"uid":"`+child+`","metadata":`+base+`}`)
			c20, b30, y40 := predicateFactEvent(20, "issue.metadata_updated", xC), predicateFactEvent(30, "issue.metadata_updated", xB), predicateFactEvent(40, "issue.metadata_updated", y)
			prior := FoldEvents([]FoldEvent{baseline, y40})
			p := FoldEvents([]FoldEvent{baseline, y40, c20, b30, b30})
			require.JSONEq(t, string(prior.IssueMetadata[child]), string(p.IssueMetadata[child]), "new accepted ABA can equal existing SQL under unrelated later history")
			require.JSONEq(t, want, string(p.IssueMetadata[child]))
			require.Equal(t, []FoldClock{clockOf(b30), clockOf(y40)}, p.ScopedPredicateFacts().IssueMetadata[child], "both surviving writers are required; subject Clock40 alone masks fresh30")
			losing := FoldEvents([]FoldEvent{baseline, b30, y40, c20})
			require.NotContains(t, losing.ScopedPredicateFacts().IssueMetadata[child], clockOf(c20), "new accepted20 overwritten by existing30 is not a surviving effect")
			snapshot := predicateFactEvent(50, "issue.snapshot", `{"uid":"`+child+`","metadata":`+want+`}`)
			final := FoldEvents([]FoldEvent{baseline, c20, b30, y40, snapshot})
			require.Equal(t, p.ScopedPredicateFacts().IssueMetadata[child], final.ScopedPredicateFacts().IssueMetadata[child], "same-value compaction snapshot keeps original winners")
			facts := p.ScopedPredicateFacts()
			// Getter must deep-copy slice storage as well as map headers.
			facts.IssueMetadata[child][0].EventUID = closeProofID(999)
			delete(facts.IssueMetadata, child)
			require.Equal(t, []FoldClock{clockOf(b30), clockOf(y40)}, p.ScopedPredicateFacts().IssueMetadata[child])
		})
	}
}

func TestScopedFoldPredicateFactsReplacementSupersedesAbsentPaths(t *testing.T) {
	child := closeProofID(1)
	baseline := predicateFactEvent(10, "issue.created", `{"uid":"`+child+`","metadata":{"bucket":{"x":"B","y":0}}}`)
	removed := predicateFactEvent(20, "issue.metadata_updated", `{"diff":{"bucket":{"from":{"x":"B","y":0},"to":{"y":0}}}}`)
	for _, tc := range []struct{ name, body, want string }{
		{"whole_value_equal_visible_removes_old_absence", `{"diff":{"bucket":{"from":1,"to":{"y":0}}}}`, `{"bucket":{"y":0}}`},
		{"whole_parent_delete", `{"diff":{"bucket":{"from":{"y":0},"to":null}}}`, `{}`},
		{"whole_value_reintroduces_leaf", `{"diff":{"bucket":{"from":1,"to":{"x":"D","y":0}}}}`, `{"bucket":{"x":"D","y":0}}`},
		{"type_replacement", `{"diff":{"bucket":{"from":{"y":0},"to":0}}}`, `{"bucket":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := predicateFactEvent(40, "issue.metadata_updated", tc.body)
			p := FoldEvents([]FoldEvent{baseline, history, removed})
			require.JSONEq(t, tc.want, string(p.IssueMetadata[child]))
			require.NotContains(t, p.ScopedPredicateFacts().IssueMetadata[child], clockOf(removed), "replacement supersedes descendant deletion witness even when path is already absent")
		})
	}
}
