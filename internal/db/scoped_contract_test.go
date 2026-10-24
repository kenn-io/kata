package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/shortid"
	"strings"
	"testing"
	"time"
)

const scopedUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const scopedPeerUID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

// These contracts prevent an unclassified producer or ambiguous reference from
// entering a view. They do not authorize a source operation.
func TestScopedContractInventoryComplete(t *testing.T) {
	for _, event := range federationEventTypes {
		if class, ok := ScopedEventClass(event.name); !ok || class == "" {
			t.Errorf("unclassified event %s", event.name)
		}
	}
	for _, kind := range []string{"project.created", "project.renamed", "project.merged", "project.author_rewritten", "project.federation_enabled", "project.alias_removed", "recurrence.created", "recurrence.updated", "recurrence.deleted", "recurrence.materialized", "recurrence.materialization_skipped", "close.throttled", "claim.acquired", "claim.renewed", "claim.released", "claim.expired", "claim.force_released", "claim.violated", "issue.scheduled_reached", "author.rewritten"} {
		if _, ok := ScopedEventClass(kind); !ok {
			t.Errorf("unclassified local event %s", kind)
		}
	}
	if _, ok := ScopedEventClass("future.structured"); ok {
		t.Fatal("unknown structured family accepted")
	}
	if SourceCommandClass("futureCommand").Disposition != "" {
		t.Fatal("unknown command accepted")
	}
	seen := map[string]bool{}
	for _, group := range scopedCommandGroups {
		for _, id := range group.operations {
			if seen[id] {
				t.Fatalf("duplicate command classification %s", id)
			}
			seen[id] = true
		}
	}
	seen = map[string]bool{}
	for _, kinds := range scopedEventGroups {
		for _, kind := range kinds {
			if seen[kind] {
				t.Fatalf("duplicate structured classification %s", kind)
			}
			seen[kind] = true
		}
	}
	for _, tc := range []struct{ id, disposition string }{{"mergeProject", "root_only"}, {"purgeProject", "root_only"}, {"importIssues", "source"}, {"notifyIssue", "source"}} {
		got := SourceCommandClass(tc.id)
		if got.Disposition != tc.disposition {
			t.Fatalf("%s: %+v", tc.id, got)
		}
	}
	// Scheduling observations are private local audit. The ordinary notification
	// metadata they may cause remains current project content.
	for _, tc := range []struct{ kind, class string }{
		{kind: "issue.scheduled_reached", class: "history"},
		{kind: "issue.metadata_updated", class: "current"},
		{kind: "recurrence.materialization_skipped", class: "root_only"},
	} {
		class, ok := ScopedEventClass(tc.kind)
		if !ok || class != tc.class {
			t.Errorf("scoped %s = %q (known=%v), want %q", tc.kind, class, ok, tc.class)
		}
	}
	for _, pair := range [][2]string{{"createIssue", "source"}, {"claimIssue", "control"}, {"getIssueLeaseStatus", "control"}, {"pollEvents", "history"}, {"moveIssue", "root_only"}, {"createRecurrence", "root_only"}, {"forceReleaseIssueLease", "root_only"}} {
		if got := SourceCommandClass(pair[0]); got.Disposition != pair[1] {
			t.Errorf("%s=%+v want %s", pair[0], got, pair[1])
		}
	}
}

func TestScopedReferenceNullableUnion(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		subject := ScopedEndpoint{ProjectUID: scopedUID, IssueUID: scopedUID}
		r := ScopedReference{Slot: "close", Kind: "close", Subject: subject, Revision: 1}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		r.Peer = &subject
		if r.Validate() == nil {
			t.Fatal("close union accepted duplicate peer fields")
		}
		r.Peer = nil
		r.Present = true
		if r.Validate() == nil {
			t.Fatal("present close accepted null value")
		}
		r.Close = &ScopedCloseValue{Kind: "duplicate", PeerProjectUID: scopedUID, PeerIssueUID: scopedPeerUID}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		r.Present = false
		if r.Validate() == nil {
			t.Fatal("null close retained a peer")
		}
	})
	t.Run("definition", func(t *testing.T) {
		r := ScopedReference{Slot: "trigger", Kind: "definition", Subject: ScopedEndpoint{ProjectUID: scopedUID}, SubjectEntityUID: scopedPeerUID, Revision: 1}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		r.Subject.IssueUID = scopedUID
		if r.Validate() == nil {
			t.Fatal("definition subject invented issue membership")
		}
		r.Subject.IssueUID = ""
		r.Slot, r.Present, r.Relation = "action", true, "workflow"
		r.Peer, r.PeerEntityUID = &ScopedEndpoint{ProjectUID: scopedUID}, "01ARZ3NDEKTSV4RRFFQ69G5FAX"
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		r.Peer.IssueUID = scopedUID
		if r.Validate() == nil {
			t.Fatal("workflow peer invented issue membership")
		}
		r.Peer.IssueUID = ""
		r.Relation = "issue"
		r.PeerEntityUID = ""
		if r.Validate() == nil {
			t.Fatal("issue peer missing required issue membership")
		}
	})
	t.Run("typed_entities", func(t *testing.T) {
		r := ScopedReference{Slot: "reply", Kind: "reply", Subject: ScopedEndpoint{ProjectUID: scopedUID, IssueUID: scopedUID}, SubjectEntityUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", Peer: &ScopedEndpoint{ProjectUID: scopedUID, IssueUID: scopedPeerUID}, PeerEntityUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Present: true, Revision: 1}
		for _, tc := range []struct {
			kind  string
			valid bool
		}{{"reply", true}, {"confirm", true}, {"refute", true}, {"supersede", true}, {"", false}, {"future-kind", false}} {
			r.Relation = tc.kind
			err := r.Validate()
			if (err == nil) != tc.valid {
				t.Errorf("reply relation %q: %v", tc.kind, err)
			}
		}
		r.Kind = "link"
		r.Relation = "blocks"
		r.Slot = "blocks"
		r.SubjectEntityUID = ""
		r.PeerEntityUID = ""
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		r.SubjectEntityUID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
		if r.Validate() == nil {
			t.Error("link retained a subject entity UID")
		}
		r.SubjectEntityUID = ""
		r.PeerEntityUID = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
		if r.Validate() == nil {
			t.Error("link retained a peer entity UID")
		}
	})
}

func TestScopedContractEffectUnion(t *testing.T) {
	e := ScopedEffect{Collection: "issues", Key: scopedUID, Action: "replace", Revision: 1, Issue: &ScopedIssueState{ProjectUID: scopedUID, UID: scopedUID, Title: "Work", Status: "open"}}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Project = &ScopedProjectState{}
	if e.Validate() == nil {
		t.Fatal("ambiguous replacement accepted")
	}
	e.Project = nil
	e.Action = "remove"
	if e.Validate() == nil {
		t.Fatal("removal disclosed body")
	}
	e.Issue = nil
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestScopedContractSignatureDomains(t *testing.T) {
	domains := []string{"kata.scoped.view.v1", "kata.scoped.entity.v1", "kata.scoped.reference.v1", "kata.scoped.provenance.v1", "kata.scoped.batch.v1", "kata.scoped.checkpoint.v1", "kata.scoped.membership.v1", "kata.scoped.result.v1", "kata.scoped.coverage.v1", "kata.scoped.cutover.v1"}
	seen := map[string]bool{}
	for _, domain := range domains {
		got, err := ScopedSigningBytes(domain, []byte(`{"z":1,"a":2}`))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"domain":"` + domain + `","record":{"a":2,"z":1}}`
		if string(got) != want {
			t.Fatalf("%s: %s", domain, got)
		}
		if seen[string(got)] {
			t.Fatal("domain collision")
		}
		seen[string(got)] = true
	}
	for _, domain := range []string{"kata.root-attribution.v1", "unknown:", "unknown:unknown", "unknown:kata.scoped.entity.v1\x00"} {
		if _, err := ScopedSigningBytes(domain, []byte(`{}`)); err == nil {
			t.Fatalf("foreign domain accepted: %q", domain)
		}
	}
	for _, bad := range []string{`{"a":1,"a":2}`, `{"x":1} trailing`} {
		if _, err := ScopedSigningBytes(domains[0], []byte(bad)); err == nil {
			t.Fatalf("invalid signed JSON accepted: %s", bad)
		}
	}
}

func TestScopedContractStrictDecode(t *testing.T) {
	for _, raw := range []string{`{"token":"opaque","extra":1}`, `{"token":"opaque","token":"other"}`} {
		var out MutationExpectation
		if DecodeScopedRecord([]byte(raw), &out) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var out MutationExpectation
	if err := DecodeScopedRecord([]byte(`{"token":"opaque"}`), &out); err != nil {
		t.Fatal(err)
	}
	if err := DecodeScopedRecord([]byte(`{}`), &out); err == nil {
		t.Error("independent record inherited the previous opaque token")
	}
}

func TestScopedContractNumericGuardBeforeSource(t *testing.T) {
	for _, token := range []string{"", "rev-1", "rev-0001", "rev-9223372036854775807"} {
		if (MutationExpectation{Token: token}).Validate() == nil {
			t.Fatalf("accepted canonical guard %q", token)
		}
	}
	if err := (MutationExpectation{Token: "opaque-token"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestScopedReferenceAliasesAndPrimary(t *testing.T) {
	resolve := func(s string) (ScopedEndpoint, error) { return ScopedEndpoint{ProjectUID: scopedUID, IssueUID: s}, nil }
	byShort := func(_, s string) (ScopedEndpoint, error) { return resolve(s) }
	subject := scopedUID
	event := RemoteEvent{Type: "issue.linked", ProjectUID: scopedUID, IssueUID: &subject, Payload: []byte(`{"from_uid":"` + scopedUID + `","to_uid":"` + scopedPeerUID + `","type":"blocks"}`)}
	refs, err := ValidateScopedReferences(event, resolve, byShort)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
	event.Payload = []byte(strings.ReplaceAll(string(event.Payload), `"to_uid":`, `"to_issue_uid":"`+scopedUID+`","to_uid":`))
	if _, err := ValidateScopedReferences(event, resolve, byShort); err == nil {
		t.Fatal("conflicting aliases accepted")
	}
	event.Payload = []byte(`{"from_uid":"` + scopedPeerUID + `","to_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAX","type":"blocks"}`)
	if _, err := ValidateScopedReferences(event, resolve, byShort); err == nil {
		t.Fatal("spoofed primary accepted")
	}
	for _, tc := range []struct{ name, from, to string }{
		{"alternate_from", "from_issue_uid", "to_uid"},
		{"alternate_to", "from_uid", "to_issue_uid"},
		{"alternate_both", "from_issue_uid", "to_issue_uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event.Payload = []byte(fmt.Sprintf(`{"%s":%q,"%s":%q,"type":"blocks"}`, tc.from, scopedUID, tc.to, scopedPeerUID))
			if _, err := ValidateScopedReferences(event, resolve, byShort); err == nil {
				t.Fatal("accepted endpoint fields rejected by native ingestion")
			}
		})
	}
	for _, tc := range []struct {
		name, asserted string
		wantError      bool
	}{
		{"matching_issue_uid", `"` + scopedUID + `"`, false},
		{"wrong_issue_uid", `"01ARZ3NDEKTSV4RRFFQ69G5FAX"`, true},
		{"empty_issue_uid", `""`, true},
		{"malformed_issue_uid", `42`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event.Payload = []byte(fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"blocks","issue_uid":%s}`, scopedUID, scopedPeerUID, tc.asserted))
			refs, err := ValidateScopedReferences(event, resolve, byShort)
			if (err != nil) != tc.wantError || !tc.wantError && len(refs) != 2 {
				t.Fatalf("payload issue subject: refs=%v err=%v", refs, err)
			}
		})
	}
}

func TestScopedContractPreparationBoundary(t *testing.T) {
	for _, tc := range []struct{ op, subject, predicate string }{{"patchProjectMetadata", "project", "metadata"}, {"patchIssueMetadata", "issue", "metadata"}, {"closeIssue", "issue", "close"}, {"editIssue", "issue", "parent"}, {"replaceCronJob", "job", "definition"}, {"replaceCronWorkflow", "workflow", "definition"}} {
		p := ScopedMutationPreparation{Kind: tc.op, Subject: ScopedMutationSubject{Kind: tc.subject, ProjectUID: scopedUID}, Predicate: tc.predicate, Peers: []ScopedEndpoint{}}
		if tc.subject == "issue" {
			p.Subject.IssueUID = scopedPeerUID
		}
		if tc.subject == "job" || tc.subject == "workflow" {
			p.Subject.DefinitionUID = scopedPeerUID
		}
		if tc.predicate == "metadata" {
			p.MetadataGuard = &ScopedMetadataPredicate{Key: "work.attention", IfAbsent: true}
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var out ScopedMutationPreparation
		if err := DecodeScopedRecord(raw, &out); err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		if out.Kind != p.Kind || out.Subject != p.Subject || out.Predicate != p.Predicate {
			t.Fatal("subject changed")
		}
		if tc.predicate == "metadata" {
			for _, guard := range []struct {
				name, key, value string
				absent, valid    bool
			}{
				{"absent", "work.attention", "", true, true},
				{"value", "work.attention", `"ready"`, false, true},
				{"blank_key", " \t ", "", true, false},
				{"null_value", "work.attention", "null", false, false},
				{"padded_null", "work.attention", " \nnull\t ", false, false},
			} {
				t.Run(tc.op+"/"+guard.name, func(t *testing.T) {
					native := &MetadataPatchGuard{Key: guard.key, IfValue: jsontext.Value(guard.value), IfAbsent: guard.absent}
					current := jsontext.Value(`{}`)
					if guard.value != "" {
						current, err = json.Marshal(map[string]jsontext.Value{guard.key: jsontext.Value(guard.value)})
						if err != nil {
							t.Fatal(err)
						}
					}
					nativeErr := CheckMetadataPatchGuard(current, map[string]jsontext.Value{guard.key: jsontext.Value(`"changed"`)}, native)
					if (nativeErr == nil) != guard.valid {
						t.Fatalf("native guard baseline: %v", nativeErr)
					}
					input := p
					input.MetadataGuard = &ScopedMetadataPredicate{Key: guard.key, IfValue: JSONBlob(guard.value), IfAbsent: guard.absent}
					if err := input.Validate(); (err == nil) != guard.valid {
						t.Errorf("direct scoped guard differs from native validity %t: %v", guard.valid, err)
					}
					wire, err := json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
					var decoded ScopedMutationPreparation
					if err := DecodeScopedRecord(wire, &decoded); (err == nil) != guard.valid {
						t.Errorf("scoped guard differs from native validity %t: %v", guard.valid, err)
					}
				})
			}
		}
		p.Subject.Kind = "project"
		if tc.subject != "project" && p.Validate() == nil {
			t.Fatalf("%s allowed wrong subject", tc.op)
		}
	}
	var out ScopedMutationPreparation
	for _, raw := range []string{`{"kind":"closeIssue","subject":{"kind":"issue","project_uid":"` + scopedUID + `","issue_uid":"` + scopedPeerUID + `"},"peers":[],"predicate":"close","issue_id":42}`, `{"kind":"closeIssue","subject":{"kind":"issue","project_uid":"invalid","issue_uid":"` + scopedPeerUID + `"},"peers":[],"predicate":"close"}`} {
		if DecodeScopedRecord([]byte(raw), &out) == nil {
			t.Fatalf("invalid wire preparation accepted %s", raw)
		}
	}
	if _, err := json.Marshal(ScopedCommand{Kind: "closeIssue", Close: &CloseIssueParams{IssueID: 42}}); err == nil {
		t.Fatal("native row IDs marshaled")
	}
}

func scopedContractView() ScopedViewDescriptor {
	public := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	return ScopedViewDescriptor{Version: 2, Schema: 1, Key: ScopedViewKey{ViewUID: scopedUID, Generation: 1}, AuthorityUID: scopedUID, DestinationUID: scopedPeerUID, RouteDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), Projects: []ScopedProjectPin{{ProjectUID: scopedUID, Pin: RootKeyPin{ProjectUID: scopedUID, AuthorityUID: scopedUID, KeyID: RootPublicKeyID(public), PublicKey: public}}}, Route: []ScopedHopBinding{{BindingUID: scopedUID, InstanceUID: scopedPeerUID, Epoch: 1}}}
}

func scopedContractCollections() ScopedCollections {
	return ScopedCollections{
		Projects: []ScopedProjectState{}, Issues: []ScopedIssueState{},
		Comments: []ScopedCommentState{}, Labels: []ScopedLabelState{},
		Definitions: []ScopedDefinitionState{}, References: []ScopedReference{},
		Provenance: []ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{},
	}
}

func TestScopedContractDescriptorAndLimits(t *testing.T) {
	t.Run("pin_identity", func(t *testing.T) {
		v := scopedContractView()
		v.Projects[0].Pin.KeyID = strings.Repeat("c", 64)
		if err := v.Validate(); err == nil {
			t.Error("descriptor accepted a key ID that does not commit its public key")
		}
	})
	v := scopedContractView()
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	v.Projects = append(v.Projects, v.Projects[0])
	if v.Validate() == nil {
		t.Fatal("duplicate project pin accepted")
	}
	v.Projects = v.Projects[:1]
	v.Version = 1
	if v.Validate() == nil {
		t.Fatal("v1 accepted as v2")
	}
	for _, n := range []int64{-1, (64 << 20) + 1} {
		if ValidateScopedCheckpointSize(n) == nil {
			t.Fatalf("accepted size %d", n)
		}
	}
	if err := ValidateScopedCheckpointSize(64 << 20); err != nil {
		t.Fatal(err)
	}
	v.Version = 2
	cut := ScopedCut{Sequence: 1, Digest: strings.Repeat("a", 64), CaptureUID: scopedUID}
	batch := ScopedBatch{View: v, Cut: cut, PreviousDigest: strings.Repeat("b", 64), Effects: []ScopedEffect{{Collection: "issues", Key: scopedUID, Action: "remove", Revision: 1}}}
	if err := batch.Validate(); err != nil {
		t.Fatal(err)
	}
	checkpoint := ScopedCheckpoint{View: v, Cut: cut, Collections: scopedContractCollections()}
	if err := checkpoint.Validate(); err != nil {
		t.Fatal(err)
	}
	batch.View.Version, checkpoint.View.Version = 99, 99
	if batch.Validate() == nil || checkpoint.Validate() == nil {
		t.Fatal("unsupported view version accepted in otherwise valid record")
	}
	batch.View.Version = 2
	batch.Effects[0].Revision = 2
	if batch.Validate() == nil {
		t.Fatal("effect position differs from its atomic batch cut")
	}
}

func scopedContractSubmission(t *testing.T, kind string, events ...RemoteEvent) SourceSubmission {
	t.Helper()
	s := SourceSubmission{Operation: SourceOperation{Version: 1, UID: scopedUID, SourceInstanceUID: scopedUID, CommandKind: kind, Principal: CustodyPrincipal{BindingUID: scopedUID, AuthorityUID: scopedUID, AccountableActor: "account-a", Epoch: 1, RouteDigest: strings.Repeat("a", 64)}}}
	for i, event := range events {
		if event.ContentHash == "" {
			var err error
			event.ContentHash, err = RemoteEventContentHash(event)
			if err != nil {
				t.Fatal(err)
			}
		}
		s.Originals = append(s.Originals, event)
		s.Operation.Members = append(s.Operation.Members, SourceMember{ProjectUID: event.ProjectUID, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: i})
	}
	var err error
	s.Operation.Digest, err = SourceOperationDigest(s.Operation)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScopedContractPrincipalBoundary(t *testing.T) {
	p := CustodyPrincipal{BindingUID: scopedUID, Epoch: 1, AuthorityUID: scopedUID, AccountableActor: "account-a", RouteDigest: strings.Repeat("a", 64)}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.AccountableActor = "bootstrap"
	if p.Validate() == nil {
		t.Fatal("invalid native actor accepted")
	}
	raw := []byte(`{"request_uid":"` + scopedUID + `","digest":"` + strings.Repeat("a", 64) + `","kind":"acquire","subject":{"project_uid":"` + scopedUID + `","issue_uid":"` + scopedPeerUID + `"},"principal":{"holder_instance_uid":"` + scopedUID + `","holder":"account-a","client_kind":"agent","authenticated_host":true},"action":{"ttl_seconds":60}}`)
	var request ScopedControlRequest
	valid := []byte(strings.Replace(string(raw), `,"authenticated_host":true`, "", 1))
	if err := DecodeScopedRecord(valid, &request); err != nil {
		t.Fatalf("valid control request rejected: %v", err)
	}
	if err := DecodeScopedRecord(raw, &request); err == nil {
		t.Fatal("forged authenticated_host admitted")
	}
}

func TestScopedReferenceLinksChangedSubject(t *testing.T) {
	subject := scopedUID
	peerAlias, err := shortid.Derive(scopedPeerUID, shortid.MinLength)
	if err != nil {
		t.Fatal(err)
	}
	peer := PeerIdentity{ShortID: peerAlias, UID: scopedPeerUID, Project: "alpha-project"}
	native, err := LinksChangedPayload(AtomicEditChanges{ParentSet: &peer}, "2026-10-24T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(value string) (ScopedEndpoint, error) {
		return ScopedEndpoint{ProjectUID: scopedUID, IssueUID: value}, nil
	}
	short := func(_, _ string) (ScopedEndpoint, error) { return resolve(scopedPeerUID) }
	for _, tc := range []struct {
		name, payload string
		envelope      *string
		wantError     bool
	}{
		{"matching", `{"issue_uid":"` + scopedUID + `","parent_set_uid":"` + scopedPeerUID + `"}`, &subject, false},
		{"different_subject", `{"issue_uid":"` + scopedPeerUID + `","parent_set_uid":"` + scopedPeerUID + `"}`, &subject, true},
		{"no_envelope_subject", `{"issue_uid":"` + scopedUID + `","parent_set_uid":"` + scopedPeerUID + `"}`, nil, true},
		{"native_no_envelope_subject", string(native), nil, true},
		{"malformed_subject", `{"issue_uid":42,"parent_set_uid":"` + scopedPeerUID + `"}`, &subject, true},
		{"both_aliases_match", `{"uid":"` + scopedUID + `","issue_uid":"` + scopedUID + `","parent_set_uid":"` + scopedPeerUID + `"}`, &subject, false},
		{"subject_aliases_disagree", `{"uid":"` + scopedUID + `","issue_uid":"` + scopedPeerUID + `","parent_set_uid":"` + scopedPeerUID + `"}`, &subject, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := RemoteEvent{Type: "issue.links_changed", ProjectUID: scopedUID, IssueUID: tc.envelope, Payload: []byte(tc.payload)}
			refs, err := ValidateScopedReferences(event, resolve, short)
			if tc.wantError {
				if err == nil {
					t.Fatal("accepted a payload subject without its matching event subject")
				}
			} else if err != nil || len(refs) != 2 {
				t.Fatalf("matching native subject rejected: %v %v", refs, err)
			}
		})
	}
}

func TestScopedReferenceCompositeReply(t *testing.T) {
	subject := scopedUID
	resolve := func(s string) (ScopedEndpoint, error) {
		if s == "01ARZ3NDEKTSV4RRFFQ69G5FAX" {
			s = scopedPeerUID
		}
		return ScopedEndpoint{ProjectUID: scopedUID, IssueUID: s}, nil
	}
	short := func(_, s string) (ScopedEndpoint, error) { return resolve(s) }
	shortID, err := shortid.Derive(subject, shortid.MinLength)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"uid":"` + subject + `","short_id":"` + shortID + `","title":"Thread","body":"","author":"account-a","status":"open","metadata":{},"created_at":"2026-10-24T00:00:00.000Z","comments":[{"comment_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAY","author":"account-a","body":"Reply","created_at":"2026-10-24T00:00:00.000Z","reply_to_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAX","reply_kind":"reply"}]}`
	created := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	event := RemoteEvent{EventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", OriginInstanceUID: scopedUID, Type: "issue.snapshot", ProjectUID: scopedUID, IssueUID: &subject, Actor: "account-a", HLCPhysicalMS: created.UnixMilli(), CreatedAt: created, Payload: []byte(payload)}
	event.ContentHash, err = RemoteEventContentHash(event)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := ValidateScopedReferences(event, resolve, short)
	if err != nil || len(refs) != 2 || refs[0] != (ScopedEndpoint{ProjectUID: scopedUID, IssueUID: subject}) || refs[1] != (ScopedEndpoint{ProjectUID: scopedUID, IssueUID: scopedPeerUID}) {
		t.Fatalf("snapshot missing resolved reply owner: %v %v", refs, err)
	}
}

func TestScopedContractCommandUnion(t *testing.T) {
	c := ScopedCommand{Kind: "closeIssue", Close: &CloseIssueParams{}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Edit = &EditIssueAtomicParams{}
	if c.Validate() == nil {
		t.Fatal("two native bodies accepted")
	}
	c.Edit = nil
	c.Kind = "createIssue"
	if c.Validate() == nil {
		t.Fatal("wrong native body accepted")
	}
	c.Kind = "closeIssue"
	rev := int64(1)
	c.Close.IfMatchRev = &rev
	if c.Validate() == nil {
		t.Fatal("numeric guard accepted before source planning")
	}
	c = ScopedCommand{Kind: "importIssues", Import: &ImportBatchParams{}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Create = &CreateIssueParams{}
	if c.Validate() == nil {
		t.Fatal("import command combined with a second native body")
	}
	for _, tc := range []struct {
		name    string
		command ScopedCommand
	}{
		{"job", ScopedCommand{Kind: "replaceCronJob", Job: &PutCronJob{ProjectID: 1, UID: scopedUID, Name: "Review", Actor: "account-a", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}}}},
		{"workflow", ScopedCommand{Kind: "replaceCronWorkflow", Workflow: &PutCronWorkflow{ProjectID: 1, UID: scopedUID, Name: "Review", Actor: "account-a", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}}}},
	} {
		if err := tc.command.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, opaque := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_expected_event_opaque_%v", tc.name, opaque), func(t *testing.T) {
				candidate := tc.command
				if candidate.Job != nil {
					candidate.Job.ExpectedEventUID = scopedPeerUID
				} else {
					candidate.Workflow.ExpectedEventUID = scopedPeerUID
				}
				if opaque {
					candidate.Expectations = []MutationExpectation{{Token: "opaque-token"}}
				}
				if candidate.Validate() == nil {
					t.Fatal("canonical cron event guard admitted to scoped planning")
				}
			})
		}
	}
}

func scopedContractDelivery(t *testing.T, sequence int64) ScopedDeliveryRecord {
	t.Helper()
	header := ScopedDeliveryHeader{Version: 2, Domain: "kata.scoped.delivery.v1", BindingUID: scopedUID, Epoch: 1, Channel: ScopedDeliverySource, Sequence: sequence, AuthorityUID: scopedUID, SenderInstanceUID: scopedUID, ReceiverInstanceUID: scopedPeerUID, Path: []string{scopedUID}, ObjectUID: fmt.Sprintf("%026d", sequence), ObjectDigest: strings.Repeat("a", 64)}
	raw, err := EncodeScopedDelivery(header, []byte("exact original"))
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := ScopedDeliveryHeaderBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	ref := ScopedDeliveryRef{ScopedDeliveryStream: ScopedDeliveryStream{BindingUID: scopedUID, Epoch: 1, Direction: ScopedDeliveryInbound, Channel: ScopedDeliverySource}, Sequence: sequence}
	return ScopedDeliveryRecord{ScopedDeliveryRef: ref, ObjectUID: header.ObjectUID, ObjectDigest: header.ObjectDigest, OfferedDigest: ScopedOfferedDigest(raw), HeaderBytes: transcript, EnvelopeBytes: raw}
}

func TestScopedContractDeliveryExactIdentity(t *testing.T) {
	row := scopedContractDelivery(t, 1)
	raw := row.EnvelopeBytes
	got, body, err := DecodeScopedDelivery(raw)
	if err != nil || !bytes.Equal(body, []byte("exact original")) || got.Sequence != 1 {
		t.Fatalf("decode %v %s %v", got, body, err)
	}
	if err := row.Validate(); err != nil {
		t.Fatal(err)
	}
	row.EnvelopeBytes = nil
	if row.Validate() == nil {
		t.Fatal("unaccepted ingress compacted")
	}
	row.Accepted = true
	if err := row.Validate(); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(raw, []byte(`"header_digest":"`), []byte(`"header_digest":"0`), 1)
	if _, _, err := DecodeScopedDelivery(changed); err == nil {
		t.Fatal("changed transcript accepted")
	}
	retry := row
	retry.OfferedDigest = strings.Repeat("b", 64)
	if SameScopedDeliveryIdentity(row, retry) {
		t.Fatal("changed old retry accepted")
	}
	retry = row
	retry.Direction = ScopedDeliveryOutbound
	if SameScopedDeliveryIdentity(row, retry) {
		t.Fatal("opposite direction conflated")
	}
}

func TestScopedContractDeliveryContiguousPrefixes(t *testing.T) {
	stream := ScopedDeliveryStream{BindingUID: scopedUID, Epoch: 1, Direction: ScopedDeliveryInbound, Channel: ScopedDeliverySource}
	first, second := scopedContractDelivery(t, 1), scopedContractDelivery(t, 2)
	cursor := ScopedDeliveryCursor{ScopedDeliveryStream: stream, ReceivedSequence: 2, ReceivedDigest: second.OfferedDigest}
	if err := cursor.Validate(); err != nil {
		t.Fatal(err)
	}
	first.Accepted, second.Accepted = true, true
	for _, row := range []ScopedDeliveryRecord{first, second} {
		if err := row.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	// An authenticated offer for sequence 2 does not prove sequence 1 absent.
	rows := []ScopedDeliveryRecord{second}
	if err := ValidateScopedDeliveryPrefix(cursor, 2, "accepted", rows); err == nil {
		t.Fatal("prefix skipped missing first sequence")
	}
	rows = append([]ScopedDeliveryRecord{first}, rows...)
	if err := ValidateScopedDeliveryPrefix(cursor, 2, "accepted", rows); err != nil {
		t.Fatal(err)
	}
	rows[0].Accepted = false
	if err := ValidateScopedDeliveryPrefix(cursor, 2, "accepted", rows); err == nil {
		t.Fatal("prefix skipped unresolved earlier offer")
	}
}

func TestScopedContractClosedTerminalUnions(t *testing.T) {
	t.Run("coverage_outcomes", func(t *testing.T) {
		coverage := SourceCoverage{OperationUID: scopedUID, OperationDigest: strings.Repeat("a", 64), View: scopedContractView().Key, Cut: ScopedCut{Sequence: 1, Digest: strings.Repeat("b", 64), CaptureUID: scopedPeerUID}}
		for _, outcome := range []string{"represented", "unchanged", "outside_scope", "future"} {
			t.Run(outcome, func(t *testing.T) {
				coverage.Outcome = outcome
				raw, err := json.Marshal(coverage)
				if err != nil {
					t.Fatal(err)
				}
				var decoded SourceCoverage
				err = DecodeScopedRecord(raw, &decoded)
				if (err == nil) != (outcome != "future") {
					t.Fatalf("coverage outcome %q decode: %v", outcome, err)
				}
			})
		}
	})
	r := SourceResult{OperationUID: scopedUID, OperationDigest: strings.Repeat("a", 64), State: "staged"}
	if r.Validate() == nil {
		t.Fatal("unsigned custody status accepted as terminal result")
	}
	r.State = "canceled"
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	c := ScopedControlResult{RequestUID: scopedUID, Digest: strings.Repeat("a", 64), State: "accepted", Lease: &ScopedLeaseState{}, Assignment: &ScopedAssignmentState{}}
	if c.Validate() == nil {
		t.Fatal("ambiguous control result accepted")
	}
	conversion := ScopedConversionState{Phase: "prepare", Request: ScopedConversionRequest{UID: scopedUID, CatalogEntryUID: scopedPeerUID, Digest: strings.Repeat("a", 64), RouteDigest: strings.Repeat("b", 64), ProjectUIDs: []string{scopedUID}, Target: scopedContractView()}}
	if err := conversion.Validate(); err != nil {
		t.Fatal(err)
	}
	conversion.Phase = "unknown"
	if conversion.Validate() == nil {
		t.Fatal("unknown conversion phase accepted")
	}
}

func TestScopedContractSignedBytesCanonical(t *testing.T) {
	for _, raw := range []string{`{"z":1,"a":2}`, `{ "a":1}`, `{"a":1,"a":1}`} {
		if ValidateScopedSignedBytes([]byte(raw)) == nil {
			t.Fatalf("noncanonical signed bytes accepted: %s", raw)
		}
	}
	if err := ValidateScopedSignedBytes([]byte(`{"a":9223372036854775807}`)); err != nil {
		t.Fatal(err)
	}
}

func TestScopedContractPrincipalKeyBinding(t *testing.T) {
	p := CustodyPrincipal{AuthorityUID: scopedUID, BindingUID: scopedPeerUID, Epoch: 1, AccountableActor: "account-a", RouteDigest: strings.Repeat("a", 64)}
	first, err := ScopedPrincipalKey(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Epoch = 2
	second, err := ScopedPrincipalKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("principal key crossed enrollment epoch")
	}
	p.Epoch = 1
	p.RouteDigest = strings.Repeat("b", 64)
	third, err := ScopedPrincipalKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("equal actor strings unioned across routes")
	}
}

func TestScopedContractSourceWireCodecPreservesWholeBytes(t *testing.T) {
	created := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	t.Run("exact_whole_bytes", func(t *testing.T) {
		event := RemoteEvent{EventUID: scopedPeerUID, OriginInstanceUID: scopedUID, ProjectUID: scopedUID, HLCPhysicalMS: created.UnixMilli(), CreatedAt: created, Type: "project.metadata_updated", Actor: "account-a", Payload: []byte("{ \"revision_new\" : 1, \"diff\" : { \"example.z\" : { \"from\" : null, \"to\" : 1 }, \"example.a\" : { \"from\" : null, \"to\" : 2 } } }")}
		hash, err := RemoteEventContentHash(event)
		if err != nil {
			t.Fatal(err)
		}
		event.ContentHash = hash
		native, err := EncodeRelaySourceEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		s := scopedContractSubmission(t, "patchProjectMetadata", event)
		encoded, err := EncodeScopedSourceSubmission(s)
		if err != nil {
			t.Fatal(err)
		}
		// A valid authenticated body may have outer whitespace too. Forward it via
		// the explicit byte codec; a generic JSON encoder may compact its output.
		retained := append([]byte("\n "), encoded...)
		retained = append(retained, '\n')
		decoded, err := DecodeScopedSourceSubmission(retained)
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded.Originals) != 1 || !bytes.Equal(decoded.Originals[0].Payload, event.Payload) || !bytes.Equal(decoded.OriginalBytes[0], native) {
			t.Fatal("native source payload or wrapper normalized")
		}
		again, err := EncodeScopedSourceSubmission(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, retained) {
			t.Fatal("whole authenticated submission normalized")
		}
		decoded.Operation.IdempotencyKey = "changed"
		decoded.Operation.Digest, err = SourceOperationDigest(decoded.Operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := EncodeScopedSourceSubmission(decoded); err == nil {
			t.Fatal("typed mutation ignored retained authenticated bytes")
		}
	})
	t.Run("manifest_identity", func(t *testing.T) {
		e := RemoteEvent{EventUID: scopedPeerUID, OriginInstanceUID: scopedUID, ProjectUID: scopedUID, HLCPhysicalMS: created.UnixMilli(), CreatedAt: created, Type: "project.metadata_updated", Actor: "account-a", Payload: []byte(`{"diff":{"example.flag":{"from":null,"to":true}},"revision_new":1}`)}
		h, err := RemoteEventContentHash(e)
		if err != nil {
			t.Fatal(err)
		}
		e.ContentHash = h
		s := scopedContractSubmission(t, "patchProjectMetadata", e)
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		s.Originals[0].Actor = "account-b"
		s.Originals[0].ContentHash, err = RemoteEventContentHash(s.Originals[0])
		if err != nil {
			t.Fatal(err)
		}
		if s.Validate() == nil {
			t.Fatal("original differs from the retained manifest member")
		}
		s.Originals[0] = e
		duplicate := s.Operation.Members[0]
		duplicate.Ordinal = 1
		s.Operation.Members = append(s.Operation.Members, duplicate)
		s.Originals = append(s.Originals, e)
		s.Operation.Digest, err = SourceOperationDigest(s.Operation)
		if err != nil {
			t.Fatal(err)
		}
		if s.Validate() == nil {
			t.Fatal("duplicate source identity accepted")
		}
	})
	t.Run("one_operation_many_import_originals", func(t *testing.T) {
		// One command can produce more originals than the component operation limit.
		events := make([]RemoteEvent, 0, 257)
		for i := 0; i < 257; i++ {
			issueUID := fmt.Sprintf("%026d", i+1000)
			shortID, err := shortid.Derive(issueUID, shortid.MinLength)
			if err != nil {
				t.Fatal(err)
			}
			// Match insertImportedIssue's replay-complete issue.created payload.
			payload := []byte(fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"Imported","body":"","status":"open","author":"account-a","metadata":{},"created_at":%q,"updated_at":%q,"source":"example-import","external_id":%q}`, issueUID, shortID, created.Format(EventTimestampFormat), created.Format(EventTimestampFormat), fmt.Sprintf("import-%d", i)))
			events = append(events, RemoteEvent{EventUID: fmt.Sprintf("%026d", i+1), HLCCounter: int64(i), OriginInstanceUID: scopedUID, ProjectUID: scopedUID, HLCPhysicalMS: created.UnixMilli(), CreatedAt: created, IssueUID: &issueUID, Type: "issue.created", Actor: "account-a", Payload: payload})
		}
		s := scopedContractSubmission(t, "importIssues", events...)
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("original_byte_budget", func(t *testing.T) {
		for _, tc := range []struct {
			spaces    int
			wantValid bool
		}{{(3 << 20) - 4096, true}, {3 << 20, false}} {
			event := RemoteEvent{EventUID: scopedPeerUID, OriginInstanceUID: scopedUID, ProjectUID: scopedUID, HLCPhysicalMS: created.UnixMilli(), CreatedAt: created, Type: "project.metadata_updated", Actor: "account-a", Payload: []byte(`{"diff":{"example.flag":{"from":null,"to":true}},"revision_new":1}` + strings.Repeat(" ", tc.spaces))}
			hash, err := RemoteEventContentHash(event)
			if err != nil {
				t.Fatal(err)
			}
			event.ContentHash = hash
			native, err := EncodeRelaySourceEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			if (len(native) <= 4<<20) != tc.wantValid {
				t.Fatalf("fixture boundary: encoded original=%d", len(native))
			}
			s := scopedContractSubmission(t, "patchProjectMetadata", event)
			// Exercise both initial native conversion and already authenticated custody.
			for _, retained := range []bool{false, true} {
				if retained {
					s.OriginalBytes = [][]byte{native}
				}
				if err := s.Validate(); (err == nil) != tc.wantValid {
					t.Errorf("retained=%v encoded=%d validate=%v", retained, len(native), err)
				}
				if _, err := EncodeScopedSourceSubmission(s); (err == nil) != tc.wantValid {
					t.Errorf("retained=%v encoded=%d encode=%v", retained, len(native), err)
				}
			}
			// Construct the hostile wire without admission; decode must enforce the same
			// byte limit before any receiver can persist or forward it.
			raw, err := s.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeScopedSourceSubmission(raw); (err == nil) != tc.wantValid {
				t.Errorf("encoded=%d decode=%v", len(native), err)
			}
		}
	})
	t.Run("whole_wire_budget", func(t *testing.T) {
		event := RemoteEvent{EventUID: scopedPeerUID, OriginInstanceUID: scopedUID, ProjectUID: scopedUID, HLCPhysicalMS: created.UnixMilli(), CreatedAt: created, Type: "project.metadata_updated", Actor: "account-a", Payload: []byte(`{"diff":{"example.flag":{"from":null,"to":true}},"revision_new":1}`)}
		hash, err := RemoteEventContentHash(event)
		if err != nil {
			t.Fatal(err)
		}
		event.ContentHash = hash
		s := scopedContractSubmission(t, "patchProjectMetadata", event)
		raw, err := EncodeScopedSourceSubmission(s)
		if err != nil {
			t.Fatal(err)
		}
		header, _, err := DecodeScopedDelivery(scopedContractDelivery(t, 1).EnvelopeBytes)
		if err != nil {
			t.Fatal(err)
		}
		header.ObjectUID, header.ObjectDigest = s.Operation.UID, s.Operation.Digest
		if _, err := EncodeScopedDelivery(header, raw); err != nil {
			t.Fatalf("valid source fits its delivery: %v", err)
		}
		// Exact source bytes become the opaque delivery body. Outer JSON
		// whitespace cannot admit a source that the next hop cannot carry.
		tooLargeForDelivery := bytes.Repeat([]byte(" "), MaxScopedCheckpointBytes+1)
		copy(tooLargeForDelivery[len(tooLargeForDelivery)-len(raw):], raw)
		fitsDelivery := tooLargeForDelivery[1:]
		decoded, err := DecodeScopedSourceSubmission(fitsDelivery)
		if err != nil {
			t.Fatalf("source at delivery body limit: %v", err)
		}
		retry, err := EncodeScopedSourceSubmission(decoded)
		if err != nil || !bytes.Equal(retry, fitsDelivery) {
			t.Fatalf("source at delivery body limit changed on retry: %v", err)
		}
		if _, err := EncodeScopedDelivery(header, retry); err != nil {
			t.Fatalf("source at delivery body limit cannot be forwarded: %v", err)
		}
		if _, err := EncodeScopedDelivery(header, tooLargeForDelivery); err == nil {
			t.Fatal("delivery body boundary fixture was accepted")
		}
		if _, err := DecodeScopedSourceSubmission(tooLargeForDelivery); err == nil {
			t.Error("source decoder accepted exact bytes too large for delivery")
		}
		s.WireBytes = tooLargeForDelivery
		if _, err := EncodeScopedSourceSubmission(s); err == nil {
			t.Error("source retry encoder accepted exact bytes too large for delivery")
		}
	})
}

func TestScopedContractCheckpointRequiredCollections(t *testing.T) {
	raw, err := json.Marshal(ScopedCollections{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projects", "issues", "comments", "labels", "definitions", "references", "provenance", "artifacts"} {
		if !bytes.Contains(raw, []byte(`"`+name+`":[]`)) {
			t.Fatalf("collection %s not explicit: %s", name, raw)
		}
	}

	var collections ScopedCollections
	if err := DecodeScopedRecord([]byte(`{"issues":[]}`), &collections); err == nil {
		t.Fatal("incomplete authoritative collections accepted")
	}
	raw, err = json.Marshal(scopedContractCollections())
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeScopedRecord(raw, &collections); err != nil {
		t.Fatal(err)
	}
	collections.Issues = []ScopedIssueState{{UID: scopedUID, ProjectUID: scopedUID}, {UID: scopedUID, ProjectUID: scopedUID}}
	if collections.Validate() == nil {
		t.Fatal("duplicate issue identity accepted")
	}
}

func TestScopedReferenceDefinitionUIDIsNotIssueUID(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	jobs := []cron.JobDefinition{
		{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"},
		{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "issue-deadline", IssueUID: scopedPeerUID}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Overlap: "forbid", Catchup: "skip"},
	}
	for n, job := range jobs {
		if err := job.Validate(); err != nil {
			t.Fatal(err)
		}
		document, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		definitionUID := "01ARZ3NDEKTSV4RRFFQ69G5FAX"
		raw, err := json.Marshal(CronDefinitionEvent{UID: definitionUID, ProjectUID: scopedUID, Name: "Review", Definition: document, Author: "worker", CreatedAt: now, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		event := RemoteEvent{EventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", OriginInstanceUID: scopedUID, ProjectUID: scopedUID, Type: "cron.job.created", Actor: "worker", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now, Payload: raw}
		if err := ValidateCronFederationEvent(event); err != nil {
			t.Fatal(err)
		}
		resolve := func(id string) (ScopedEndpoint, error) {
			if id == definitionUID {
				t.Fatal("definition UID resolved as an issue")
			}
			return ScopedEndpoint{ProjectUID: scopedUID, IssueUID: id}, nil
		}
		refs, err := ValidateScopedReferences(event, resolve, func(_, id string) (ScopedEndpoint, error) { return resolve(id) })
		if err != nil || len(refs) != n {
			t.Fatalf("cron typed references=%v err=%v", refs, err)
		}
	}
}

func TestScopedContractOldBindingNativeIdentity(t *testing.T) {
	for _, tc := range []struct {
		mode, binding string
		epoch         int64
		valid         bool
	}{
		{"v1", scopedUID, 1, true}, {"v1", "", 0, false},
		{"direct", "", 0, true}, {"direct", scopedUID, 1, false},
		{"detached", "", 0, true}, {"detached", scopedUID, 1, true},
		{"detached", scopedUID, 0, false}, {"detached", "", 1, false},
	} {
		old := ScopedOldBinding{ProjectUID: scopedUID, BindingUID: tc.binding, Epoch: tc.epoch, Mode: tc.mode}
		if err := old.Validate(); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", old, err)
		}
	}
}

func TestScopedReferenceCrossProjectAtomicAliases(t *testing.T) {
	projectP, projectQ := scopedUID, "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	subject, foreign, local := scopedUID, scopedPeerUID, "01ARZ3NDEKTSV4RRFFQ68G5FAW"
	otherLocal, otherForeign := "01ARZ3NDEKTSV4RRFFQ69G5FA0", "01ARZ3NDEKTSV4RRFFQ69G5FA1"
	selfAlias, err := shortid.Derive(subject, shortid.MinLength)
	if err != nil {
		t.Fatal(err)
	}
	peerAlias, err := shortid.Derive(foreign, shortid.MinLength)
	if err != nil {
		t.Fatal(err)
	}
	localAlias, err := shortid.Derive(local, shortid.MinLength)
	if err != nil || localAlias != peerAlias {
		t.Fatalf("fixture needs equal native aliases in different projects: %q %q %v", localAlias, peerAlias, err)
	}
	members := map[string]ScopedEndpoint{otherLocal: {ProjectUID: projectP, IssueUID: otherLocal}, otherForeign: {ProjectUID: projectQ, IssueUID: otherForeign}, subject: {ProjectUID: projectP, IssueUID: subject}, foreign: {ProjectUID: projectQ, IssueUID: foreign}, local: {ProjectUID: projectP, IssueUID: local}}
	resolve := func(id string) (ScopedEndpoint, error) {
		v, ok := members[id]
		if !ok {
			return ScopedEndpoint{}, fmt.Errorf("missing member")
		}
		return v, nil
	}
	short := func(project, alias string) (ScopedEndpoint, error) {
		if alias == selfAlias {
			return members[subject], nil
		}
		if alias != peerAlias {
			return ScopedEndpoint{}, fmt.Errorf("unknown alias")
		}
		if project == projectQ {
			return members[foreign], nil
		}
		return members[local], nil
	}
	cases := func(peer PeerIdentity) []struct {
		name    string
		changes AtomicEditChanges
	} {
		return []struct {
			name    string
			changes AtomicEditChanges
		}{
			{"parent add", AtomicEditChanges{ParentSet: &peer}}, {"parent remove", AtomicEditChanges{ParentRemoved: &peer}},
			{"blocks add", AtomicEditChanges{BlocksAdded: []PeerIdentity{peer}}}, {"blocks remove", AtomicEditChanges{BlocksRemoved: []PeerIdentity{peer}}},
			{"blocked by add", AtomicEditChanges{BlockedByAdded: []PeerIdentity{peer}}}, {"blocked by remove", AtomicEditChanges{BlockedByRemoved: []PeerIdentity{peer}}},
			{"related add", AtomicEditChanges{RelatedAdded: []PeerIdentity{peer}}}, {"related remove", AtomicEditChanges{RelatedRemoved: []PeerIdentity{peer}}},
		}
	}
	for _, target := range []struct {
		name     string
		peer     PeerIdentity
		endpoint ScopedEndpoint
		mismatch string
	}{
		{"local", PeerIdentity{ShortID: peerAlias, UID: local, Project: "alpha-project"}, members[local], otherLocal},
		{"foreign", PeerIdentity{ShortID: peerAlias, UID: foreign, Project: "beta-project"}, members[foreign], otherForeign},
	} {
		for _, tc := range cases(target.peer) {
			t.Run(target.name+"_"+tc.name, func(t *testing.T) {
				raw, err := LinksChangedPayload(tc.changes, "")
				if err != nil {
					t.Fatal(err)
				}
				refs, err := ValidateScopedReferences(RemoteEvent{Type: "issue.links_changed", ProjectUID: projectP, IssueUID: &subject, Payload: raw}, resolve, short)
				if err != nil || len(refs) != 2 || refs[1] != target.endpoint {
					t.Fatalf("native foreign alias mapped through wrong project: %v %v", refs, err)
				}
				mismatch := []byte(strings.ReplaceAll(string(raw), target.peer.UID, target.mismatch))
				if _, err := ValidateScopedReferences(RemoteEvent{Type: "issue.links_changed", ProjectUID: projectP, IssueUID: &subject, Payload: mismatch}, resolve, short); err == nil {
					t.Fatal("UID/short alias disagreement accepted")
				}
			})
		}
	}
	// Without an authoritative UID pair, legacy aliases remain local.
	refs, err := ValidateScopedReferences(RemoteEvent{Type: "issue.links_changed", ProjectUID: projectP, IssueUID: &subject, Payload: []byte(fmt.Sprintf(`{"blocks_added":[%q]}`, peerAlias))}, resolve, short)
	if err != nil || len(refs) != 2 || refs[1] != members[local] {
		t.Fatalf("legacy local alias changed: %v %v", refs, err)
	}
	for _, key := range []string{"parent_set", "parent_removed", "blocks_added", "blocks_removed", "blocked_by_added", "blocked_by_removed", "related_added", "related_removed"} {
		t.Run("self_"+key, func(t *testing.T) {
			value := fmt.Sprintf(`[%q]`, selfAlias)
			if strings.HasPrefix(key, "parent_") {
				value = fmt.Sprintf(`%q`, selfAlias)
			}
			raw := []byte(fmt.Sprintf(`{"%s":%s}`, key, value))
			if _, err := ValidateScopedReferences(RemoteEvent{Type: "issue.links_changed", ProjectUID: projectP, IssueUID: &subject, Payload: raw}, resolve, short); err == nil {
				t.Fatal("self reference accepted after short-alias resolution")
			}
		})
	}
}

func TestScopedContractDefinitionStructuralVersion(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		job      *cron.JobDefinition
		workflow *cron.WorkflowDefinition
		valid    bool
	}{
		// Minimal historical/tombstone documents need structural compatibility,
		// not today's scheduling, action, or workflow step business validation.
		{kind: "job", job: &cron.JobDefinition{Version: 1, Kind: "job"}, valid: true},
		{kind: "job", job: &cron.JobDefinition{Version: 99, Kind: "job"}},
		{kind: "job", job: &cron.JobDefinition{Version: 1, Kind: "future-kind"}},
		{kind: "workflow", workflow: &cron.WorkflowDefinition{Version: 1}, valid: true},
		{kind: "workflow", workflow: &cron.WorkflowDefinition{Version: 99}},
	} {
		state := ScopedDefinitionState{UID: scopedUID, ProjectUID: scopedPeerUID, Kind: tc.kind, Job: tc.job, Workflow: tc.workflow, Deleted: true}
		if err := state.Validate(); (err == nil) != tc.valid {
			t.Errorf("%s job=%+v workflow=%+v: %v", tc.kind, tc.job, tc.workflow, err)
		}
	}
}
