package db

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"testing"
)

func TestScopedCloseClockProofCodec(t *testing.T) {
	boundary := closeProofClock(10)
	proof := ScopedCloseClockProof{Version: 1, StatusClock: closeProofClock(30), CloseBoundaryClock: &boundary}
	raw, err := EncodeScopedCloseClockProof(proof)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeScopedCloseClockProof(raw)
	if err != nil || !reflect.DeepEqual(got, proof) {
		t.Fatalf("clock proof roundtrip: %#v %v", got, err)
	}
	legacy, err := json.Marshal(closeProofClock(30))
	if err != nil {
		t.Fatal(err)
	}
	old, err := DecodeScopedCloseClockProof(legacy)
	if err != nil || old.StatusClock != proof.StatusClock || old.CloseBoundaryClock != nil {
		t.Fatalf("legacy clock must remain boundary unknown: %#v %v", old, err)
	}
	for _, bad := range [][]byte{nil, []byte(`null`), []byte(`{}`), bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1), bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"future":0`), 1), bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(raw, []byte(`"status_clock":{`), []byte(`"status_clock":null,"ignored":{`), 1)} {
		if _, err := DecodeScopedCloseClockProof(bad); err == nil {
			t.Fatalf("accepted invalid clock wrapper: %s", bad)
		}
	}
	proof.Version = 2
	if _, err := EncodeScopedCloseClockProof(proof); err == nil {
		t.Fatal("encoded unsupported version")
	}
	proof.Version = 1
	future := closeProofClock(40)
	proof.CloseBoundaryClock = &future
	if _, err := EncodeScopedCloseClockProof(proof); err == nil {
		t.Fatal("boundary exceeds latest writer")
	}
}

func TestScopedCanonicalCloseStateProof(t *testing.T) {
	good := closeProofState(10, 30, closeProofValue("duplicate"))
	if err := ValidateScopedCanonicalCloseState(*good, closeProofID(1), "closed"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CanonicalCloseState)
		status string
	}{
		{"wrong subject", func(p *CanonicalCloseState) { p.IssueUID = closeProofID(8) }, "closed"},
		{"unknown boundary", func(p *CanonicalCloseState) { p.CloseBoundaryClock = nil }, "closed"},
		{"wrong boundary intent", func(p *CanonicalCloseState) { v := closeProofClock(20); p.CloseBoundaryClock = &v }, "closed"},
		{"open with value", func(*CanonicalCloseState) {}, "open"},
		{"self peer", func(p *CanonicalCloseState) { p.Value.PeerIssueUID = p.IssueUID }, "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := closeProofState(10, 30, closeProofValue("duplicate"))
			tc.mutate(p)
			if ValidateScopedCanonicalCloseState(*p, closeProofID(1), tc.status) == nil {
				t.Fatal("invalid canonical close proof accepted")
			}
		})
	}
	// Open restatements retain native reopen UID but advance the invalidation
	// boundary; the two event UIDs need not agree while status is open.
	invalidation := closeProofClock(30)
	open := CanonicalCloseState{IssueUID: closeProofID(1), StatusIntentUID: closeProofID(20), StatusClock: closeProofClock(30), CloseBoundaryClock: &invalidation}
	if err := ValidateScopedCanonicalCloseState(open, closeProofID(1), "open"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeClosePeerReference(t *testing.T) {
	value := closeProofValue("duplicate")
	original, err := json.Marshal(struct {
		UID    string            `json:"uid"`
		Reason string            `json:"reason"`
		Value  *ScopedCloseValue `json:"close_value"`
	}{closeProofID(1), "duplicate", value})
	if err != nil {
		t.Fatal(err)
	}
	proof := closeProofState(10, 30, value)
	snapshot, err := json.Marshal(struct {
		UID    string               `json:"uid"`
		Status string               `json:"status"`
		State  *CanonicalCloseState `json:"close_state"`
	}{closeProofID(1), "closed", proof})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind string
		raw  []byte
	}{{"issue.closed", original}, {"issue.snapshot", snapshot}} {
		before := bytes.Clone(tc.raw)
		got, err := NativeClosePeerReference(tc.kind, tc.raw)
		want := &ScopedEndpoint{ProjectUID: value.PeerProjectUID, IssueUID: value.PeerIssueUID}
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("typed peer: got%#v err%v", got, err)
		}
		if !bytes.Equal(before, tc.raw) {
			t.Fatal("accessor mutated retained original")
		}
		got.ProjectUID = closeProofID(99)
		again, err := NativeClosePeerReference(tc.kind, tc.raw)
		if err != nil || !reflect.DeepEqual(want, again) {
			t.Fatalf("returned endpoint aliases retained state: %v", err)
		}
	}
	proof.Value = nil
	currentNull, err := json.Marshal(struct {
		UID    string               `json:"uid"`
		Status string               `json:"status"`
		State  *CanonicalCloseState `json:"close_state"`
	}{closeProofID(1), "closed", proof})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind string
		raw  []byte
	}{{"issue.snapshot", currentNull}, {"issue.labeled", []byte(`{"uid":7,"status":9,"reason":{}}`)}, {"project.metadata_updated", []byte(`[1,2]`)}, {"issue.closed", []byte(`{"reason":"duplicate","evidence":[{"issue_ref":"other#example"}]}`)}} {
		got, err := NativeClosePeerReference(tc.kind, tc.raw)
		if err != nil || got != nil {
			t.Fatalf("absent/current-null carrier has no inferred peer: %#v %v", got, err)
		}
	}
	for _, tc := range []struct {
		kind string
		raw  []byte
	}{{"issue.updated", original}, {"issue.closed", []byte(`{"close_value":null}`)}, {"issue.snapshot", []byte(`{"close_state":{}}`)}, {"issue.closed", bytes.Replace(original, []byte(`"close_value":`), []byte(`"close_value":null,"close_value":`), 1)}} {
		if _, err := NativeClosePeerReference(tc.kind, tc.raw); err == nil {
			t.Fatalf("accepted invalid reserved carrier: %s", tc.raw)
		}
	}
}
