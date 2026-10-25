//go:build rcpl_private

package db_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/embedding"
)

func projectorUID(n int) string { return fmt.Sprintf("%026d", n) }
func projectorFixture(t *testing.T, projects ...int) (db.CanonicalViewState, db.ScopedViewDescriptor, db.RootSigners) {
	t.Helper()
	d := db.ScopedViewDescriptor{Version: 2, Schema: 1, Key: db.ScopedViewKey{ViewUID: projectorUID(90), Generation: 1}, AuthorityUID: projectorUID(91), DestinationUID: projectorUID(92), RouteDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), Route: []db.ScopedHopBinding{{BindingUID: projectorUID(93), InstanceUID: projectorUID(92), Epoch: 1}}}
	c := db.CanonicalViewState{CaptureUID: projectorUID(94), ProjectCuts: map[string]int64{}, Fold: db.FoldProjection{Issues: map[string]db.FoldIssue{}, Comments: map[string]db.FoldComment{}, Labels: map[db.FoldLabelKey]db.FoldElementState{}, Links: map[db.FoldLinkKey]db.FoldElementState{}}}
	keys := db.RootSigners{}
	for _, n := range []int{1, 2, 3} {
		p, i := projectorUID(n), projectorUID(n+10)
		c.Projects = append(c.Projects, db.Project{UID: p, Name: fmt.Sprintf("project-%d", n), Metadata: db.JSONBlob(`{}`)})
		c.Fold.Issues[i] = db.FoldIssue{UID: i, ProjectUID: p, ShortID: fmt.Sprintf("i%d", n), Title: fmt.Sprintf("Issue %d", n), Body: "authored body", Author: "author", Status: "open", CreatedAt: "2026-01-01T00:00:00Z"}
		c.Creation = append(c.Creation, db.ScopedProvenance{ProjectUID: p, Kind: "issue", EntityUID: i, Creator: "author", AttributionStatus: "legacy"})
		seed := sha256.Sum256([]byte(p))
		private := ed25519.NewKeyFromSeed(seed[:])
		pub := private.Public().(ed25519.PublicKey)
		keys[p] = db.RootAttributionSigner{AuthorityUID: d.AuthorityUID, PrivateKey: private}
		for _, selected := range projects {
			if n == selected {
				d.Projects = append(d.Projects, db.ScopedProjectPin{ProjectUID: p, Pin: db.RootKeyPin{ProjectUID: p, AuthorityUID: d.AuthorityUID, KeyID: db.RootPublicKeyID(pub), PublicKey: pub}})
			}
		}
	}
	return c, d, keys
}
func project(t *testing.T, c db.CanonicalViewState, d db.ScopedViewDescriptor) db.ScopedCollections {
	t.Helper()
	got, err := db.ProjectScopedState(c, d)
	if err != nil {
		t.Fatal(err)
	}
	if err = got.Validate(); err != nil {
		t.Fatal(err)
	}
	return got
}
func projectedBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func reference(t *testing.T, c db.ScopedCollections, kind, subject string) db.ScopedReference {
	t.Helper()
	for _, r := range c.References {
		if r.Kind == kind && (r.Subject.IssueUID == subject || r.SubjectEntityUID == subject) {
			return r
		}
	}
	t.Fatalf("missing %s register for %s", kind, subject)
	return db.ScopedReference{}
}

// Contract: hidden link and close activity cannot change current alpha content,
// its ETag, or its view-local position. Copying source clocks breaks this test.
func TestScopedHiddenActivityEquivalent(t *testing.T) {
	c, d, _ := projectorFixture(t, 1)
	i := projectorUID(11)
	issue := c.Fold.Issues[i]
	issue.Status = "closed"
	issue.StatusIntentUID = projectorUID(81)
	c.Fold.Issues[i] = issue
	before := project(t, c, d)
	c.ProjectCuts[projectorUID(1)] = 999
	c.CaptureUID = projectorUID(95)
	issue.UpdatedAt = "2099-01-01T00:00:00Z"
	reason := "HIDDEN-CLOSE-MARKER"
	issue.ClosedReason = &reason
	issue.ClosedAt = &issue.UpdatedAt
	c.Fold.Issues[i] = issue
	c.Fold.Links[db.FoldLinkKey{FromUID: i, ToUID: projectorUID(13), Type: "blocks"}] = db.FoldElementState{Present: true, Clock: db.FoldClock{HLCPhysicalMS: 999}}
	c.Close = []db.CanonicalCloseState{{IssueUID: i, StatusIntentUID: issue.StatusIntentUID, Value: &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: projectorUID(3), PeerIssueUID: projectorUID(13)}}}
	after := project(t, c, d)
	if !bytes.Equal(projectedBytes(t, before), projectedBytes(t, after)) {
		t.Fatal("hidden activity changed scoped state")
	}
	a, err := db.ScopedStateDigest(before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.ScopedStateDigest(after)
	if err != nil || a != b {
		t.Fatal("hidden activity changed content ETag", err)
	}
	effects, err := db.DiffScopedState(before, after, 0)
	if err != nil || len(effects) != 0 {
		t.Fatal("hidden-only activity allocated effects", effects, err)
	}
	for _, marker := range []string{reason, "2099", projectorUID(13)} {
		if bytes.Contains(projectedBytes(t, after), []byte(marker)) {
			t.Fatalf("leaked %s", marker)
		}
	}
}

// Contract: creation snapshots containing hidden references still introduce all
// permitted current fields, comments and labels. Originals are never exported.
func TestScopedCompositeIntroduction(t *testing.T) {
	c, d, _ := projectorFixture(t, 1)
	i := projectorUID(11)
	c.Fold = db.FoldEvents([]db.FoldEvent{
		{UID: projectorUID(51), ProjectUID: projectorUID(1), IssueUID: i, Type: "issue.created", Actor: "author", HLCPhysicalMS: 1, CreatedAt: "2026-01-01T00:00:00Z", Payload: []byte(`{"uid":"` + i + `","title":"Composite","body":"authored body","status":"open","links":[{"to_issue_uid":"` + projectorUID(13) + `","type":"parent"}],"labels":["ready"]}`)},
		{UID: projectorUID(52), ProjectUID: projectorUID(1), IssueUID: i, Type: "issue.assigned", Actor: "author", HLCPhysicalMS: 2, Payload: []byte(`{"owner":"reader"}`)},
	})
	got := project(t, c, d)
	if len(got.Issues) != 1 || got.Issues[0].Title != "Composite" || got.Issues[0].Owner == nil || *got.Issues[0].Owner != "reader" {
		t.Fatalf("lost composite introduction: %+v", got.Issues)
	}
	if len(got.Labels) != 1 || got.Labels[0].Label != "ready" {
		t.Fatal("lost embedded label", got.Labels)
	}
	if r := reference(t, got, "parent", i); r.Present || r.Peer != nil {
		t.Fatal("hidden parent survived", r)
	}
	if bytes.Contains(projectedBytes(t, got), []byte(projectorUID(13))) {
		t.Fatal("composite leaked hidden parent")
	}
}

// Contract: current artifacts commit exact permitted text. Stale/generated
// hidden close text vectors must be omitted, rather than relabeled as current.
func TestScopedArtifactInput(t *testing.T) {
	c, d, _ := projectorFixture(t, 1)
	identity := embedding.ArtifactIdentity{ProjectUID: projectorUID(1), IssueUID: projectorUID(11), ProducerInstanceUID: projectorUID(91), RecipeFingerprint: strings.Repeat("c", 64), Provider: "provider", Model: "model", Dimensions: 2, InputType: "document", Normalization: "none", Preprocessing: "plain", RecipeVersion: 1, SplitMaxRunes: 1000, Encoding: embedding.ArtifactFloat32Encoding}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText("Issue 1", "authored body"), [][]float32{{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := embedding.NewArtifact(identity, embedding.EmbedText("Issue 1", "HIDDEN-GENERATED-CLOSE-MARKER"), [][]float32{{3, 4}})
	if err != nil {
		t.Fatal(err)
	}
	c.Artifacts = []embedding.ArtifactManifest{hidden.Manifest(), artifact.Manifest()}
	got := project(t, c, d)
	if len(got.Artifacts) != 1 || got.Artifacts[0].Digest != artifact.Digest {
		t.Fatal("artifact input eligibility failed", got.Artifacts)
	}
	c.Artifacts[1].Chunks[0].Index = 4
	if _, err = db.ProjectScopedState(c, d); err == nil {
		t.Fatal("invalid retained manifest accepted")
	}
}

func TestScopedProjectRestrictionAndDiff(t *testing.T) {
	c, broad, _ := projectorFixture(t, 1, 2, 3)
	_, narrow, _ := projectorFixture(t, 1)
	c.Fold.Links[db.FoldLinkKey{FromUID: projectorUID(11), ToUID: projectorUID(12), Type: "blocks"}] = db.FoldElementState{Present: true}
	want := project(t, c, narrow)
	full := project(t, c, broad)
	got, err := db.RestrictScopedState(full, []string{projectorUID(1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("restriction differs from narrow projection\nwant=%s\ngot=%s", projectedBytes(t, want), projectedBytes(t, got))
	}
	next := project(t, c, narrow)
	next.Issues[0].Title = "Visible edit"
	next.Labels = append(next.Labels, db.ScopedLabelState{ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Label: "ready"})
	effects, err := db.DiffScopedState(want, next, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 2 {
		t.Fatal("expected atomic issue/label effects", effects)
	}
	for _, e := range effects {
		if e.Revision != 7 || e.Validate() != nil {
			t.Fatal("invalid atomic effect", e)
		}
	}
	if _, err = db.DiffScopedState(want, next, 0); err == nil {
		t.Fatal("visible change accepted without positive position")
	}
	// The projector must own its output; consumers can modify it without changing capture.
	got.Issues[0].Title = "modified"
	if c.Fold.Issues[projectorUID(11)].Title != "Issue 1" {
		t.Fatal("projector mutated input")
	}
}

// Contract: references retain canonical direction, but never expose unavailable
// endpoints; parent/close/reply and definition slots have stable identities.
func TestScopedProjectReferenceInventory(t *testing.T) {
	c, d, _ := projectorFixture(t, 1, 2)
	a, b, h := projectorUID(11), projectorUID(12), projectorUID(13)
	c.Fold.Links[db.FoldLinkKey{FromUID: b, ToUID: a, Type: "blocks"}] = db.FoldElementState{Present: true}
	c.Fold.Links[db.FoldLinkKey{FromUID: a, ToUID: b, Type: "parent"}] = db.FoldElementState{Present: true}
	c.Fold.Comments[projectorUID(31)] = db.FoldComment{UID: projectorUID(31), IssueUID: a, Body: "reply body", ReplyToUID: projectorUID(32), ReplyKind: "reply", CreatedAt: "2026-01-01T00:00:00Z"}
	c.Fold.Comments[projectorUID(32)] = db.FoldComment{UID: projectorUID(32), IssueUID: b, Body: "target body", CreatedAt: "2026-01-01T00:00:00Z"}
	got := project(t, c, d)
	parent := reference(t, got, "parent", a)
	if parent.Peer == nil || parent.Peer.IssueUID != b {
		t.Fatal("parent direction lost", parent)
	}
	link := reference(t, got, "link", b)
	if link.Peer == nil || link.Peer.IssueUID != a || link.Relation != "blocks" {
		t.Fatal("incoming block reversed", link)
	}
	reply := reference(t, got, "reply", projectorUID(31))
	if reply.Peer == nil || reply.Peer.IssueUID != b || reply.PeerEntityUID != projectorUID(32) {
		t.Fatal("reply membership lost", reply)
	}
	comment := c.Fold.Comments[projectorUID(32)]
	comment.IssueUID = h
	c.Fold.Comments[comment.UID] = comment
	next := project(t, c, d)
	null := reference(t, next, "reply", projectorUID(31))
	if null.Slot != reply.Slot || null.Present || null.Peer != nil || null.PeerEntityUID != "" || null.Relation != "" {
		t.Fatal("reply withdrawal leaked peer", null)
	}
	// Unsupported structured relations must refuse, never silently disappear.
	c.Fold.Links[db.FoldLinkKey{FromUID: a, ToUID: b, Type: "future-machine-reference"}] = db.FoldElementState{Present: true}
	if _, err := db.ProjectScopedState(c, d); err == nil {
		t.Fatal("unknown structured relation accepted")
	}
}

func TestScopedProjectDefinitions(t *testing.T) {
	c, d, _ := projectorFixture(t, 1, 2)
	job := db.FoldCronJob{ProjectUID: projectorUID(1), CronJob: db.CronJob{CronDefinition: db.CronDefinition{UID: projectorUID(41), Name: "scheduled", Author: "author"}, Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "issue_deadline", IssueUID: projectorUID(12)}, Action: cron.Action{Kind: "notify", Message: "authored text"}, Issue: &cron.IssuePolicy{Kind: "existing", UID: projectorUID(12)}}}}
	c.Fold.CronJobs = map[string]db.FoldCronJob{job.UID: job}
	got := project(t, c, d)
	if len(got.Definitions) != 1 {
		t.Fatal("visible definition lost")
	}
	if got.Definitions[0].Job.Trigger.IssueUID != "" || got.Definitions[0].Job.Issue.UID != "" {
		t.Fatal("definition embedded raw references")
	}
	_, narrow, _ := projectorFixture(t, 1)
	direct := project(t, c, narrow)
	restricted, err := db.RestrictScopedState(got, []string{projectorUID(1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Definitions) != 0 || len(restricted.Definitions) != 0 {
		t.Fatal("unsafe mandatory reference did not withdraw definition")
	}
	job.Definition.Version = 2
	c.Fold.CronJobs[job.UID] = job
	if _, err := db.ProjectScopedState(c, d); err == nil {
		t.Fatal("unknown definition version accepted")
	}
}

func TestScopedProjectCloseEffectiveProof(t *testing.T) {
	c, d, _ := projectorFixture(t, 1, 2)
	id := projectorUID(11)
	issue := c.Fold.Issues[id]
	issue.Status = "closed"
	issue.StatusIntentUID = projectorUID(71)
	issue.StatusClock = db.FoldClock{HLCPhysicalMS: 7, EventUID: issue.StatusIntentUID}
	c.Fold.Issues[id] = issue
	proof := db.CanonicalCloseState{IssueUID: id, StatusIntentUID: issue.StatusIntentUID, StatusClock: issue.StatusClock, Value: &db.ScopedCloseValue{Kind: "superseded", PeerProjectUID: projectorUID(2), PeerIssueUID: projectorUID(12)}}
	c.Close = []db.CanonicalCloseState{proof}
	present := reference(t, project(t, c, d), "close", id)
	if !present.Present || present.Close == nil {
		t.Fatal("effective close lost", present)
	}
	proof.StatusIntentUID = projectorUID(70)
	c.Close[0] = proof
	absent := reference(t, project(t, c, d), "close", id)
	if absent.Present || absent.Close != nil || absent.Slot != present.Slot {
		t.Fatal("stale close proof revived", absent)
	}
}

// The oracle independently constructs and restricts typed canonical facts; it
// never calls the production projector, its reference predicate or digest.
func TestScopedProjectOraclePermutations(t *testing.T) {
	for seed := int64(0); seed < 32; seed++ {
		c, d, _ := projectorFixture(t, 1, 2)
		events := []db.FoldEvent{}
		for _, i := range c.Fold.SortedIssues() {
			events = append(events, db.FoldEvent{UID: projectorUID(100 + len(events)), ProjectUID: i.ProjectUID, IssueUID: i.UID, Type: "issue.created", Actor: "author", HLCPhysicalMS: 1, CreatedAt: i.CreatedAt, Payload: projectedBytes(t, map[string]any{"uid": i.UID, "title": i.Title, "body": i.Body, "status": i.Status, "created_at": i.CreatedAt})})
		}
		events = append(events,
			db.FoldEvent{UID: projectorUID(111), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.updated", HLCPhysicalMS: 8, Payload: []byte(`{"title":"winning edit"}`)},
			db.FoldEvent{UID: projectorUID(112), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.updated", HLCPhysicalMS: 3, Payload: []byte(`{"title":"late losing edit"}`)},
			db.FoldEvent{UID: projectorUID(113), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.linked", HLCPhysicalMS: 4, Payload: projectedBytes(t, map[string]any{"from_uid": projectorUID(11), "to_uid": projectorUID(12), "type": "blocks"})},
			db.FoldEvent{UID: projectorUID(114), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.linked", HLCPhysicalMS: 5, Payload: projectedBytes(t, map[string]any{"from_uid": projectorUID(11), "to_uid": projectorUID(13), "type": "parent"})},
			db.FoldEvent{UID: projectorUID(115), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.scheduled_reached", HLCPhysicalMS: 9, Payload: []byte(`{"audit":"HIDDEN-AUDIT-ORACLE-MARKER"}`)},
			db.FoldEvent{UID: projectorUID(116), ProjectUID: projectorUID(2), IssueUID: projectorUID(12), Type: "issue.commented", HLCPhysicalMS: 4, Payload: projectedBytes(t, map[string]any{"comment_uid": projectorUID(31), "body": "target", "author": "author", "created_at": "2026-01-01T00:00:00Z"})},
			db.FoldEvent{UID: projectorUID(117), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.commented", HLCPhysicalMS: 5, Payload: projectedBytes(t, map[string]any{"comment_uid": projectorUID(32), "body": "reply", "author": "author", "reply_to_uid": projectorUID(31), "reply_kind": "reply", "created_at": "2026-01-01T00:00:00Z"})},
			db.FoldEvent{UID: projectorUID(118), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.comment_edited", HLCPhysicalMS: 7, Payload: projectedBytes(t, map[string]any{"comment_uid": projectorUID(32), "body": "winning reply edit", "edited_at": "2026-01-01T00:00:07Z"})},
			db.FoldEvent{UID: projectorUID(119), ProjectUID: projectorUID(1), IssueUID: projectorUID(11), Type: "issue.comment_edited", HLCPhysicalMS: 6, Payload: projectedBytes(t, map[string]any{"comment_uid": projectorUID(32), "body": "late losing reply edit", "edited_at": "2026-01-01T00:00:06Z"})},
			db.FoldEvent{UID: projectorUID(120), ProjectUID: projectorUID(2), IssueUID: projectorUID(12), Type: "issue.linked", HLCPhysicalMS: 6, Payload: projectedBytes(t, map[string]any{"from_uid": projectorUID(12), "to_uid": projectorUID(11), "type": "blocks"})},
		)
		rand.New(rand.NewSource(seed)).Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
		c.Fold = db.FoldEvents(events)
		c.Fold.CronWorkflows = map[string]db.FoldCronWorkflow{projectorUID(42): {ProjectUID: projectorUID(2), CronWorkflow: db.CronWorkflow{CronDefinition: db.CronDefinition{UID: projectorUID(42), Name: "fixture-workflow", Author: "creator"}, Definition: cron.WorkflowDefinition{Version: 1}}}}
		c.Fold.CronJobs = map[string]db.FoldCronJob{}
		jobOrder := []int{41, 43}
		rand.New(rand.NewSource(seed)).Shuffle(len(jobOrder), func(i, j int) { jobOrder[i], jobOrder[j] = jobOrder[j], jobOrder[i] })
		for _, id := range jobOrder {
			target := projectorUID(12)
			if id == 43 {
				target = projectorUID(13)
			}
			c.Fold.CronJobs[projectorUID(id)] = db.FoldCronJob{ProjectUID: projectorUID(1), CronJob: db.CronJob{CronDefinition: db.CronDefinition{UID: projectorUID(id), Name: "fixture-job", Author: "creator"}, Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "issue_deadline", IssueUID: target}, Action: cron.Action{Kind: "execute", WorkflowUID: projectorUID(42)}}}}
		}
		got := project(t, c, d)
		want, err := dbtest.ScopedOracle(c, []string{projectorUID(1), projectorUID(2)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(projectedBytes(t, want), projectedBytes(t, got)) {
			t.Fatalf("seed %d oracle mismatch\nwant=%s\ngot=%s", seed, projectedBytes(t, want), projectedBytes(t, got))
		}
	}
}

func FuzzScopedProjectOracle(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{})
	f.Add([]byte{255, 255, 0})
	f.Fuzz(func(t *testing.T, actions []byte) {
		selected := []int{1}
		if len(actions) > 0 && actions[0]&1 != 0 {
			selected = append(selected, 2)
		}
		if len(actions) > 0 && actions[0]&2 != 0 {
			selected = append(selected, 3)
		}
		c, d, _ := projectorFixture(t, selected...)
		projectIDs := []string{}
		for _, pin := range d.Projects {
			projectIDs = append(projectIDs, pin.ProjectUID)
		}
		// Bound materialization work, while accepting the whole byte domain.
		if len(actions) > 64 {
			actions = actions[:64]
		}
		for n, action := range actions {
			a, b := projectorUID(int(action%3)+11), projectorUID(int((action/3)%3)+11)
			if a == b {
				continue
			}
			kind := []string{"blocks", "related"}[int(action/9)%2]
			if kind == "related" && a > b {
				a, b = b, a
			}
			c.Fold.Links[db.FoldLinkKey{FromUID: a, ToUID: b, Type: kind}] = db.FoldElementState{Present: action%2 == 0, Clock: db.FoldClock{HLCPhysicalMS: int64(n)}}
			i := c.Fold.Issues[a]
			i.UpdatedAt = fmt.Sprintf("hidden-clock-%d", n)
			c.Fold.Issues[a] = i
			switch action % 5 {
			case 0:
				id := projectorUID(300 + n)
				c.Fold.Comments[id] = db.FoldComment{UID: id, IssueUID: a, Body: fmt.Sprintf("authored-%x", action), ReplyToUID: projectorUID(299 + n), ReplyKind: "reply", CreatedAt: "2026-01-01T00:00:00Z"}
			case 1:
				i.Status = "closed"
				i.StatusIntentUID = projectorUID(500 + n)
				i.StatusClock = db.FoldClock{HLCPhysicalMS: int64(n), EventUID: i.StatusIntentUID}
				c.Fold.Issues[a] = i
				// Keep one effective proof per subject, as capture does.
				proofs := []db.CanonicalCloseState{}
				for _, proof := range c.Close {
					if proof.IssueUID != a {
						proofs = append(proofs, proof)
					}
				}
				c.Close = append(proofs, db.CanonicalCloseState{IssueUID: a, StatusIntentUID: i.StatusIntentUID, StatusClock: i.StatusClock, Value: &db.ScopedCloseValue{Kind: []string{"duplicate", "superseded"}[int(action/5)%2], PeerProjectUID: c.Fold.Issues[b].ProjectUID, PeerIssueUID: b}})
			case 2:
				if c.Fold.CronJobs == nil {
					c.Fold.CronJobs = map[string]db.FoldCronJob{}
				}
				id := projectorUID(600 + n)
				c.Fold.CronJobs[id] = db.FoldCronJob{ProjectUID: i.ProjectUID, CronJob: db.CronJob{CronDefinition: db.CronDefinition{UID: id, Name: "fixture", Author: "author"}, Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "issue_deadline", IssueUID: b}, Action: cron.Action{Kind: "notify", Message: "authored"}}}}
			case 3:
				c.Fold.Labels[db.FoldLabelKey{IssueUID: a, Label: fmt.Sprintf("label-%d", action)}] = db.FoldElementState{Present: action&1 == 0}
			}
		}
		got := project(t, c, d)
		want, err := dbtest.ScopedOracle(c, projectIDs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(projectedBytes(t, want), projectedBytes(t, got)) {
			t.Fatalf("oracle mismatch\nwant=%s\ngot=%s", projectedBytes(t, want), projectedBytes(t, got))
		}
		narrow, err := db.RestrictScopedState(got, []string{projectorUID(1)}, []string{projectorUID(11)})
		if err != nil {
			t.Fatal(err)
		}
		expected, err := dbtest.ScopedOracle(c, []string{projectorUID(1)}, []string{projectorUID(11)})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(projectedBytes(t, expected), projectedBytes(t, narrow)) {
			t.Fatalf("narrow oracle mismatch\nwant=%s\ngot=%s", projectedBytes(t, expected), projectedBytes(t, narrow))
		}
	})
}

func TestScopedProjectRetainedInputsUnchanged(t *testing.T) {
	for _, operation := range []string{"digest", "restrict", "diff"} {
		t.Run(operation, func(t *testing.T) {
			c, d, _ := projectorFixture(t, 1)
			owner := "owner"
			priority := int64(2)
			i := c.Fold.Issues[projectorUID(11)]
			i.Owner = &owner
			i.Priority = &priority
			c.Fold.Issues[i.UID] = i
			c.Fold.CronWorkflows = map[string]db.FoldCronWorkflow{projectorUID(45): {ProjectUID: projectorUID(1), CronWorkflow: db.CronWorkflow{CronDefinition: db.CronDefinition{UID: projectorUID(45)}, Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "step", Kind: "command", Command: "true"}}}}}}
			full := project(t, c, d)
			full.References[0].Revision = 91
			full.References[0].Signatures = []db.ScopedSignature{{ProjectUID: projectorUID(1), KeyID: "retained", Signature: "retained-signature"}}
			full.Provenance[0].Signatures = []db.ScopedSignature{{ProjectUID: projectorUID(1), KeyID: "retained", Signature: "retained-provenance"}}
			full.Projects[0].Metadata = db.JSONBlob(`{"z":1,"a":2}`)
			full.Issues[0].Metadata = db.JSONBlob(`{"z":3,"a":4}`)
			before := projectedBytes(t, full)
			switch operation {
			case "digest":
				if _, err := db.ScopedStateDigest(full); err != nil {
					t.Fatal(err)
				}
			case "restrict":
				v, err := db.RestrictScopedState(full, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				*v.Issues[0].Owner = "mutated owner"
				*v.Issues[0].Priority = 99
				v.Definitions[0].Workflow.Steps[0].Command = "mutated command"
			case "diff":
				next := project(t, c, d)
				if _, err := db.DiffScopedState(full, next, 6); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(before, projectedBytes(t, full)) {
				t.Fatalf("%s mutated retained input", operation)
			}
		})
	}
}

func TestScopedProjectExplicitEmptyAuthority(t *testing.T) {
	c, d, _ := projectorFixture(t, 1, 2)
	c.Fold.CronWorkflows = map[string]db.FoldCronWorkflow{projectorUID(45): {ProjectUID: projectorUID(1), CronWorkflow: db.CronWorkflow{CronDefinition: db.CronDefinition{UID: projectorUID(45)}, Definition: cron.WorkflowDefinition{Version: 1}}}}
	c.Creation = append(c.Creation, db.ScopedProvenance{ProjectUID: projectorUID(1), Kind: "cron_workflow", EntityUID: projectorUID(45)})
	full := project(t, c, d)
	for _, tc := range []struct {
		name                                      string
		projects, issues                          []string
		wantProjects, wantIssues, wantDefinitions int
	}{
		{"unrestricted", nil, nil, 2, 2, 1}, {"empty projects", []string{}, nil, 0, 0, 0}, {"empty issues", nil, []string{}, 2, 0, 0}, {"one project", []string{projectorUID(1)}, nil, 1, 1, 1}, {"issue restricted", []string{projectorUID(1)}, []string{projectorUID(11)}, 1, 1, 0}, {"cross intersection", []string{projectorUID(1)}, []string{projectorUID(12)}, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.RestrictScopedState(full, tc.projects, tc.issues)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Projects) != tc.wantProjects || len(got.Issues) != tc.wantIssues || len(got.Definitions) != tc.wantDefinitions {
				t.Fatalf("authority widened: projects=%d issues=%d definitions=%d", len(got.Projects), len(got.Issues), len(got.Definitions))
			}
			for _, p := range got.Provenance {
				if tc.issues != nil && strings.HasPrefix(p.Kind, "cron_") {
					t.Fatal("issue authority retained definition provenance")
				}
			}
		})
	}
}

// Project-qualified keys distinguish a membership move from a replacement in
// the old owner. A removal never needs the new project or withheld payload.
func TestScopedProjectEffectKeys(t *testing.T) {
	c, d, _ := projectorFixture(t, 1, 2)
	commentID := projectorUID(37)
	c.Fold.Comments[commentID] = db.FoldComment{UID: commentID, IssueUID: projectorUID(11), Body: "comment", CreatedAt: "2026-01-01T00:00:00Z"}
	before := project(t, c, d)
	i := c.Fold.Issues[projectorUID(11)]
	i.ProjectUID = projectorUID(2)
	c.Fold.Issues[i.UID] = i
	after := project(t, c, d)
	effects, err := db.DiffScopedState(before, after, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"issues/remove/" + projectorUID(1) + ":" + i.UID:        false,
		"issues/replace/" + projectorUID(2) + ":" + i.UID:       false,
		"comments/remove/" + projectorUID(1) + ":" + commentID:  false,
		"comments/replace/" + projectorUID(2) + ":" + commentID: false,
	}
	for _, e := range effects {
		key := e.Collection + "/" + e.Action + "/" + e.Key
		if _, ok := want[key]; ok {
			want[key] = true
		}
		if e.Action == "remove" && (e.Issue != nil || e.Comment != nil) {
			t.Fatal("withdrawal retained payload")
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("missing project-owned effect %s", key)
		}
	}
}

func TestScopedProjectCompleteInput(t *testing.T) {
	c, d, _ := projectorFixture(t, 1)
	full := project(t, c, d)
	incomplete := full
	incomplete.Artifacts = nil
	if _, err := db.ScopedStateDigest(incomplete); err == nil {
		t.Error("content digest silently completed missing collection")
	}
	if _, err := db.DiffScopedState(full, incomplete, 1); err == nil {
		t.Error("diff silently completed missing collection")
	}
	if _, err := db.RestrictScopedState(incomplete, nil, nil); err == nil {
		t.Error("restriction accepted missing collection")
	}
}

// Issue/comment UID ordering is independent of project order, including after
// a membership move. The oracle must implement the typed order independently.
func TestScopedProjectOracleInterleavedOwnership(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "interleaved"
		if moved {
			name = "moved owner"
		}
		t.Run(name, func(t *testing.T) {
			c, d, _ := projectorFixture(t, 1, 2)
			a, b := c.Fold.Issues[projectorUID(11)], c.Fold.Issues[projectorUID(12)]
			delete(c.Fold.Issues, projectorUID(11))
			delete(c.Fold.Issues, projectorUID(12))
			a.UID = projectorUID(20)
			b.UID = projectorUID(11)
			if moved {
				a.ProjectUID, b.ProjectUID = b.ProjectUID, a.ProjectUID
			}
			c.Fold.Issues[a.UID] = a
			c.Fold.Issues[b.UID] = b
			c.Creation = []db.ScopedProvenance{}
			c.Fold.Comments[projectorUID(21)] = db.FoldComment{UID: projectorUID(21), IssueUID: a.UID, Body: "first project comment", CreatedAt: "2026-01-01T00:00:00Z"}
			c.Fold.Comments[projectorUID(10)] = db.FoldComment{UID: projectorUID(10), IssueUID: b.UID, Body: "second project comment", CreatedAt: "2026-01-01T00:00:00Z"}
			got := project(t, c, d)
			want, err := dbtest.ScopedOracle(c, []string{projectorUID(1), projectorUID(2)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(projectedBytes(t, want), projectedBytes(t, got)) {
				t.Fatalf("oracle order differs across project membership\nwant=%s\ngot=%s", projectedBytes(t, want), projectedBytes(t, got))
			}
		})
	}
}

// Native jobs and workflows have separate UID namespaces. A workflow allowed
// in one project must not authorize a job in another project with the same UID.
// This fixture has no workflow dependency, so a missing dependency cannot mask
// disclosure by withdrawing the allowed workflow first.
func TestScopedProjectCronUIDCollisionRestriction(t *testing.T) {
	const hiddenJobName = "hidden-cron-job-name-marker"
	const hiddenJobBody = "hidden-cron-job-body-marker"
	const hiddenJobProvenance = "hidden-cron-job-provenance-marker"
	c, broadView, _ := projectorFixture(t, 1, 2)
	projectA, projectB, sharedUID := projectorUID(1), projectorUID(2), projectorUID(41)
	workflow := cron.WorkflowDefinition{
		Version: 1,
		Steps:   []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}},
	}
	job := cron.JobDefinition{
		Version: 1,
		Kind:    "job",
		Trigger: cron.Trigger{Kind: "manual"},
		Action:  cron.Action{Kind: "execute", Prompt: "Inspect fixture"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Fixture occurrence", Body: hiddenJobBody},
		Overlap: "forbid",
		Catchup: "skip",
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("workflow fixture violates the native document contract: %v", err)
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("job fixture violates the native document contract: %v", err)
	}
	c.Fold.CronWorkflows = map[string]db.FoldCronWorkflow{
		sharedUID: {
			ProjectUID: projectA,
			CronWorkflow: db.CronWorkflow{
				CronDefinition: db.CronDefinition{UID: sharedUID, Name: "visible-workflow", Author: "workflow-creator"},
				Definition:     workflow,
			},
		},
	}
	c.Fold.CronJobs = map[string]db.FoldCronJob{
		sharedUID: {
			ProjectUID: projectB,
			CronJob: db.CronJob{
				CronDefinition: db.CronDefinition{UID: sharedUID, Name: hiddenJobName, Author: hiddenJobProvenance},
				Definition:     job,
			},
		},
	}
	c.Creation = append(c.Creation,
		db.ScopedProvenance{ProjectUID: projectA, Kind: "cron_workflow", EntityUID: sharedUID, Creator: "workflow-creator", AttributionStatus: "legacy"},
		db.ScopedProvenance{ProjectUID: projectB, Kind: "cron_job", EntityUID: sharedUID, Creator: hiddenJobProvenance, AttributionStatus: "legacy"},
	)
	broad := project(t, c, broadView)
	if len(broad.Definitions) != 2 {
		t.Fatalf("valid broad fixture must retain both typed definitions: %+v", broad.Definitions)
	}
	want, err := dbtest.ScopedOracle(c, []string{projectA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(want.Definitions) != 1 || want.Definitions[0].ProjectUID != projectA || want.Definitions[0].Kind != "workflow" || want.Definitions[0].UID != sharedUID {
		t.Fatalf("canonical oracle must retain only the allowed workflow: %+v", want.Definitions)
	}
	got, err := db.RestrictScopedState(broad, []string{projectA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("restriction must retain complete valid collections: %v", err)
	}
	broadBytes, gotBytes := projectedBytes(t, broad), projectedBytes(t, got)
	for _, marker := range []string{hiddenJobName, hiddenJobBody, hiddenJobProvenance} {
		if !bytes.Contains(broadBytes, []byte(marker)) {
			t.Fatalf("broad fixture lost hidden job marker %q", marker)
		}
		if bytes.Contains(gotBytes, []byte(marker)) {
			t.Errorf("restriction disclosed hidden job marker %q through a colliding UID", marker)
		}
	}
	if !bytes.Equal(projectedBytes(t, want), gotBytes) {
		t.Errorf("restriction differs from independently restricted canonical state\nwant=%s\ngot=%s", projectedBytes(t, want), gotBytes)
	}
}

// The exported adapter must apply mandatory-target eligibility to externally
// supplied, structurally valid collections. ProjectScopedState does not produce
// this inconsistent input: it already withdraws jobs targeting deleted workflows.
func TestScopedProjectDeletedWorkflowAdapterEligibility(t *testing.T) {
	for _, crossProject := range []bool{false, true} {
		name := "same-project target"
		if crossProject {
			name = "cross-project target"
		}
		t.Run(name, func(t *testing.T) {
			c, broadView, _ := projectorFixture(t, 1, 2, 3)
			// Only the ordinary project/issue baseline comes from the projector.
			// Definition rows and their typed reference below are explicit adapter input.
			external := project(t, c, broadView)
			jobProject, workflowProject := projectorUID(1), projectorUID(1)
			if crossProject {
				workflowProject = projectorUID(2)
			}
			jobUID, workflowUID := projectorUID(41), projectorUID(42)
			createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			deletedAt := createdAt.Add(time.Hour)
			workflow := cron.WorkflowDefinition{
				Version: 1,
				Steps:   []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}},
			}
			job := cron.JobDefinition{
				Version: 1,
				Kind:    "job",
				Trigger: cron.Trigger{Kind: "manual"},
				Action:  cron.Action{Kind: "execute", WorkflowUID: workflowUID},
				Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Fixture occurrence", Body: "deleted-workflow-job-body-marker"},
				Overlap: "forbid",
				Catchup: "skip",
			}
			if err := workflow.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := job.Validate(); err != nil {
				t.Fatal(err)
			}
			c.Fold.CronWorkflows = map[string]db.FoldCronWorkflow{
				workflowUID: {ProjectUID: workflowProject, CronWorkflow: db.CronWorkflow{
					CronDefinition: db.CronDefinition{UID: workflowUID, Name: "archived-workflow", Author: "workflow-creator", CreatedAt: createdAt, DeletedAt: &deletedAt},
					Definition:     workflow,
				}},
			}
			c.Fold.CronJobs = map[string]db.FoldCronJob{
				jobUID: {ProjectUID: jobProject, CronJob: db.CronJob{
					CronDefinition: db.CronDefinition{UID: jobUID, Name: "dependent-job", Author: "job-creator", CreatedAt: createdAt},
					Definition:     job,
				}},
			}
			creation := []db.ScopedProvenance{
				{ProjectUID: workflowProject, Kind: "cron_workflow", EntityUID: workflowUID, Creator: "workflow-creator", AttributionStatus: "legacy"},
				{ProjectUID: jobProject, Kind: "cron_job", EntityUID: jobUID, Creator: "job-creator", AttributionStatus: "legacy"},
			}
			c.Creation = append(c.Creation, creation...)
			projectedJob := job
			projectedJob.Action.WorkflowUID = ""
			external.Definitions = append(external.Definitions,
				db.ScopedDefinitionState{UID: workflowUID, ProjectUID: workflowProject, Kind: "workflow", Name: "archived-workflow", Author: "workflow-creator", CreatedAt: createdAt, Deleted: true, Workflow: &workflow},
				db.ScopedDefinitionState{UID: jobUID, ProjectUID: jobProject, Kind: "job", Name: "dependent-job", Author: "job-creator", CreatedAt: createdAt, Job: &projectedJob},
			)
			external.References = append(external.References, db.ScopedReference{
				Kind: "definition", Slot: "action.workflow", Subject: db.ScopedEndpoint{ProjectUID: jobProject}, SubjectEntityUID: jobUID,
				Peer: &db.ScopedEndpoint{ProjectUID: workflowProject}, PeerEntityUID: workflowUID, Relation: "workflow", Present: true, Revision: 1,
			})
			external.Provenance = append(external.Provenance, creation...)
			if err := external.Validate(); err != nil {
				t.Fatalf("external adapter fixture must be structurally valid: %v", err)
			}
			selected := []string{projectorUID(1), projectorUID(2)}
			want, err := dbtest.ScopedOracle(c, selected, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, narrowView, _ := projectorFixture(t, 1, 2)
			if rebuilt := project(t, c, narrowView); !bytes.Equal(projectedBytes(t, want), projectedBytes(t, rebuilt)) {
				t.Fatal("canonical narrow rebuild differs from the independent oracle")
			}
			if len(want.Definitions) != 1 || want.Definitions[0].UID != workflowUID || !want.Definitions[0].Deleted {
				t.Fatalf("canonical expectation must retain the deleted workflow row only: %+v", want.Definitions)
			}
			got, err := db.RestrictScopedState(external, selected, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			retainedWorkflow, retainedProvenance := false, false
			for _, definition := range got.Definitions {
				if definition.Kind == "workflow" && definition.ProjectUID == workflowProject && definition.UID == workflowUID && definition.Deleted {
					retainedWorkflow = true
				}
				if definition.Kind == "job" && definition.ProjectUID == jobProject && definition.UID == jobUID {
					t.Error("adapter retained job whose mandatory workflow target is deleted")
				}
			}
			for _, provenance := range got.Provenance {
				if provenance.Kind == "cron_workflow" && provenance.ProjectUID == workflowProject && provenance.EntityUID == workflowUID {
					retainedProvenance = true
				}
				if provenance.Kind == "cron_job" && provenance.ProjectUID == jobProject && provenance.EntityUID == jobUID {
					t.Error("adapter retained provenance for an ineligible dependent job")
				}
			}
			if !retainedWorkflow || !retainedProvenance {
				t.Error("deleted workflow row and its creation provenance must remain visible")
			}
			if !bytes.Equal(projectedBytes(t, want), projectedBytes(t, got)) {
				t.Errorf("external adapter eligibility differs from canonical narrow rebuild/oracle\nwant=%s\ngot=%s", projectedBytes(t, want), projectedBytes(t, got))
			}
		})
	}
}

func TestScopedProjectCronSameUIDTypedEffects(t *testing.T) {
	for _, removedKind := range []string{"job", "workflow"} {
		t.Run("remove "+removedKind, func(t *testing.T) {
			c, view, keys := projectorFixture(t, 1)
			projectUID, sharedUID := projectorUID(1), projectorUID(41)
			workflow := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}
			job := cron.JobDefinition{
				Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"},
				Action:  cron.Action{Kind: "execute", Prompt: "Inspect fixture"},
				Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Fixture occurrence"},
				Overlap: "forbid", Catchup: "skip",
			}
			if err := workflow.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := job.Validate(); err != nil {
				t.Fatal(err)
			}
			c.Fold.CronWorkflows = map[string]db.FoldCronWorkflow{
				sharedUID: {ProjectUID: projectUID, CronWorkflow: db.CronWorkflow{
					CronDefinition: db.CronDefinition{UID: sharedUID, Name: "same-uid-workflow", Author: "workflow-creator"}, Definition: workflow,
				}},
			}
			c.Fold.CronJobs = map[string]db.FoldCronJob{
				sharedUID: {ProjectUID: projectUID, CronJob: db.CronJob{
					CronDefinition: db.CronDefinition{UID: sharedUID, Name: "same-uid-job", Author: "job-creator"}, Definition: job,
				}},
			}
			c.Creation = append(c.Creation,
				db.ScopedProvenance{ProjectUID: projectUID, Kind: "cron_workflow", EntityUID: sharedUID, Creator: "workflow-creator", AttributionStatus: "legacy"},
				db.ScopedProvenance{ProjectUID: projectUID, Kind: "cron_job", EntityUID: sharedUID, Creator: "job-creator", AttributionStatus: "legacy"},
			)
			before := project(t, c, view)
			if len(before.Definitions) != 2 {
				t.Fatal("same-project cross-kind UID collision dropped a definition", before.Definitions)
			}
			oracle, err := dbtest.ScopedOracle(c, []string{projectUID}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(projectedBytes(t, oracle), projectedBytes(t, before)) {
				t.Fatal("same-project typed definitions differ from independent canonical oracle")
			}
			restricted, err := db.RestrictScopedState(before, []string{projectUID}, nil)
			if err != nil || !bytes.Equal(projectedBytes(t, oracle), projectedBytes(t, restricted)) {
				t.Fatal("restriction merged same-UID job/workflow identities", err)
			}
			retainedKind := "job"
			if removedKind == "job" {
				retainedKind = "workflow"
				delete(c.Fold.CronJobs, sharedUID)
				updated := c.Fold.CronWorkflows[sharedUID]
				updated.Name = "updated-workflow"
				c.Fold.CronWorkflows[sharedUID] = updated
			} else {
				delete(c.Fold.CronWorkflows, sharedUID)
				updated := c.Fold.CronJobs[sharedUID]
				updated.Name = "updated-job"
				c.Fold.CronJobs[sharedUID] = updated
			}
			after := project(t, c, view)
			effects, err := db.DiffScopedState(before, after, 13)
			if err != nil {
				t.Fatal(err)
			}
			removed, replaced, definitionEffects := false, false, 0
			for _, effect := range effects {
				if effect.Collection != "definitions" {
					continue
				}
				definitionEffects++
				if effect.Action == "remove" && effect.Key == projectUID+":"+removedKind+":"+sharedUID && effect.Definition == nil {
					removed = true
				}
				if effect.Action == "replace" && effect.Key == projectUID+":"+retainedKind+":"+sharedUID && effect.Definition != nil && effect.Definition.Kind == retainedKind && effect.Definition.Name == "updated-"+retainedKind {
					replaced = true
				}
				if owner, err := db.ScopedEffectProjectUID(effect); err != nil || owner != projectUID {
					t.Fatal("typed definition effect disagrees with shared ownership parser", effect, err)
				}
			}
			if definitionEffects != 2 || !removed || !replaced {
				t.Fatalf("typed definition removal/replacement collapsed: %+v", effects)
			}
			batch, err := db.SignScopedBatch(db.ScopedBatch{
				View: view, Cut: db.ScopedCut{Sequence: 13, CaptureUID: projectorUID(96)}, PreviousDigest: strings.Repeat("c", 64), Effects: effects,
			}, keys)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.VerifyScopedBatch(batch, view, projectorPins(view)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Selected tombstones retain latest comment facts and creation attribution;
// either deleted owning issue withdraws the live reply edge in BOTH directions.
func TestScopedProjectOracleTombstoneComments(t *testing.T) {
	c, view, _ := projectorFixture(t, 1, 2)
	a, b, hidden := projectorUID(11), projectorUID(12), projectorUID(13)
	ca, cb, ch := projectorUID(31), projectorUID(32), projectorUID(33)
	const precise = "2026-01-01T00:00:00.123456789Z"
	deleted := "2026-01-02T00:00:00Z"
	issue := c.Fold.Issues[a]
	issue.DeletedAt = &deleted
	c.Fold.Issues[a] = issue
	c.Fold.Comments[ca] = db.FoldComment{UID: ca, IssueUID: a, Author: "original-creator", Body: "latest tombstone comment", CreatedAt: precise, EditedAt: deleted, ReplyToUID: cb, ReplyKind: "reply"}
	c.Fold.Comments[cb] = db.FoldComment{UID: cb, IssueUID: b, Author: "peer-creator", Body: "latest live comment", CreatedAt: precise, ReplyToUID: ca, ReplyKind: "reply"}
	c.Fold.Comments[ch] = db.FoldComment{UID: ch, IssueUID: hidden, Author: "hidden-creator", Body: "unselected comment", CreatedAt: precise}
	creation := db.ScopedProvenance{ProjectUID: projectorUID(1), Kind: "comment", EntityUID: ca, Creator: "original-creator", AttributionStatus: "legacy", Signatures: []db.ScopedSignature{}}
	c.Creation = append(c.Creation, creation)

	got := project(t, c, view)
	created := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	literal := []db.ScopedCommentState{
		{UID: ca, ProjectUID: projectorUID(1), IssueUID: a, Author: "original-creator", Body: "latest tombstone comment", CreatedAt: created},
		{UID: cb, ProjectUID: projectorUID(2), IssueUID: b, Author: "peer-creator", Body: "latest live comment", CreatedAt: created},
	}
	if !reflect.DeepEqual(literal, got.Comments) {
		t.Fatalf("selected latest comment facts/precision differ from literal native law: want=%+v got=%+v", literal, got.Comments)
	}
	for _, slot := range []db.ScopedReference{
		{Kind: "reply", Slot: "reply", Subject: db.ScopedEndpoint{ProjectUID: projectorUID(1), IssueUID: a}, SubjectEntityUID: ca, Revision: 1, Signatures: []db.ScopedSignature{}},
		{Kind: "reply", Slot: "reply", Subject: db.ScopedEndpoint{ProjectUID: projectorUID(2), IssueUID: b}, SubjectEntityUID: cb, Revision: 1, Signatures: []db.ScopedSignature{}},
	} {
		if actual := reference(t, got, "reply", slot.SubjectEntityUID); !reflect.DeepEqual(slot, actual) {
			t.Fatalf("deleted subject or peer must yield exact null reply slot: want=%+v got=%+v", slot, actual)
		}
	}
	found := false
	for _, actual := range got.Provenance {
		if actual.Kind == "comment" && actual.EntityUID == ca {
			if !reflect.DeepEqual(creation, actual) {
				t.Fatalf("selected tombstone comment creation changed: want=%+v got=%+v", creation, actual)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("selected tombstone comment lost original creation attribution")
	}

	// Compare the SAME complete project-selected domain. No issue restriction
	// or native acceptance/admission is inferred from this independent oracle.
	oracle, err := dbtest.ScopedOracle(c, []string{projectorUID(1), projectorUID(2)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectedBytes(t, got), projectedBytes(t, oracle)) {
		t.Fatalf("tombstone comment oracle disagrees with literal production law: production=%s oracle=%s", projectedBytes(t, got), projectedBytes(t, oracle))
	}
}
