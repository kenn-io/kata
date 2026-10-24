package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/embedding"
)

func captureCodecUID(n int) string { return fmt.Sprintf("%026d", n) }

// This fixture is current state, not a history replay or source authority.
func captureCodecFixture() CanonicalViewState {
	p, q, a, b, c := captureCodecUID(1), captureCodecUID(2), captureCodecUID(3), captureCodecUID(4), captureCodecUID(5)
	stamp := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := FoldClock{HLCPhysicalMS: 42, HLCCounter: 3, OriginInstanceUID: captureCodecUID(10), EventUID: captureCodecUID(11)}
	intent := captureCodecUID(12) // A later same-state restatement has a different clock.
	boundary := FoldClock{HLCPhysicalMS: 31, HLCCounter: 1, OriginInstanceUID: captureCodecUID(10), EventUID: intent}
	closeValue := &ScopedCloseValue{Kind: "duplicate", PeerProjectUID: q, PeerIssueUID: b}
	owner, empty, zero := "member", "", int64(0)
	jobUID, workflowUID, runUID := captureCodecUID(20), captureCodecUID(21), captureCodecUID(22)
	hlc := CronDefinitionHLC{Version: 1, PhysicalMS: 42, Counter: 3, OriginInstanceUID: captureCodecUID(10)}
	definition := CronDefinition{UID: jobUID, Name: "example-job", DefinitionEventUID: captureCodecUID(11), DefinitionHLC: hlc, Author: "creator", Revision: 2, CreatedAt: stamp, UpdatedAt: stamp}
	workflow := definition
	workflow.UID, workflow.Name = workflowUID, "example-workflow"
	digest := strings.Repeat("a", 64)
	return CanonicalViewState{
		CaptureUID: captureCodecUID(30), ProjectCuts: map[string]int64{p: 9007199254740993, q: 0},
		Projects: []Project{{UID: p, Name: "alpha-project", Metadata: JSONBlob(`{"area":"example"}`), Revision: 1, CreatedAt: stamp}, {UID: q, Name: "beta-project", Revision: 1, CreatedAt: stamp}},
		Fold: FoldProjection{
			Issues: map[string]FoldIssue{
				a: {UID: a, ProjectUID: p, ShortID: "abcd", Title: "Example", Body: "Body", Author: "creator", Owner: &owner, Priority: &zero, AssignmentExpiresOn: &empty, Status: "closed", StatusIntentUID: intent, StatusClock: clock, CloseValue: closeValue, CloseBoundaryClock: &boundary, CreatedAt: stamp.Format(time.RFC3339), UpdatedAt: stamp.Format(time.RFC3339)},
				b: {UID: b, ProjectUID: q, ShortID: "bcde", Status: "open"},
			},
			Comments:        map[string]FoldComment{c: {UID: c, IssueUID: a, Author: "creator", Teammate: "peer", Body: "Latest body", ReplyToUID: captureCodecUID(99), ReplyKind: "reply", CreatedAt: stamp.Format(time.RFC3339), EditedAt: stamp.Format(time.RFC3339), Clock: clock}},
			Labels:          map[FoldLabelKey]FoldElementState{{IssueUID: a, Label: "retired"}: {Present: false, Clock: clock, Author: "member"}, {IssueUID: b, Label: "active"}: {Present: true, Clock: clock, Author: "member"}},
			Links:           map[FoldLinkKey]FoldElementState{{FromUID: a, ToUID: b, Type: "related"}: {Present: false, Clock: clock, Author: "member", CreatedAt: stamp.Format(time.RFC3339)}, {FromUID: a, ToUID: captureCodecUID(98), Type: "blocks"}: {Present: true, Clock: clock, Author: "member"}},
			IssueMetadata:   map[string]jsontext.Value{a: jsontext.Value(`{"custom":{"n":9007199254740993},"notify.member":{"message":"hello"},"someday":false}`), b: jsontext.Value(`{}`)},
			ProjectMetadata: map[string]jsontext.Value{p: jsontext.Value(`{"area":"example"}`)},
			CronJobs:        map[string]FoldCronJob{jobUID: {CronJob: CronJob{CronDefinition: definition, Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: false, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60}, Action: cron.Action{Kind: "process", WorkflowUID: workflowUID}, Overlap: "allow", Catchup: "all"}}, ProjectUID: p}},
			CronWorkflows:   map[string]FoldCronWorkflow{workflowUID: {CronWorkflow: CronWorkflow{CronDefinition: workflow, Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "command", Command: "true"}}}}, ProjectUID: p}},
			CronRuns:        map[string]FoldCronRun{runUID: {CronRun: CronRun{UID: runUID, JobUID: &jobUID, DefinitionEventUID: &intent, Actor: "member", Status: "succeeded", Summary: cron.Summary{Version: 1, Message: "done"}, Revision: 1, CreatedAt: stamp, UpdatedAt: stamp}, ProjectUID: p, ObservationEventUID: captureCodecUID(11), ObservationHLC: hlc}},
			Warnings:        []string{"example retained warning"},
		},
		Creation:  []ScopedProvenance{{ProjectUID: p, Kind: "project", EntityUID: p, Creator: "creator", AttributionStatus: "legacy", Signatures: []ScopedSignature{}}, {ProjectUID: p, Kind: "issue", EntityUID: a, Creator: "creator", AttributionStatus: "unverified", Signatures: []ScopedSignature{}}, {ProjectUID: p, Kind: "comment", EntityUID: c, Creator: "creator", Teammate: "peer", AttributionStatus: "verified", Signatures: []ScopedSignature{}}, {ProjectUID: p, Kind: "cron_job", EntityUID: jobUID, Creator: "creator", AttributionStatus: "legacy", Signatures: []ScopedSignature{}}, {ProjectUID: p, Kind: "cron_workflow", EntityUID: workflowUID, Creator: "creator", AttributionStatus: "legacy", Signatures: []ScopedSignature{}}},
		Close:     []CanonicalCloseState{{IssueUID: a, StatusIntentUID: intent, StatusClock: clock, CloseBoundaryClock: &boundary, Value: closeValue}, {IssueUID: b, Value: nil}},
		Artifacts: []embedding.ArtifactManifest{{ArtifactIdentity: embedding.ArtifactIdentity{ProjectUID: p, IssueUID: a, ProducerInstanceUID: captureCodecUID(10), InputHash: digest, RecipeFingerprint: digest, Provider: "example", Model: "example", Dimensions: 1, InputType: "text", Normalization: "none", Preprocessing: "none", RecipeVersion: 1, SplitMaxRunes: 100, Encoding: embedding.ArtifactFloat32Encoding}, Chunks: []embedding.ChunkManifest{{Index: 0, InputHash: digest, VectorDigest: digest}}, ChunkCount: 1, VectorByteSize: 4, Digest: digest}},
	}
}

// Dropping json:- status provenance or struct-key tombstones breaks this roundtrip.
func TestScopedCaptureCodecRoundtrip(t *testing.T) {
	want := captureCodecFixture()
	raw, err := EncodeScopedCanonicalCapture(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeScopedCanonicalCapture(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Creation is an unordered inventory; the codec sorts its portable records.
	if !reflect.DeepEqual(got.Fold, want.Fold) {
		t.Fatalf("fold lost current state:\ngot %#v\nwant %#v", got.Fold, want.Fold)
	}
	if !reflect.DeepEqual(got.Close, want.Close) || !reflect.DeepEqual(got.Projects, want.Projects) || !reflect.DeepEqual(got.Artifacts, want.Artifacts) || !reflect.DeepEqual(got.ProjectCuts, want.ProjectCuts) || got.CaptureUID != want.CaptureUID {
		t.Fatal("capture context, nullable close proof or artifact changed")
	}
	for _, proof := range want.Creation {
		found := false
		for _, v := range got.Creation {
			if reflect.DeepEqual(v, proof) {
				found = true
			}
		}
		if !found {
			t.Fatalf("creator evidence changed: %#v", proof)
		}
	}
	again, err := EncodeScopedCanonicalCapture(got)
	if err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("unstable bytes: %v", err)
	}
	if !bytes.Contains(raw, []byte(`9007199254740993`)) {
		t.Fatal("private int64 cut lost precision")
	}
	t.Run("close_boundary_legacy", func(t *testing.T) {
		state := want
		a := captureCodecUID(3)
		boundary := []byte(`"close_boundary_clock":{"EventUID":"` + captureCodecUID(12) + `","HLCCounter":1,"HLCPhysicalMS":31,"OriginInstanceUID":"` + captureCodecUID(10) + `"},`)
		if !bytes.Contains(raw, boundary) {
			t.Fatal("fixture missing boundary")
		}
		// A legacy capture cannot upgrade its old typed value from a latest clock.
		if _, err := DecodeScopedCanonicalCapture(bytes.ReplaceAll(raw, boundary, nil)); err == nil {
			t.Fatal("legacy typed value accepted without original intent boundary")
		}
		state.Close[0].Value = nil
		state.Close[0].CloseBoundaryClock = nil
		issue := state.Fold.Issues[a]
		issue.CloseValue = nil
		issue.CloseBoundaryClock = nil
		state.Fold.Issues[a] = issue
		legacy, err := EncodeScopedCanonicalCapture(state)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeScopedCanonicalCapture(legacy)
		if err != nil {
			t.Fatal("legacy boundary-unknown capture must decode without typed value", err)
		}
		if decoded.Close[0].CloseBoundaryClock != nil || decoded.Close[0].Value != nil || decoded.Fold.Issues[a].CloseBoundaryClock != nil {
			t.Fatal("legacy capture invented original intent proof")
		}
	})
}

func TestScopedCaptureCodecMapPermutation(t *testing.T) {
	original := captureCodecFixture()
	baseline, err := EncodeScopedCanonicalCapture(original)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 40; trial++ {
		state := captureCodecFixture()
		keys := []string{captureCodecUID(3), captureCodecUID(4)}
		rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		state.Fold.Issues = map[string]FoldIssue{}
		state.Fold.IssueMetadata = map[string]jsontext.Value{}
		for _, k := range keys {
			state.Fold.Issues[k] = original.Fold.Issues[k]
			state.Fold.IssueMetadata[k] = original.Fold.IssueMetadata[k]
		}
		labels := []FoldLabelKey{}
		for k := range original.Fold.Labels {
			labels = append(labels, k)
		}
		rng.Shuffle(len(labels), func(i, j int) { labels[i], labels[j] = labels[j], labels[i] })
		state.Fold.Labels = map[FoldLabelKey]FoldElementState{}
		for _, k := range labels {
			state.Fold.Labels[k] = original.Fold.Labels[k]
		}
		links := []FoldLinkKey{}
		for k := range original.Fold.Links {
			links = append(links, k)
		}
		rng.Shuffle(len(links), func(i, j int) { links[i], links[j] = links[j], links[i] })
		state.Fold.Links = map[FoldLinkKey]FoldElementState{}
		for _, k := range links {
			state.Fold.Links[k] = original.Fold.Links[k]
		}
		rng.Shuffle(len(state.Projects), func(i, j int) { state.Projects[i], state.Projects[j] = state.Projects[j], state.Projects[i] })
		rng.Shuffle(len(state.Creation), func(i, j int) { state.Creation[i], state.Creation[j] = state.Creation[j], state.Creation[i] })
		rng.Shuffle(len(state.Close), func(i, j int) { state.Close[i], state.Close[j] = state.Close[j], state.Close[i] })
		raw, err := EncodeScopedCanonicalCapture(state)
		if err != nil || !bytes.Equal(raw, baseline) {
			t.Fatalf("map permutation %d changed capture: %v", trial, err)
		}
	}
}

func TestScopedCaptureCodecPortableCopies(t *testing.T) {
	state := captureCodecFixture()
	state.Projects[0].ID = 71
	state.Fold.IssueMetadata[captureCodecUID(3)] = jsontext.Value(`{"notify.member":{"message":"hello"},"custom":{"n":9007199254740993},"someday":false}`)
	jobUID, runUID := captureCodecUID(20), captureCodecUID(22)
	job := state.Fold.CronJobs[jobUID]
	job.ID = 72
	job.ProjectID = 71
	state.Fold.CronJobs[jobUID] = job
	run := state.Fold.CronRuns[runUID]
	run.ID = 73
	run.ProjectID = 71
	state.Fold.CronRuns[runUID] = run
	state.Fold.cronAuthors = map[string]foldCronAuthorCandidate{"job:" + jobUID: {author: "creator", snapshot: true}}
	raw, err := EncodeScopedCanonicalCapture(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeScopedCanonicalCapture(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Projects[0].ID != 0 || got.Fold.CronJobs[jobUID].ID != 0 || got.Fold.CronJobs[jobUID].ProjectID != 0 || got.Fold.CronRuns[runUID].ID != 0 || got.Fold.CronRuns[runUID].ProjectID != 0 || got.Fold.cronAuthors != nil {
		t.Fatal("capture retained local IDs or incremental working state")
	}
	if state.Projects[0].ID != 71 || state.Fold.CronJobs[jobUID].ID != 72 || state.Fold.CronRuns[runUID].ID != 73 || state.Fold.cronAuthors["job:"+jobUID].author != "creator" {
		t.Fatal("encode mutated native state")
	}
	*got.Fold.Issues[captureCodecUID(3)].Owner = "changed"
	got.Close[0].Value.Kind = "superseded"
	got.Fold.IssueMetadata[captureCodecUID(3)][0] = '!'
	if *state.Fold.Issues[captureCodecUID(3)].Owner != "member" || state.Close[0].Value.Kind != "duplicate" {
		t.Fatal("decoded capture aliases input state")
	}
	raw[0] = '!'
	if !bytes.Equal(state.Fold.IssueMetadata[captureCodecUID(3)], []byte(`{"notify.member":{"message":"hello"},"custom":{"n":9007199254740993},"someday":false}`)) {
		t.Fatal("encode canonicalized input metadata in place")
	}
}

func TestScopedCaptureCodecRejectInvalidState(t *testing.T) {
	p, a, b := captureCodecUID(1), captureCodecUID(3), captureCodecUID(4)
	cases := []struct {
		name   string
		mutate func(*CanonicalViewState)
	}{
		{"capture UID", func(s *CanonicalViewState) { s.CaptureUID = "bad" }},
		{"missing cut", func(s *CanonicalViewState) { delete(s.ProjectCuts, p) }},
		{"extra cut", func(s *CanonicalViewState) { s.ProjectCuts[captureCodecUID(80)] = 0 }},
		{"negative cut", func(s *CanonicalViewState) { s.ProjectCuts[p] = -1 }},
		{"duplicate project", func(s *CanonicalViewState) { s.Projects = append(s.Projects, s.Projects[0]) }},
		{"issue key mismatch", func(s *CanonicalViewState) { s.Fold.Issues[b] = s.Fold.Issues[a] }},
		{"issue outside capture", func(s *CanonicalViewState) {
			v := s.Fold.Issues[a]
			v.ProjectUID = captureCodecUID(80)
			s.Fold.Issues[a] = v
		}},
		{"comment key mismatch", func(s *CanonicalViewState) { s.Fold.Comments[a] = s.Fold.Comments[captureCodecUID(5)] }},
		{"comment owner missing", func(s *CanonicalViewState) {
			v := s.Fold.Comments[captureCodecUID(5)]
			v.IssueUID = captureCodecUID(90)
			s.Fold.Comments[v.UID] = v
		}},
		{"label owner missing", func(s *CanonicalViewState) {
			s.Fold.Labels[FoldLabelKey{IssueUID: captureCodecUID(90), Label: "example"}] = FoldElementState{}
		}},
		{"metadata owner missing", func(s *CanonicalViewState) { s.Fold.IssueMetadata[captureCodecUID(90)] = jsontext.Value(`{}`) }},
		{"project metadata owner missing", func(s *CanonicalViewState) { s.Fold.ProjectMetadata[captureCodecUID(90)] = jsontext.Value(`{}`) }},
		{"duplicate metadata JSON", func(s *CanonicalViewState) { s.Fold.IssueMetadata[a] = jsontext.Value(`{"x":1,"x":2}`) }},
		{"partial clock", func(s *CanonicalViewState) {
			v := s.Fold.Issues[b]
			v.StatusClock = FoldClock{HLCPhysicalMS: 1}
			s.Fold.Issues[b] = v
		}},
		{"duplicate close", func(s *CanonicalViewState) { s.Close = append(s.Close, s.Close[0]) }},
		{"close clock mismatch", func(s *CanonicalViewState) { s.Close[0].StatusClock.HLCCounter++ }},
		{"close intent mismatch", func(s *CanonicalViewState) { s.Close[0].StatusIntentUID = captureCodecUID(90) }},
		{"close open issue", func(s *CanonicalViewState) { v := s.Fold.Issues[a]; v.Status = "open"; s.Fold.Issues[a] = v }},
		{"close peer ownership", func(s *CanonicalViewState) { s.Close[0].Value.PeerProjectUID = p }},
		{"link type", func(s *CanonicalViewState) {
			s.Fold.Links[FoldLinkKey{FromUID: a, ToUID: b, Type: "future"}] = FoldElementState{}
		}},
		{"related direction", func(s *CanonicalViewState) {
			s.Fold.Links[FoldLinkKey{FromUID: b, ToUID: a, Type: "related"}] = FoldElementState{}
		}},
		{"link without captured endpoint", func(s *CanonicalViewState) {
			s.Fold.Links[FoldLinkKey{FromUID: captureCodecUID(91), ToUID: captureCodecUID(92), Type: "blocks"}] = FoldElementState{}
		}},
		{"unknown close kind", func(s *CanonicalViewState) { s.Close[0].Value.Kind = "future" }},
		{"creation ownership", func(s *CanonicalViewState) { s.Creation[1].ProjectUID = captureCodecUID(2) }},
		{"unknown creation kind", func(s *CanonicalViewState) { s.Creation[0].Kind = "future" }},
		{"job key mismatch", func(s *CanonicalViewState) { s.Fold.CronJobs[a] = s.Fold.CronJobs[captureCodecUID(20)] }},
		{"job version", func(s *CanonicalViewState) {
			v := s.Fold.CronJobs[captureCodecUID(20)]
			v.Definition.Version = 2
			s.Fold.CronJobs[v.UID] = v
		}},
		{"workflow version", func(s *CanonicalViewState) {
			v := s.Fold.CronWorkflows[captureCodecUID(21)]
			v.Definition.Version = 2
			s.Fold.CronWorkflows[v.UID] = v
		}},
		{"observation HLC version", func(s *CanonicalViewState) {
			v := s.Fold.CronRuns[captureCodecUID(22)]
			v.ObservationHLC.Version = 2
			s.Fold.CronRuns[v.UID] = v
		}},
		{"summary version", func(s *CanonicalViewState) {
			v := s.Fold.CronRuns[captureCodecUID(22)]
			v.Summary.Version = 2
			s.Fold.CronRuns[v.UID] = v
		}},
		{"artifact ownership", func(s *CanonicalViewState) { s.Artifacts[0].ProjectUID = captureCodecUID(2) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := captureCodecFixture()
			tc.mutate(&state)
			if _, err := EncodeScopedCanonicalCapture(state); err == nil {
				t.Fatal("accepted invalid capture")
			}
		})
	}
}

func TestScopedCaptureCodecStrictDecode(t *testing.T) {
	raw, err := EncodeScopedCanonicalCapture(captureCodecFixture())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		old, new string
	}{
		{"version", `"version":1`, `"version":2`},
		{"unknown field", `"version":1`, `"future":false,"version":1`},
		{"duplicate field", `"version":1`, `"version":1,"version":1`},
		{"missing array", `,"warnings":["example retained warning"]`, ``},
		{"null array", `"warnings":["example retained warning"]`, `"warnings":null`},
		{"null scalar", `"cut":0`, `"cut":null`},
		{"nonportable ID", `"id":0`, `"id":17`},
		{"nested unknown", `"Status":"closed"`, `"Status":"closed","Future":true`},
		{"status wrapper omitted", `,"status_intent_uid":"` + captureCodecUID(12) + `"`, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Contains(raw, []byte(tc.old)) {
				t.Fatalf("fixture missing %s", tc.old)
			}
			bad := bytes.Replace(raw, []byte(tc.old), []byte(tc.new), 1)
			if tc.name == "version" {
				pos := bytes.LastIndex(raw, []byte(tc.old))
				bad = append(bytes.Clone(raw[:pos]), append([]byte(tc.new), raw[pos+len(tc.old):]...)...)
			}
			if _, err := DecodeScopedCanonicalCapture(bad); err == nil {
				t.Fatal("accepted malformed typed capture")
			}
		})
	}
	for _, bad := range [][]byte{nil, []byte(`null`), []byte(`{`), append(bytes.Clone(raw), []byte(` {}`)...)} {
		if _, err := DecodeScopedCanonicalCapture(bad); err == nil {
			t.Fatal("accepted invalid JSON document")
		}
	}
	// Duplicate typed keys must not be overwritten during array-to-map conversion.
	var dto struct {
		Version     int              `json:"version"`
		CaptureUID  string           `json:"capture_uid"`
		ProjectCuts []jsontext.Value `json:"project_cuts"`
		Projects    []jsontext.Value `json:"projects"`
		Fold        jsontext.Value   `json:"fold"`
		Creation    []jsontext.Value `json:"creation"`
		Close       []jsontext.Value `json:"close"`
		Artifacts   []jsontext.Value `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatal(err)
	}
	dto.ProjectCuts = append(dto.ProjectCuts, dto.ProjectCuts[0])
	bad, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeScopedCanonicalCapture(bad); err == nil {
		t.Fatal("duplicate project cut accepted")
	}
	type wireFold struct {
		Issues          []jsontext.Value `json:"issues"`
		Comments        []jsontext.Value `json:"comments"`
		Labels          []jsontext.Value `json:"labels"`
		Links           []jsontext.Value `json:"links"`
		IssueMetadata   []jsontext.Value `json:"issue_metadata"`
		ProjectMetadata []jsontext.Value `json:"project_metadata"`
		CronJobs        []jsontext.Value `json:"cron_jobs"`
		CronWorkflows   []jsontext.Value `json:"cron_workflows"`
		CronRuns        []jsontext.Value `json:"cron_runs"`
		Warnings        []string         `json:"warnings"`
	}
	for _, family := range []string{"issues", "comments", "labels", "links", "issue_metadata", "project_metadata", "cron_jobs", "cron_workflows", "cron_runs", "creation", "close", "artifacts"} {
		t.Run("duplicate "+family, func(t *testing.T) {
			if err := json.Unmarshal(raw, &dto); err != nil {
				t.Fatal(err)
			}
			var f wireFold
			if err := json.Unmarshal(dto.Fold, &f); err != nil {
				t.Fatal(err)
			}
			var values *[]jsontext.Value
			switch family {
			case "issues":
				values = &f.Issues
			case "comments":
				values = &f.Comments
			case "labels":
				values = &f.Labels
			case "links":
				values = &f.Links
			case "issue_metadata":
				values = &f.IssueMetadata
			case "project_metadata":
				values = &f.ProjectMetadata
			case "cron_jobs":
				values = &f.CronJobs
			case "cron_workflows":
				values = &f.CronWorkflows
			case "cron_runs":
				values = &f.CronRuns
			case "creation":
				values = &dto.Creation
			case "close":
				values = &dto.Close
			case "artifacts":
				values = &dto.Artifacts
			}
			*values = append(*values, (*values)[0])
			dto.Fold, err = json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			bad, err := json.Marshal(dto)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeScopedCanonicalCapture(bad); err == nil {
				t.Fatal("duplicate typed key accepted")
			}
		})
	}
}

func TestScopedCaptureCodecCapacity(t *testing.T) {
	state := captureCodecFixture()
	a := captureCodecUID(3)
	state.Fold.IssueMetadata[a] = jsontext.Value(`{"number":1e20}`)
	v := state.Fold.Issues[a]
	v.Body = ""
	state.Fold.Issues[a] = v
	baseline, err := EncodeScopedCanonicalCapture(state)
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("x", MaxScopedCheckpointBytes-len(baseline))
	v.Body = padding
	state.Fold.Issues[a] = v
	atLimit, err := EncodeScopedCanonicalCapture(state)
	if err != nil || len(atLimit) != MaxScopedCheckpointBytes {
		t.Fatalf("capture at assembled limit refused: size=%d error=%v", len(atLimit), err)
	}
	t.Run("exact canonical boundary", func(t *testing.T) {
		if _, err := DecodeScopedCanonicalCapture(atLimit); err != nil {
			t.Fatal("valid canonical capture at limit must decode:", err)
		}
	})
	t.Run("one byte over canonical encode", func(t *testing.T) {
		v.Body = padding + "x"
		state.Fold.Issues[a] = v
		if raw, err := EncodeScopedCanonicalCapture(state); err == nil {
			t.Fatalf("canonical assembled capture escaped cap: %d bytes", len(raw))
		}
	})
	t.Run("oversized raw decode", func(t *testing.T) {
		// Unlike whitespace alone, this is valid JSON whose canonical form is
		// exactly at the limit; only the raw byte budget must refuse it.
		oversized := append(bytes.Clone(atLimit), ' ')
		if _, err := DecodeScopedCanonicalCapture(oversized); err == nil {
			t.Fatal("accepted valid raw capture above cap")
		}
	})
	t.Run("short exponent expands on decode", func(t *testing.T) {
		compact := bytes.Replace(baseline, []byte(`100000000000000000000`), []byte(`1e20`), 1)
		if bytes.Equal(compact, baseline) {
			t.Fatal("fixture did not canonicalize its number")
		}
		compact = bytes.Replace(compact, []byte(`"Body":""`), []byte(`"Body":"`+padding+`x"`), 1)
		if len(compact) > MaxScopedCheckpointBytes {
			t.Fatal("fixture exceeded raw input cap")
		}
		if _, err := DecodeScopedCanonicalCapture(compact); err == nil {
			t.Fatal("decoder accepted assembled canonical capture above cap")
		}
	})
}

func TestScopedCaptureCodecEmptyCollectionsAndLegacyClose(t *testing.T) {
	p, a := captureCodecUID(1), captureCodecUID(3)
	state := CanonicalViewState{CaptureUID: captureCodecUID(30), ProjectCuts: map[string]int64{p: 0}, Projects: []Project{{UID: p, Name: "example-project"}}, Fold: FoldProjection{Issues: map[string]FoldIssue{a: {UID: a, ProjectUID: p, Status: "closed"}}}}
	raw, err := EncodeScopedCanonicalCapture(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeScopedCanonicalCapture(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Close) != 0 || got.Fold.Issues[a].StatusIntentUID != "" {
		t.Fatal("legacy closed state manufactured proof")
	}
	if got.Close == nil || got.Creation == nil || got.Artifacts == nil || got.Fold.Labels == nil || got.Fold.Links == nil || got.Fold.Comments == nil || got.Fold.IssueMetadata == nil || got.Fold.ProjectMetadata == nil || got.Fold.CronJobs == nil || got.Fold.CronWorkflows == nil || got.Fold.CronRuns == nil || got.Fold.Warnings == nil {
		t.Fatal("empty authoritative collections were not explicit")
	}
	if state.Close != nil || state.Fold.Labels != nil || state.Fold.Warnings != nil {
		t.Fatal("encode filled input collections")
	}
}

// Native ValidateCommentReply forbids self targets without requiring a peer to
// have arrived. Both capture entry points must preserve that structural rule.
func TestScopedCaptureCodecSelfReply(t *testing.T) {
	state := captureCodecFixture()
	commentUID := captureCodecUID(5)
	raw, err := EncodeScopedCanonicalCapture(state)
	if err != nil {
		t.Fatal(err)
	}
	comment := state.Fold.Comments[commentUID]
	comment.ReplyToUID = commentUID
	state.Fold.Comments[commentUID] = comment
	t.Run("encode", func(t *testing.T) {
		if _, err := EncodeScopedCanonicalCapture(state); err == nil {
			t.Fatal("encoded self reply")
		}
	})
	t.Run("decode", func(t *testing.T) {
		old := []byte(`"ReplyToUID":"` + captureCodecUID(99) + `"`)
		if !bytes.Contains(raw, old) {
			t.Fatal("fixture missing reply target")
		}
		bad := bytes.Replace(raw, old, []byte(`"ReplyToUID":"`+commentUID+`"`), 1)
		if _, err := DecodeScopedCanonicalCapture(bad); err == nil {
			t.Fatal("decoded self reply")
		}
	})
}

// Canonical native links and ProjectScopedState forbid self edges, including
// tombstones. This is a local structural check, not graph cycle validation.
func TestScopedCaptureCodecSelfLink(t *testing.T) {
	for _, kind := range []string{"related", "blocks", "parent"} {
		t.Run(kind, func(t *testing.T) {
			state := captureCodecFixture()
			a, b := captureCodecUID(3), captureCodecUID(4)
			raw, err := EncodeScopedCanonicalCapture(state)
			if err != nil {
				t.Fatal(err)
			}
			state.Fold.Links[FoldLinkKey{FromUID: a, ToUID: a, Type: kind}] = FoldElementState{Present: false}
			t.Run("encode", func(t *testing.T) {
				if _, err := EncodeScopedCanonicalCapture(state); err == nil {
					t.Fatal("encoded self link")
				}
			})
			t.Run("decode", func(t *testing.T) {
				old := []byte(`"FromUID":"` + a + `","ToUID":"` + b + `","Type":"related"`)
				if !bytes.Contains(raw, old) {
					t.Fatal("fixture missing related edge")
				}
				bad := bytes.Replace(raw, old, []byte(`"FromUID":"`+a+`","ToUID":"`+a+`","Type":"`+kind+`"`), 1)
				if _, err := DecodeScopedCanonicalCapture(bad); err == nil {
					t.Fatal("decoded self link")
				}
			})
		})
	}
}
