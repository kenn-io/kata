package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func closeProofID(n int) string { return fmt.Sprintf("%026d", n) }
func closeProofClock(n int) FoldClock {
	return FoldClock{HLCPhysicalMS: int64(n), OriginInstanceUID: closeProofID(900), EventUID: closeProofID(n)}
}
func closeProofEvent(n int, kind, status string, value *ScopedCloseValue, proof *CanonicalCloseState) FoldEvent {
	payload := struct {
		UID        string               `json:"uid"`
		Status     string               `json:"status"`
		Reason     string               `json:"reason"`
		CloseValue *ScopedCloseValue    `json:"close_value,omitempty"`
		CloseState *CanonicalCloseState `json:"close_state,omitempty"`
	}{UID: closeProofID(1), Status: status, Reason: "done", CloseValue: value, CloseState: proof}
	if value != nil {
		payload.Reason = value.Kind
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return FoldEvent{UID: closeProofID(n), OriginInstanceUID: closeProofID(900), ProjectUID: closeProofID(2), IssueUID: closeProofID(1), Type: kind, HLCPhysicalMS: int64(n), Payload: raw}
}
func closeProofValue(kind string) *ScopedCloseValue {
	return &ScopedCloseValue{Kind: kind, PeerProjectUID: closeProofID(3), PeerIssueUID: closeProofID(4)}
}
func closeProofState(intent, statusWriter int, value *ScopedCloseValue) *CanonicalCloseState {
	boundary := closeProofClock(intent)
	return &CanonicalCloseState{IssueUID: closeProofID(1), StatusIntentUID: closeProofID(intent), StatusClock: closeProofClock(statusWriter), CloseBoundaryClock: &boundary, Value: value}
}

// Literal outcomes distinguish original intent/invalidation from later envelope
// clocks. The existing native FoldEvents is the only engine exercised.
func TestScopedCloseRegisterLifetime(t *testing.T) {
	c1 := closeProofEvent(10, "issue.closed", "closed", closeProofValue("duplicate"), nil)
	c2 := closeProofEvent(25, "issue.closed", "closed", closeProofValue("superseded"), nil)
	open20 := closeProofEvent(20, "issue.updated", "open", nil, nil)
	reopen20 := closeProofEvent(20, "issue.reopened", "open", nil, nil)
	closed30 := closeProofEvent(30, "issue.snapshot", "closed", nil, nil)
	old40 := closeProofEvent(40, "issue.snapshot", "closed", nil, closeProofState(10, 10, closeProofValue("duplicate")))
	genuine40 := closeProofEvent(40, "issue.snapshot", "closed", nil, closeProofState(25, 30, closeProofValue("superseded")))
	for _, tc := range []struct {
		name                     string
		events                   []FoldEvent
		status                   string
		intent, boundary, latest int
		kind                     string
	}{
		{"same closed restatement", []FoldEvent{c1, closed30}, "closed", 10, 10, 30, "duplicate"},
		{"imported open then ordinary closed", []FoldEvent{c1, open20, closed30}, "closed", 0, 20, 30, ""},
		{"old proof cannot cross empty intent snapshots", []FoldEvent{c1, open20, closed30, old40, closeProofEvent(50, "issue.snapshot", "closed", nil, closeProofState(10, 10, closeProofValue("duplicate")))}, "closed", 0, 20, 50, ""},
		{"genuine C2 before same closed envelope", []FoldEvent{c1, closed30, genuine40}, "closed", 25, 25, 40, "superseded"},
		{"genuine snapshot agrees with full native replay", []FoldEvent{c1, c2, closed30, genuine40}, "closed", 25, 25, 40, "superseded"},
		{"compacted genuine bootstrap", []FoldEvent{genuine40}, "closed", 25, 25, 40, "superseded"},
		{"open restatement advances invalidation", []FoldEvent{c1, reopen20, closeProofEvent(30, "issue.updated", "open", nil, nil), genuine40}, "closed", 0, 30, 40, ""},
		{"reopen clears current value", []FoldEvent{c1, reopen20}, "open", 20, 20, 20, ""},
		{"reclosed replaces proof", []FoldEvent{c1, reopen20, c2}, "closed", 25, 25, 25, "superseded"},
		{"peerless close clears previous value", []FoldEvent{c1, closeProofEvent(50, "issue.closed", "closed", nil, nil)}, "closed", 50, 50, 50, ""},
		{"legacy closed snapshot no intent", []FoldEvent{closed30}, "closed", 0, 1, 30, ""},
		{"content only retains boundary", []FoldEvent{c1, {UID: closeProofID(50), IssueUID: closeProofID(1), Type: "issue.updated", HLCPhysicalMS: 50, Payload: jsontext.Value(`{"title":"Changed"}`)}}, "closed", 10, 10, 10, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := closeProofEvent(1, "issue.created", "open", nil, nil)
			for permutation := 0; permutation < 3; permutation++ {
				events := append([]FoldEvent{base}, tc.events...)
				if permutation == 1 {
					for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
						events[i], events[j] = events[j], events[i]
					}
				}
				if permutation == 2 {
					events = append(events, tc.events[0])
				}
				got := FoldEvents(events).Issues[closeProofID(1)]
				intent := ""
				if tc.intent != 0 {
					intent = closeProofID(tc.intent)
				}
				if got.Status != tc.status || got.StatusIntentUID != intent || got.StatusClock != closeProofClock(tc.latest) {
					t.Fatalf("permutation%d status/intent/latest: %#v", permutation, got)
				}
				if tc.boundary == 0 {
					if got.CloseBoundaryClock != nil {
						t.Fatal("invented close boundary")
					}
				} else if got.CloseBoundaryClock == nil || *got.CloseBoundaryClock != closeProofClock(tc.boundary) {
					t.Fatalf("boundary=%#v want tick%d", got.CloseBoundaryClock, tc.boundary)
				}
				if tc.kind == "" {
					if got.CloseValue != nil {
						t.Fatalf("resurrected value: %#v", got.CloseValue)
					}
				} else if !reflect.DeepEqual(got.CloseValue, closeProofValue(tc.kind)) {
					t.Fatalf("value=%#v want%s", got.CloseValue, tc.kind)
				}
			}
		})
	}
}

func TestScopedCloseMachineValidation(t *testing.T) {
	good := closeProofState(10, 30, closeProofValue("duplicate"))
	encoded, _ := json.Marshal(good)
	cases := []struct{ name, kind, payload string }{
		{"unknown close kind", "issue.closed", `{"reason":"duplicate","close_value":{"kind":"future","peer_project_uid":"` + closeProofID(3) + `","peer_issue_uid":"` + closeProofID(4) + `"}}`},
		{"unknown close field", "issue.closed", `{"reason":"duplicate","close_value":{"kind":"duplicate","peer_project_uid":"` + closeProofID(3) + `","peer_issue_uid":"` + closeProofID(4) + `","extra":1}}`},
		{"reason pairing", "issue.closed", `{"reason":"done","close_value":{"kind":"duplicate","peer_project_uid":"` + closeProofID(3) + `","peer_issue_uid":"` + closeProofID(4) + `"}}`},
		{"self close", "issue.closed", `{"uid":"` + closeProofID(1) + `","reason":"duplicate","close_value":{"kind":"duplicate","peer_project_uid":"` + closeProofID(2) + `","peer_issue_uid":"` + closeProofID(1) + `"}}`},
		{"wrong snapshot subject", "issue.snapshot", `{"uid":"` + closeProofID(8) + `","status":"closed","close_state":` + string(encoded) + `}`},
		{"open snapshot typed value", "issue.snapshot", `{"uid":"` + closeProofID(1) + `","status":"open","close_state":` + string(encoded) + `}`},
		{"null close value field", "issue.closed", `{"reason":"done","close_value":null}`},
		{"null close state field", "issue.snapshot", `{"uid":"` + closeProofID(1) + `","status":"closed","close_state":null}`},
		{"wrong family field", "issue.updated", `{"close_state":` + string(encoded) + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ValidateFederationEntries(tc.kind, closeProofID(50), jsontext.Value(tc.payload)) == nil {
				t.Fatal("malformed native close machine shape accepted")
			}
		})
	}
	valid := closeProofEvent(50, "issue.snapshot", "closed", nil, good)
	if err := ValidateFederationEntries(valid.Type, valid.UID, valid.Payload); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFederationEntries("issue.closed", closeProofID(10), jsontext.Value(`{"reason":"duplicate","evidence":[{"type":"duplicate-of","issue_ref":"legacy-reference"}]}`)); err != nil {
		t.Fatal("legacy absence must remain null proof", err)
	}
}

func TestScopedCloseReferenceMembership(t *testing.T) {
	for _, kind := range []string{"issue.closed", "issue.snapshot"} {
		t.Run(kind, func(t *testing.T) {
			proof := closeProofState(10, 10, closeProofValue("duplicate"))
			event := closeProofEvent(50, kind, "closed", proof.Value, nil)
			if kind == "issue.snapshot" {
				event = closeProofEvent(50, kind, "closed", nil, proof)
			}
			remote := RemoteEvent{EventUID: event.UID, ProjectUID: event.ProjectUID, IssueUID: &event.IssueUID, Type: event.Type, Payload: event.Payload, HLCPhysicalMS: event.HLCPhysicalMS, OriginInstanceUID: event.OriginInstanceUID}
			resolve := func(id string) (ScopedEndpoint, error) {
				if id == closeProofID(1) {
					return ScopedEndpoint{ProjectUID: closeProofID(2), IssueUID: id}, nil
				}
				return ScopedEndpoint{ProjectUID: closeProofID(3), IssueUID: id}, nil
			}
			refs, err := ValidateScopedReferences(remote, resolve, func(string, string) (ScopedEndpoint, error) {
				t.Fatal("must not resolve historical aliases")
				return ScopedEndpoint{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []ScopedEndpoint{{ProjectUID: closeProofID(2), IssueUID: closeProofID(1)}, {ProjectUID: closeProofID(3), IssueUID: closeProofID(4)}}
			if !reflect.DeepEqual(refs, want) {
				t.Fatalf("typed close peer not part of exact native membership: %#v", refs)
			}
			_, err = ValidateScopedReferences(remote, func(id string) (ScopedEndpoint, error) {
				v, _ := resolve(id)
				if id == closeProofID(4) {
					v.ProjectUID = closeProofID(9)
				}
				return v, nil
			}, nil)
			if err == nil {
				t.Fatal("claimed peer project did not match native membership")
			}
		})
	}
}

func TestScopedCurrentCloseAdvancesOnlyNewLocalWriter(t *testing.T) {
	initial := FoldIssue{UID: closeProofID(1), ProjectUID: closeProofID(2), Status: "open"}
	closed, err := AdvanceScopedCanonicalClose(initial, closeProofEvent(10, "issue.closed", "closed", closeProofValue("duplicate"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if initial.Status != "open" || initial.CloseValue != nil {
		t.Fatal("input state mutated")
	}
	if closed.StatusIntentUID != closeProofID(10) || closed.CloseBoundaryClock == nil || closed.CloseBoundaryClock.EventUID != closeProofID(10) || closed.CloseValue == nil {
		t.Fatal("explicit local close provenance lost")
	}
	reopened, err := AdvanceScopedCanonicalClose(closed, closeProofEvent(30, "issue.reopened", "open", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	late, err := AdvanceScopedCanonicalClose(reopened, closeProofEvent(20, "issue.closed", "closed", closeProofValue("duplicate"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened, late) {
		t.Fatal("late local event rewrote current writer")
	}
	same, err := AdvanceScopedCanonicalClose(closed, closeProofEvent(30, "issue.snapshot", "closed", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if same.StatusClock != closeProofClock(30) || same.StatusIntentUID != closed.StatusIntentUID || !sameCloseBoundary(same.CloseBoundaryClock, closed.CloseBoundaryClock) || !sameCloseValue(same.CloseValue, closed.CloseValue) {
		t.Fatal("snapshot confused latest writer and original boundary")
	}
}

func TestScopedCloseSameBoundaryCurrentNullSticky(t *testing.T) {
	original := closeProofEvent(10, "issue.closed", "closed", closeProofValue("duplicate"), nil)
	current := closeProofState(10, 10, nil)
	cleared := closeProofEvent(20, "issue.snapshot", "closed", nil, current)
	stale := closeProofEvent(30, "issue.snapshot", "closed", nil, closeProofState(10, 10, closeProofValue("duplicate")))
	projection := FoldEvents([]FoldEvent{original, cleared, stale})
	issue := projection.Issues[original.IssueUID]
	require.Nil(t, issue.CloseValue, "same-intent current null invalidates C1 and later same-boundary stale snapshots cannot revive it")
	require.Equal(t, original.UID, issue.StatusIntentUID)
	require.Equal(t, clockOf(original), *issue.CloseBoundaryClock)
	next := closeProofEvent(40, "issue.closed", "closed", closeProofValue("superseded"), nil)
	projection = FoldEvents([]FoldEvent{original, cleared, stale, next})
	require.Equal(t, next.UID, projection.Issues[original.IssueUID].StatusIntentUID)
	require.Equal(t, closeProofValue("superseded"), projection.Issues[original.IssueUID].CloseValue)
}
