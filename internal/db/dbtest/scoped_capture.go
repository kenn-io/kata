package dbtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// ScopedCaptureFixture starts without capture bytes under a real native grant.
// It shares the authority/pin/team setup with native admission tests; imported
// root context alone never substitutes for runtime credentials or authority.
type ScopedCaptureFixture struct {
	ScopedAuthorityFixture
	Grant db.CreatedFederationEnrollment
}

func NewScopedCaptureFixture(t *testing.T, s db.Storage) ScopedCaptureFixture {
	t.Helper()
	f := NewScopedAuthorityFixture(t, s)
	grant, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), f.Params)
	require.NoError(t, err)
	for _, r := range scopedAuthoritySnapshot(t, s) {
		if w, ok := r.(*db.ScopedWorkExport); ok {
			require.Nil(t, w.CaptureUID)
			require.Nil(t, w.CaptureBytes)
			require.Nil(t, w.CaptureDigest)
			require.Nil(t, w.CapturedRevision)
		}
	}
	return ScopedCaptureFixture{ScopedAuthorityFixture: f, Grant: grant}
}

// ScopedCurrentClose reads the current typed owner row, never event history.
func ScopedCurrentClose(t *testing.T, s db.Storage, issueUID string) db.CanonicalCloseState {
	t.Helper()
	var row *db.ScopedCanonicalCloseExport
	for _, r := range scopedAuthoritySnapshot(t, s) {
		if c, ok := r.(*db.ScopedCanonicalCloseExport); ok && c.IssueUID == issueUID {
			require.Nil(t, row, "one current native close row per issue")
			row = c
		}
	}
	require.NotNil(t, row, "committed native status mutation retains current proof")
	proof, err := db.DecodeScopedCloseClockProof(row.StatusClockBytes)
	require.NoError(t, err)
	out := db.CanonicalCloseState{IssueUID: row.IssueUID, StatusClock: proof.StatusClock, CloseBoundaryClock: proof.CloseBoundaryClock}
	if row.StatusIntentUID != nil {
		out.StatusIntentUID = *row.StatusIntentUID
	}
	if row.ValueBytes != nil {
		require.NoError(t, json.Unmarshal(row.ValueBytes, &out.Value))
	}
	return out
}

func RunScopedNativeCurrentClose(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	ctx := t.Context()
	issue, events, changed, err := s.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "member"})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, events)
	proof := ScopedCurrentClose(t, s, issue.UID)
	require.Equal(t, events[0].UID, proof.StatusIntentUID)
	require.Equal(t, db.FoldClock{EventUID: events[0].UID, OriginInstanceUID: events[0].OriginInstanceUID, HLCPhysicalMS: events[0].HLCPhysicalMS, HLCCounter: events[0].HLCCounter}, proof.StatusClock)
	require.Equal(t, &proof.StatusClock, proof.CloseBoundaryClock)
	require.Nil(t, proof.Value, "legacy peerless close does not invent a typed disposition")
	require.NoError(t, db.ValidateScopedCanonicalCloseState(proof, issue.UID, "closed"))
	_, err = s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "editor", Patch: map[string]jsontext.Value{"notify.example": jsontext.Value(`true`)}})
	require.NoError(t, err)
	require.Equal(t, proof, ScopedCurrentClose(t, s, issue.UID), "ordinary current metadata does not change status provenance")
	reopened, event, changed, err := s.ReopenIssue(ctx, issue.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, event)
	openProof := ScopedCurrentClose(t, s, issue.UID)
	require.Equal(t, event.UID, openProof.StatusIntentUID)
	require.Equal(t, &openProof.StatusClock, openProof.CloseBoundaryClock)
	require.Nil(t, openProof.Value)
	require.NoError(t, db.ValidateScopedCanonicalCloseState(openProof, reopened.UID, "open"))
	records := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(ctx, records, db.ImportOptions{}))
	require.Equal(t, openProof, ScopedCurrentClose(t, s, issue.UID), "full owner replay preserves exact current proof")
}

// ScopedCaptureSigner uses only the deterministic root keys owned by the
// retained test fixture. No live credentials or source receipts are fabricated.
func ScopedCaptureSigner(t *testing.T, f ScopedCaptureFixture, projectUID string) db.RootAttributionSigner {
	t.Helper()
	for n, p := range f.Params.View.Projects {
		if p.ProjectUID == projectUID {
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(n + 1)}, 32))
			require.Equal(t, p.Pin.PublicKey, []byte(key.Public().(ed25519.PublicKey)))
			return db.RootAttributionSigner{AuthorityUID: f.Params.View.AuthorityUID, PrivateKey: key}
		}
	}
	t.Fatalf("project is not in root fixture selection")
	return db.RootAttributionSigner{}
}

func RunScopedFirstNativeCapture(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	ctx := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[0].UID), "member")
	issue, created, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Title: "Original title", Body: "Original body", Author: "creation-label", Metadata: map[string]jsontext.Value{"example.flag": jsontext.Value(`true`)}})
	require.NoError(t, err)
	comment, _, err := s.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-creator", Teammate: "helper", Body: "Original comment"})
	require.NoError(t, err)
	edited, _, changed, err := s.EditComment(t.Context(), db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "editor", Body: "Current comment"})
	require.NoError(t, err)
	require.True(t, changed)
	_, _, err = s.AddLabelAndEvent(t.Context(), issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "current-label", Actor: "editor"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: f.Projects[0].UID, IssueUID: issue.UID, ProducerInstanceUID: s.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	stale, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := s.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), stale)
	require.NoError(t, err)
	require.True(t, durable)
	title, body := "Current title", "Current body"
	_, err = s.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "latest-editor", Title: &title, Body: &body})
	require.NoError(t, err)
	current, err := embedding.NewArtifact(identity, embedding.EmbedText(title, body), [][]float32{{0, 1}})
	require.NoError(t, err)
	durable, err = s.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), current)
	require.NoError(t, err)
	require.True(t, durable)
	workflow, _, err := s.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: f.Projects[0].ID, Name: "Current workflow", Actor: "workflow-creator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
	require.NoError(t, err)
	job, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.Projects[1].ID, UID: workflow.UID, Name: "Current job", Actor: "job-creator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect example work"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example work"}, Overlap: "forbid", Catchup: "skip"}})
	require.NoError(t, err)
	capturer, ok := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	require.True(t, ok, "native CaptureScopedView API is required")
	before, err := s.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	got, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	require.NotEmpty(t, got.CaptureUID)
	require.Len(t, got.Projects, 2)
	require.Len(t, got.ProjectCuts, 2)
	for _, p := range got.Projects {
		require.Zero(t, p.ID)
		require.Contains(t, []string{f.Projects[0].UID, f.Projects[1].UID}, p.UID)
	}
	expectedCuts := map[string]int64{f.Projects[0].UID: 0, f.Projects[1].UID: 0}
	for _, event := range before {
		if _, selected := expectedCuts[event.ProjectUID]; selected && event.ID > expectedCuts[event.ProjectUID] {
			expectedCuts[event.ProjectUID] = event.ID
		}
	}
	require.Equal(t, expectedCuts, got.ProjectCuts, "content and selected cuts come from the same native transaction")
	literal := got.Fold.Issues[issue.UID]
	require.Equal(t, title, literal.Title)
	require.Equal(t, body, literal.Body)
	require.Equal(t, "creation-label", literal.Author)
	require.JSONEq(t, `{"example.flag":true}`, string(got.Fold.IssueMetadata[issue.UID]))
	require.True(t, got.Fold.Labels[db.FoldLabelKey{IssueUID: issue.UID, Label: "current-label"}].Present)
	require.Equal(t, edited.Body, got.Fold.Comments[comment.UID].Body)
	capturedCreatedAt, err := time.Parse(time.RFC3339Nano, got.Fold.Comments[comment.UID].CreatedAt)
	require.NoError(t, err)
	require.True(t, comment.CreatedAt.Equal(capturedCreatedAt), "capture must preserve the actual native comment creation timestamp: native=%s captured=%s", comment.CreatedAt.Format(time.RFC3339Nano), got.Fold.Comments[comment.UID].CreatedAt)
	require.Equal(t, "comment-creator", got.Fold.Comments[comment.UID].Author)
	require.Equal(t, "helper", got.Fold.Comments[comment.UID].Teammate)
	require.Equal(t, workflow.Name, got.Fold.CronWorkflows[workflow.UID].Name)
	require.Equal(t, f.Projects[0].UID, got.Fold.CronWorkflows[workflow.UID].ProjectUID)
	require.Equal(t, job.Name, got.Fold.CronJobs[job.UID].Name)
	require.Equal(t, f.Projects[1].UID, got.Fold.CronJobs[job.UID].ProjectUID)
	require.Equal(t, []embedding.ArtifactManifest{current.Manifest()}, got.Artifacts, "stale input bytes cannot become a current artifact")
	creation, err := s.EntityAttribution(t.Context(), f.Projects[0].UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, created.UID, creation.EventUID, "creator receipt remains original after content edit")
	foundCreation := false
	for _, p := range got.Creation {
		if p.Kind == "issue" && p.EntityUID == issue.UID {
			foundCreation = true
			require.Equal(t, "verified", p.AttributionStatus)
			require.NotEqual(t, "latest-editor", p.Creator)
		}
	}
	require.True(t, foundCreation)
	after, err := s.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	require.Equal(t, before, after, "capture creates no synthetic original or source cursor")
	again, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	require.Equal(t, got, again)
	records := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(records))
	for _, r := range records {
		if work, ok := r.(*db.ScopedWorkExport); ok && work.ViewUID == f.Params.View.Key.ViewUID {
			require.Equal(t, &got.CaptureUID, work.CaptureUID)
			require.Equal(t, db.ScopedOfferedDigest(work.CaptureBytes), *work.CaptureDigest)
			decoded, err := db.DecodeScopedCanonicalCapture(work.CaptureBytes)
			require.NoError(t, err)
			require.Equal(t, got, decoded)
		}
	}
}

func RunScopedNativeTypedClose(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Typed disposition peer"})
	require.NoError(t, err)
	value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.Projects[1].UID, PeerIssueUID: peer.UID}
	issue, events, changed, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "duplicate", Actor: "member", CloseValue: value})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, events)
	var payload struct {
		Value *db.ScopedCloseValue `json:"close_value"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Equal(t, value, payload.Value, "new native typed input must survive in the exact original source")
	proof := ScopedCurrentClose(t, s, issue.UID)
	require.Equal(t, value, proof.Value)
	require.NoError(t, db.ValidateScopedCanonicalCloseState(proof, issue.UID, "closed"))
	for _, wrong := range []db.ScopedCloseValue{
		{Kind: "superseded", PeerProjectUID: f.Projects[1].UID, PeerIssueUID: peer.UID},
		{Kind: "duplicate", PeerProjectUID: f.Projects[0].UID, PeerIssueUID: peer.UID},
		{Kind: "duplicate", PeerProjectUID: f.Projects[0].UID, PeerIssueUID: f.Issue.UID},
	} {
		_, _, _, err = s.ReopenIssue(t.Context(), issue.ID, "member")
		require.NoError(t, err)
		before := scopedAuthoritySnapshot(t, s)
		_, _, _, err = s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: issue.ID, Reason: "duplicate", Actor: "member", CloseValue: &wrong})
		require.Error(t, err)
		require.Equal(t, before, scopedAuthoritySnapshot(t, s), "native peer/self/reason mismatch rolls back whole mutation")
	}
}

func RunScopedNativeImportStatusBoundary(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	stamp := time.Now().UTC().Add(-time.Hour)
	batch := db.ImportBatchParams{ProjectID: f.Projects[0].ID, Source: "example:source", Actor: "import-worker", Items: []db.ImportItem{{ExternalID: "example-item", Title: "Imported issue", Author: "creation-label", Status: "open", CreatedAt: stamp, UpdatedAt: stamp}}}
	result, _, err := s.ImportBatch(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Created)
	mapping, err := s.ImportMappingBySource(t.Context(), f.Projects[0].ID, batch.Source, "issue", "example-item")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	issue, events, changed, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: *mapping.IssueID, Reason: "done", Actor: "member"})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, events[0].UID, ScopedCurrentClose(t, s, issue.UID).StatusIntentUID)
	batch.Items[0].UpdatedAt = issue.UpdatedAt.Add(time.Minute)
	result, events, err = s.ImportBatch(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.NotEmpty(t, events)
	opened := ScopedCurrentClose(t, s, issue.UID)
	require.Empty(t, opened.StatusIntentUID, "nonexplicit closed-to-open import invalidates previous explicit intent using BEFORE status")
	require.Equal(t, events[0].UID, opened.StatusClock.EventUID)
	require.Equal(t, &opened.StatusClock, opened.CloseBoundaryClock)
	batch.Items[0].Status = "closed"
	batch.Items[0].ClosedReason = new("done")
	batch.Items[0].ClosedAt = new(stamp.Add(2 * time.Minute))
	batch.Items[0].UpdatedAt = issue.UpdatedAt.Add(2 * time.Minute)
	result, events, err = s.ImportBatch(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.NotEmpty(t, events)
	closed := ScopedCurrentClose(t, s, issue.UID)
	require.Empty(t, closed.StatusIntentUID, "nonexplicit open-to-closed import does not acquire explicit intent")
	require.Equal(t, events[0].UID, closed.StatusClock.EventUID)
	require.Equal(t, opened.CloseBoundaryClock, closed.CloseBoundaryClock)
	require.Nil(t, closed.Value)
	require.NoError(t, db.ValidateScopedCanonicalCloseState(closed, issue.UID, "closed"))
}

// ScopedCaptureWork reads the exact durable private work tuple from a complete
// owner snapshot, including the assembled bytes that must remain immutable.
func ScopedCaptureWork(t *testing.T, s db.Storage, viewUID string) db.ScopedWorkExport {
	t.Helper()
	for _, r := range scopedAuthoritySnapshot(t, s) {
		if work, ok := r.(*db.ScopedWorkExport); ok && work.ViewUID == viewUID {
			return *work
		}
	}
	t.Fatal("missing native capture work")
	return db.ScopedWorkExport{}
}

func RunScopedCaptureLocalDirty(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	capturer := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	first, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	_, err = s.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"notification.example": jsontext.Value(`true`)}})
	require.NoError(t, err)
	after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Greater(t, after.DirtyRevision, before.DirtyRevision, "ordinary current notification metadata must dirty canonical work in the native transaction")
	require.Equal(t, before.CapturedRevision, after.CapturedRevision)
	require.Equal(t, before.CaptureUID, after.CaptureUID)
	require.Equal(t, before.CaptureBytes, after.CaptureBytes, "dirty source never overwrites an owed cut")
	require.Equal(t, before.CaptureDigest, after.CaptureDigest)
	require.Equal(t, "captured", after.State)
	again, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	require.Equal(t, first, again, "new dirty work cannot replace the immutable in-flight representation")
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

// Completion is a private work CAS only. The later publisher owns publication
// and obligation settlement; this helper neither simulates nor releases them.
func RunScopedCaptureCompletion(t *testing.T, s db.Storage, complete func(context.Context, string, string, int64) (bool, error)) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	capturer := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	first, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	for _, stale := range []struct {
		uid      string
		revision int64
	}{{f.Issue.UID, *before.CapturedRevision}, {first.CaptureUID, *before.CapturedRevision + 1}} {
		done, err := complete(t.Context(), f.Params.View.Key.ViewUID, stale.uid, stale.revision)
		require.NoError(t, err)
		require.False(t, done)
		require.Equal(t, before, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
	}
	_, err = s.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"example.next": jsontext.Value(`true`)}})
	require.NoError(t, err)
	dirty := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	done, err := complete(t.Context(), f.Params.View.Key.ViewUID, first.CaptureUID, *before.CapturedRevision)
	require.NoError(t, err)
	require.True(t, done)
	after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Equal(t, dirty.DirtyRevision, after.DirtyRevision, "completion preserves every newer dirty revision")
	require.Equal(t, "dirty", after.State)
	require.Nil(t, after.CaptureUID)
	require.Nil(t, after.CaptureBytes)
	require.Nil(t, after.CaptureDigest)
	require.Nil(t, after.CapturedRevision)
	second, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	require.NotEqual(t, first.CaptureUID, second.CaptureUID)
	require.JSONEq(t, `{"example.next":true}`, string(second.Fold.IssueMetadata[f.Issue.UID]))
	current := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	done, err = complete(t.Context(), f.Params.View.Key.ViewUID, first.CaptureUID, *before.CapturedRevision)
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, current, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
	done, err = complete(t.Context(), f.Params.View.Key.ViewUID, second.CaptureUID, *current.CapturedRevision)
	require.NoError(t, err)
	require.True(t, done)
	after = ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Equal(t, "idle", after.State)
	require.Equal(t, current.DirtyRevision, after.DirtyRevision)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCapturePinnedCompletion(t *testing.T, s db.Storage, complete func(context.Context, string, string, int64) (bool, error)) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	capture, err := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	records := scopedAuthoritySnapshot(t, s)
	work := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	request := db.ScopedConversionRequest{UID: scopedOwnerUID(710), Digest: strings.Repeat("e", 64), CatalogEntryUID: f.Params.CatalogEntryUID, RouteDigest: f.Params.View.RouteDigest, Target: f.Params.View, SuspendBindingUIDs: []string{}}
	for _, p := range f.Params.View.Projects {
		request.ProjectUIDs = append(request.ProjectUIDs, p.ProjectUID)
	}
	principal := db.CustodyPrincipal{AuthorityUID: f.Params.View.AuthorityUID, BindingUID: f.Params.View.Route[0].BindingUID, Epoch: f.Params.View.Route[0].Epoch, AccountableActor: f.Parent.Actor, RouteDigest: f.Params.View.RouteDigest}
	principalKey, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)
	records = append(records, &db.ScopedConversionExport{ConversionUID: request.UID, RequestDigest: request.Digest, RequestBytes: scopedOwnerBytes(t, request), OldBindingsBytes: []byte(`[]`), Phase: "prepare"}, &db.ScopedConversionObligationExport{ConversionUID: request.UID, ObligationUID: scopedOwnerUID(711), Kind: "capture", BindingUID: principal.BindingUID, ObjectUID: capture.CaptureUID, Digest: *work.CaptureDigest, Epoch: principal.Epoch, PrincipalKey: principalKey, State: "pending"})
	require.NoError(t, db.ValidateImportRecords(records), "real captured representation is retained by a complete typed conversion graph")
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	done, err := complete(t.Context(), f.Params.View.Key.ViewUID, capture.CaptureUID, *work.CapturedRevision)
	require.NoError(t, err)
	require.False(t, done, "owed conversion pins exact capture until settlement")
	require.Equal(t, work, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCaptureIndexedHopQuota(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedAuthorityFixture(t, s)
	next := db.ScopedHopBinding{BindingUID: scopedOwnerUID(720), InstanceUID: scopedOwnerUID(721), Epoch: 1}
	f.Params.View.Route = append(f.Params.View.Route, next)
	f.Params.View.DestinationUID = next.InstanceUID
	_, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), f.Params)
	require.NoError(t, err)
	records := scopedAuthoritySnapshot(t, s)
	var work *db.ScopedWorkExport
	for _, r := range records {
		switch row := r.(type) {
		case *db.ScopedViewExport:
			row.BindingUID = next.BindingUID
			row.Epoch = next.Epoch
			row.AccountableActor = "other-hop-member"
		case *db.ScopedWorkExport:
			work = row
		}
	}
	require.NotNil(t, work)
	principal := db.CustodyPrincipal{AuthorityUID: f.Params.View.AuthorityUID, BindingUID: next.BindingUID, Epoch: 1, AccountableActor: "other-hop-member", RouteDigest: f.Params.View.RouteDigest}
	request := db.ScopedControlRequest{RequestUID: scopedOwnerUID(722), Digest: strings.Repeat("d", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: f.Projects[0].UID, IssueUID: f.Issue.UID}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: s.InstanceUID(), Holder: "member", ClientKind: "cli"}}
	control := &db.ScopedControlCustodyExport{RequestUID: request.RequestUID, RequestDigest: request.Digest, BindingUID: principal.BindingUID, PrincipalBytes: scopedOwnerBytes(t, principal), RequestBytes: scopedOwnerBytes(t, request), State: "pending"}
	padding := db.MaxScopedRecoveryBytes - db.ScopedRetainedBytes(work) - db.ScopedRetainedBytes(control)
	require.Positive(t, padding)
	control.RequestBytes = append(control.RequestBytes, bytes.Repeat([]byte{' '}, int(padding))...)
	records = append(records, control)
	require.NoError(t, db.ValidateImportRecords(records), "indexed other-hop context and inherited bytes are an allowed complete owner graph")
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	_, err = s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.Error(t, err, "new capture must share private budget with the retained indexed hop, not Route[0]")
	require.Equal(t, before, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "capacity refusal is atomic")
}

func RunScopedCaptureConsistentRetry(t *testing.T, s db.Storage, openWriter func() db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[0].ID, Author: "member", Title: "Race peer"})
	require.NoError(t, err)
	writer := openWriter()
	ready, changed := make(chan struct{}), make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	writerCtx := ctx // Shares cancellation/deadline, never the capture fence.
	var attempts atomic.Int32
	ctx = db.WithAdditionalTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
		if attempts.Add(1) != 1 {
			return nil
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
			return err
		}
		close(ready)
		select {
		case err := <-changed:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			changed <- ctx.Err()
			return
		}
		_, err := writer.PatchIssueMetadata(writerCtx, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"race.example": jsontext.Value(`true`)}})
		if err == nil {
			_, _, err = writer.CreateComment(writerCtx, db.CreateCommentParams{IssueID: f.Issue.ID, Author: "member", Body: "After snapshot"})
		}
		if err == nil {
			_, err = writer.CreateLink(writerCtx, db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: peer.ID, Type: "related", Author: "member"})
		}
		changed <- err
	}()
	got, err := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(ctx, f.Params.View.Key)
	require.NoError(t, err)
	require.GreaterOrEqual(t, attempts.Load(), int32(2), "native snapshot conflict retries the entire attempt")
	require.JSONEq(t, `{"race.example":true}`, string(got.Fold.IssueMetadata[f.Issue.UID]))
	found := false
	for _, c := range got.Fold.Comments {
		if c.IssueUID == f.Issue.UID && c.Body == "After snapshot" {
			found = true
		}
	}
	require.True(t, found)
	linkFound := false
	for k, v := range got.Fold.Links {
		if k.Type == "related" && v.Present && (k.FromUID == f.Issue.UID && k.ToUID == peer.UID || k.ToUID == f.Issue.UID && k.FromUID == peer.UID) {
			linkFound = true
		}
	}
	require.True(t, linkFound)
	events, err := s.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	cuts := map[string]int64{f.Projects[0].UID: 0, f.Projects[1].UID: 0}
	for _, e := range events {
		if _, ok := cuts[e.ProjectUID]; ok && e.ID > cuts[e.ProjectUID] {
			cuts[e.ProjectUID] = e.ID
		}
	}
	require.Equal(t, cuts, got.ProjectCuts, "all three current changes and their cuts belong to the retried native cut")
}

func RunScopedCaptureGenerationAssociation(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedAuthorityFixture(t, s)
	expanded := f.Params.View
	expanded.Key.Generation = 2
	expanded.RequestDigest = strings.Repeat("b", 64)
	f.Params.View.Projects = f.Params.View.Projects[:1]
	_, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), f.Params)
	require.NoError(t, err)
	capturer := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	first, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	records := scopedAuthoritySnapshot(t, s)
	// This is exact retained typed owner context, not a runtime activation call.
	checkpoint := db.ScopedCheckpoint{View: f.Params.View, Cut: db.ScopedCut{Sequence: 1, CaptureUID: first.CaptureUID}, Collections: db.ScopedCollections{Projects: []db.ScopedProjectState{}, Issues: []db.ScopedIssueState{}, Comments: []db.ScopedCommentState{}, Labels: []db.ScopedLabelState{}, Definitions: []db.ScopedDefinitionState{}, References: []db.ScopedReference{}, Provenance: []db.ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}}
	checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
	require.NoError(t, err)
	signing, err := db.ScopedCheckpointSigningBytes(checkpoint)
	require.NoError(t, err)
	signer := ScopedCaptureSigner(t, ScopedCaptureFixture{ScopedAuthorityFixture: f}, f.Projects[0].UID)
	pin := f.Params.View.Projects[0].Pin
	checkpoint.Signatures = []db.ScopedSignature{{ProjectUID: pin.ProjectUID, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signer.PrivateKey, signing))}}
	for _, r := range records {
		switch row := r.(type) {
		case *db.ScopedViewExport:
			row.ActiveGeneration = scopedOwnerPtr(int64(1))
			row.PreparedGeneration = scopedOwnerPtr(int64(2))
			row.State = "active"
			row.RequestDigest = expanded.RequestDigest
		case *db.ScopedGenerationExport:
			row.State = "active"
			row.CheckpointBytes = scopedOwnerBytes(t, checkpoint)
			row.CheckpointDigest = scopedOwnerPtr(checkpoint.Cut.Digest)
			row.CutSequence = 1
			row.CutDigest = checkpoint.Cut.Digest
			row.AssembledBytes = int64(len(row.CheckpointBytes))
		}
	}
	raw := scopedOwnerBytes(t, expanded)
	records = append(records, &db.ScopedGenerationExport{ViewUID: expanded.Key.ViewUID, Generation: 2, DescriptorBytes: raw, DescriptorDigest: db.ScopedOfferedDigest(raw), State: "prepared", RootCutoverUID: expanded.RootCutoverUID})
	require.NoError(t, db.ValidateImportRecords(records), "active old capture remains valid beside prepared expansion")
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	old, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	require.Equal(t, first, old)
	_, err = capturer.CaptureScopedView(t.Context(), expanded.Key)
	require.ErrorIs(t, err, db.ErrScopedCapturePending)
	require.Equal(t, before, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
	// A prepared descriptor with exactly the old selection may reuse the bytes.
	expanded.Projects = f.Params.View.Projects
	for _, r := range records {
		if gen, ok := r.(*db.ScopedGenerationExport); ok && gen.Generation == 2 {
			gen.DescriptorBytes = scopedOwnerBytes(t, expanded)
			gen.DescriptorDigest = db.ScopedOfferedDigest(gen.DescriptorBytes)
		}
	}
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	same, err := capturer.CaptureScopedView(t.Context(), expanded.Key)
	require.NoError(t, err)
	require.Equal(t, first, same)
	parent, err := s.ResolveAPIToken(t.Context(), "parent-test-token")
	require.NoError(t, err)
	_, _, err = s.RevokeAPIToken(t.Context(), parent.ID, "admin")
	require.NoError(t, err)
	_, err = capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.Error(t, err, "cached bytes do not bypass current native parent revocation")
	require.Equal(t, before, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
}

func RunScopedOrdinaryCloseCompatibility(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, merge := range []bool{false, true} {
		name := "new_instance"
		if merge {
			name = "project_copy"
		}
		t.Run(name, func(t *testing.T) {
			s := open(t)
			p, err := s.CreateProject(t.Context(), "ordinary-project")
			require.NoError(t, err)
			copyRecords := func() []db.ImportRecord {
				if !merge {
					return scopedAuthoritySnapshot(t, s)
				}
				filtered, err := CollectImportRecords(t.Context(), s, db.ExportFilter{ProjectID: &p.ID, IncludeDeleted: true})
				require.NoError(t, err)
				for _, r := range scopedAuthoritySnapshot(t, s) {
					if db.IsScopedImportRecord(r) {
						filtered = append(filtered, r)
					}
				}
				return filtered
			}
			baseline := copyRecords()
			require.NoError(t, open(t).ImportReplay(t.Context(), baseline, db.ImportOptions{NewInstance: !merge, MergeProject: merge}), "ordinary native transform is supported before status writes")
			issue, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Author: "member", Title: "Ordinary status"})
			require.NoError(t, err)
			_, _, _, err = s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: issue.ID, Actor: "member", Reason: "done"})
			require.NoError(t, err)
			records := copyRecords()
			require.NoError(t, db.ValidateImportRecords(records))
			require.NoError(t, open(t).ImportReplay(t.Context(), records, db.ImportOptions{NewInstance: !merge, MergeProject: merge}), "ordinary status writes cannot turn a normal backup into scoped owner custody")
		})
	}
	for _, operation := range []string{"purge_issue", "purge_project", "move", "merge"} {
		t.Run(operation, func(t *testing.T) {
			s := open(t)
			p, err := s.CreateProject(t.Context(), "ordinary-project")
			require.NoError(t, err)
			q, err := s.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			issue, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Author: "member", Title: "Ordinary lifetime"})
			require.NoError(t, err)
			issue, _, _, err = s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: issue.ID, Actor: "member", Reason: "done"})
			require.NoError(t, err)
			switch operation {
			case "purge_issue":
				_, err = s.PurgeIssue(t.Context(), issue.ID, "member", nil)
			case "purge_project":
				_, _, err = s.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: p.ID, Actor: "member", Force: true})
				require.NoError(t, err)
				_, err = s.PurgeProject(t.Context(), db.PurgeProjectParams{ProjectID: p.ID, Actor: "member"})
			case "move":
				_, err = s.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: p.ID, ToProjectID: q.ID, IfMatchRev: issue.Revision, Actor: "member"})
			case "merge":
				_, err = s.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: p.ID, TargetProjectID: q.ID, Actor: "member"})
			}
			require.NoError(t, err, "ordinary no-federation native operation passes its actual gates")
			records := scopedAuthoritySnapshot(t, s)
			require.NoError(t, db.ValidateImportRecords(records), "ordinary legal lifetime keeps a complete restorable owner graph")
			require.NoError(t, open(t).ImportReplay(t.Context(), records, db.ImportOptions{}))
		})
	}
}

func RunScopedCloseParticipation(t *testing.T, s db.Storage) {
	t.Helper()
	root := ScopedOwnerRootIdleFixture(t)
	var project db.Project
	for _, selected := range root.View.Projects {
		p, err := s.CreateProjectWithUID(t.Context(), "legacy-"+selected.ProjectUID[len(selected.ProjectUID)-4:], selected.ProjectUID)
		require.NoError(t, err)
		if project.UID == "" {
			project = p
		}
	}
	issue, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Legacy closed state"})
	require.NoError(t, err)
	issue, _, _, err = s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: issue.ID, Actor: "member", Reason: "done"})
	require.NoError(t, err)
	records := scopedAuthoritySnapshot(t, s)
	for _, r := range records {
		_, proof := r.(*db.ScopedCanonicalCloseExport)
		require.False(t, proof, "ordinary projects do not opt into private scoped proof storage")
	}
	for _, r := range records {
		if m, ok := r.(*db.MetaKV); ok && m.Key == "instance_uid" {
			m.Value = root.View.AuthorityUID
		}
	}
	for _, r := range root.Records {
		switch r.(type) {
		case *db.RootKeyPin, *db.ScopedProjectExport, *db.ScopedProtocolCutoverExport:
			records = append(records, r)
		}
	}
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	issue, err = s.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	for _, r := range scopedAuthoritySnapshot(t, s) {
		_, proof := r.(*db.ScopedCanonicalCloseExport)
		require.False(t, proof, "conversion context never infers the old close intent")
	}
	_, event, changed, err := s.ReopenIssue(t.Context(), issue.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, event.UID, ScopedCurrentClose(t, s, issue.UID).StatusIntentUID, "actual scoped root mutation advances without any leaf grant")
}

func RunScopedCloseAfterGrantLoss(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	_, _, _, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "done"})
	require.NoError(t, err)
	before := ScopedCurrentClose(t, s, f.Issue.UID)
	_, _, err = s.RevokeAPIToken(t.Context(), f.Parent.ID, "admin")
	require.NoError(t, err)
	_, event, changed, err := s.ReopenIssue(t.Context(), f.Issue.ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	after := ScopedCurrentClose(t, s, f.Issue.UID)
	require.NotEqual(t, before.StatusClock, after.StatusClock)
	require.Equal(t, event.UID, after.StatusIntentUID)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedObservedStatusBoundary(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	issue, events, changed, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "done"})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, events[0].UID, ScopedCurrentClose(t, s, issue.UID).StatusIntentUID)
	// Install the actual provider mapping after the old explicit close. No new
	// outbound pending event is invented or cleared to force an inward write.
	binding, err := s.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: f.Projects[0].ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example source", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	mapping, err := s.UpsertImportMapping(t.Context(), db.ImportMappingParams{ProjectID: f.Projects[0].ID, Source: binding.SourceKey, ExternalID: "example-item", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	at := issue.CreatedAt.Add(time.Hour)
	_, claimed, err := s.ClaimIssueSyncBinding(t.Context(), binding.ID, binding.Provider, at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	p := db.IssueStatusObservationParams{Guard: db.IssueSyncImportGuard{BindingID: binding.ID, Provider: binding.Provider, StartedAt: at}, MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: issue.UID, Status: "open", Observation: db.IssueStatusObservation{Raw: new("open"), Version: at}}
	writer := s.(db.IssueStatusWriter)
	changed, events, err = writer.ObserveIssueStatus(t.Context(), p)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 1)
	require.Equal(t, "issue.updated", events[0].Type)
	require.Equal(t, "notion-sync", events[0].Actor)
	proof := ScopedCurrentClose(t, s, issue.UID)
	require.Empty(t, proof.StatusIntentUID, "nonexplicit provider reopen invalidates the old explicit close intent")
	require.Equal(t, events[0].UID, proof.StatusClock.EventUID)
	require.Nil(t, proof.Value)
	p.Status = "closed"
	p.Observation = db.IssueStatusObservation{Raw: new("closed"), Version: at.Add(time.Minute)}
	changed, events, err = writer.ObserveIssueStatus(t.Context(), p)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 1)
	closed := ScopedCurrentClose(t, s, issue.UID)
	require.Empty(t, closed.StatusIntentUID)
	require.Equal(t, events[0].UID, closed.StatusClock.EventUID)
	require.Equal(t, proof.CloseBoundaryClock, closed.CloseBoundaryClock)
	require.Nil(t, closed.Value)
	current, err := s.(db.IssueStatusReader).IssueStatusMappingByID(t.Context(), p.Guard, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, current.State.PendingEventUID, "inward observation never creates provider pending echo")
	require.Equal(t, &p.Observation, current.State.Observed)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCloseLegalPurge(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, subject := range []bool{true, false} {
		name := "peer"
		if subject {
			name = "subject"
		}
		t.Run(name, func(t *testing.T) {
			s := open(t)
			f := NewScopedCaptureFixture(t, s)
			peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Current proof peer"})
			require.NoError(t, err)
			value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.Projects[1].UID, PeerIssueUID: peer.UID}
			_, events, _, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "duplicate", CloseValue: value})
			require.NoError(t, err)
			before := ScopedCurrentClose(t, s, f.Issue.UID)
			require.NoError(t, s.RevokeFederationEnrollment(t.Context(), f.Grant.Enrollment.ID), "actual revocation drains native relay pending guards")
			doomed := peer
			if subject {
				doomed = f.Issue
			}
			_, err = s.PurgeIssue(t.Context(), doomed.ID, "member", nil)
			require.NoError(t, err, "only exercise lifetime after actual native gates permit purge")
			records := scopedAuthoritySnapshot(t, s)
			require.NoError(t, db.ValidateImportRecords(records), "legal retained scoped root purge must not orphan a current proof subject or peer")
			if subject {
				for _, r := range records {
					if proof, ok := r.(*db.ScopedCanonicalCloseExport); ok {
						require.NotEqual(t, f.Issue.UID, proof.IssueUID)
					}
				}
			} else {
				after := ScopedCurrentClose(t, s, f.Issue.UID)
				require.Nil(t, after.Value, "purged peer invalidates only derived current disposition")
				before.Value = nil
				require.Equal(t, before, after, "writer, intent and invalidation boundary remain exact")
				current, err := s.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
				require.NoError(t, err)
				originalFound := false
				for _, e := range current {
					if e.UID == events[0].UID {
						originalFound = true
						require.Equal(t, events[0].Payload, e.Payload)
						require.Equal(t, events[0].ContentHash, e.ContentHash)
					}
				}
				require.True(t, originalFound)
			}
			require.NoError(t, open(t).ImportReplay(t.Context(), records, db.ImportOptions{}))
		})
	}
}

func RunScopedCloseCurrentSnapshot(t *testing.T, s db.Storage, snapshot func(context.Context, db.Issue) (string, error)) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Current close peer"})
	require.NoError(t, err)
	value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.Projects[1].UID, PeerIssueUID: peer.UID}
	issue, _, _, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "duplicate", CloseValue: value})
	require.NoError(t, err)
	for _, state := range []string{"closed", "open"} {
		if state == "open" {
			issue, _, _, err = s.ReopenIssue(t.Context(), issue.ID, "member")
			require.NoError(t, err)
		}
		proof := ScopedCurrentClose(t, s, issue.UID)
		raw, err := snapshot(t.Context(), issue)
		require.NoError(t, err)
		var payload struct {
			Close *db.CanonicalCloseState `json:"close_state"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &payload))
		require.NotNil(t, payload.Close, "current native snapshot must retain effective typed close proof independently of history")
		require.Equal(t, proof, *payload.Close)
	}
	ordinary, err := s.CreateProject(t.Context(), "ordinary-snapshot")
	require.NoError(t, err)
	legacy, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: ordinary.ID, Author: "member", Title: "Legacy state"})
	require.NoError(t, err)
	raw, err := snapshot(t.Context(), legacy)
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(raw), &fields))
	require.NotContains(t, fields, "close_state", "ordinary legacy snapshot gains no scoped proof carrier")
}

func scopedNativeFold(events []db.Event) db.FoldProjection {
	out := make([]db.FoldEvent, 0, len(events))
	for _, e := range events {
		f := db.FoldEvent{UID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: e.ProjectUID, Type: e.Type, Actor: e.Actor, Payload: []byte(e.Payload), HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, CreatedAt: e.CreatedAt.UTC().Format(db.EventTimestampFormat)}
		if e.IssueUID != nil {
			f.IssueUID = *e.IssueUID
		}
		if e.RelatedIssueUID != nil {
			f.RelatedIssueUID = *e.RelatedIssueUID
		}
		out = append(out, f)
	}
	return db.FoldEvents(out)
}

func RunScopedCloseSnapshotReplay(t *testing.T, s db.Storage, open func(*testing.T) db.Storage, compact func(context.Context, int64, int64) error) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Invalidated peer"})
	require.NoError(t, err)
	value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.Projects[1].UID, PeerIssueUID: peer.UID}
	_, original, _, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "duplicate", CloseValue: value})
	require.NoError(t, err)
	_, err = s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	owed := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.NoError(t, s.RevokeFederationEnrollment(t.Context(), f.Grant.Enrollment.ID))
	_, err = s.PurgeIssue(t.Context(), peer.ID, "member", nil)
	require.NoError(t, err)
	proof := ScopedCurrentClose(t, s, f.Issue.UID)
	require.Nil(t, proof.Value)
	require.Equal(t, original[0].UID, proof.StatusIntentUID)
	require.NoError(t, s.MaterializeFederatedProject(t.Context(), f.Projects[0].ID))
	require.Nil(t, ScopedCurrentClose(t, s, f.Issue.UID).Value, "immediate native materialization before any new snapshot cannot resurrect invalidated C1")
	baseline, changed, err := s.RefreshProjectFederationBaseline(t.Context(), f.Projects[0].ID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	events, err := s.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.Projects[0].ID, Limit: 1000})
	require.NoError(t, err)
	folded := scopedNativeFold(events)
	require.Nil(t, folded.Issues[f.Issue.UID].CloseValue, "actual current null snapshot must invalidate retained C1 value at the same intent/boundary")
	require.NoError(t, s.MaterializeFederatedProject(t.Context(), f.Projects[0].ID))
	require.Nil(t, ScopedCurrentClose(t, s, f.Issue.UID).Value)
	// Explicit isolated compaction seam: remove only this project's pre-baseline
	// native history after real native baseline creation. Owed capture bytes stay.
	require.NoError(t, compact(t.Context(), f.Projects[0].ID, baseline.ReplayHorizonEventID))
	remaining, err := s.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.Projects[0].ID, Limit: 1000})
	require.NoError(t, err)
	remainingUIDs := map[string]bool{}
	for _, e := range remaining {
		remainingUIDs[e.UID] = true
		require.GreaterOrEqual(t, e.ID, baseline.ReplayHorizonEventID)
	}
	require.NotContains(t, remainingUIDs, original[0].UID, "physical compaction actually removes original C1")
	removed := 0
	for _, e := range events {
		if e.ID < baseline.ReplayHorizonEventID {
			removed++
			require.NotContains(t, remainingUIDs, e.UID, "every chosen pre-baseline row is absent")
		}
	}
	require.Positive(t, removed, "the compaction seam must delete actual history")
	snapshotOnly := scopedNativeFold(remaining).Issues[f.Issue.UID]
	require.Equal(t, original[0].UID, snapshotOnly.StatusIntentUID, "snapshot alone carries original intent without current SQL proof")
	boundary := db.FoldClock{EventUID: original[0].UID, OriginInstanceUID: original[0].OriginInstanceUID, HLCPhysicalMS: original[0].HLCPhysicalMS, HLCCounter: original[0].HLCCounter}
	require.Equal(t, &boundary, snapshotOnly.CloseBoundaryClock)
	require.Nil(t, snapshotOnly.CloseValue, "snapshot-only native fold retains current invalidation without the old original or SQL proof")
	require.NoError(t, s.MaterializeFederatedProject(t.Context(), f.Projects[0].ID))
	after := ScopedCurrentClose(t, s, f.Issue.UID)
	require.Nil(t, after.Value)
	require.Equal(t, proof.StatusIntentUID, after.StatusIntentUID)
	require.Equal(t, proof.CloseBoundaryClock, after.CloseBoundaryClock)
	require.Equal(t, owed.CaptureBytes, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID).CaptureBytes)
	records := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(records))
	restored := open(t)
	require.NoError(t, restored.ImportReplay(t.Context(), records, db.ImportOptions{}))
	project, err := restored.ProjectByUID(t.Context(), f.Projects[0].UID)
	require.NoError(t, err)
	require.NoError(t, restored.MaterializeFederatedProject(t.Context(), project.ID))
	require.Equal(t, after, ScopedCurrentClose(t, restored, f.Issue.UID))
	issue, err := s.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, _, err = s.ReopenIssue(t.Context(), issue.ID, "member")
	require.NoError(t, err)
	nextPeer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Genuine new peer"})
	require.NoError(t, err)
	nextValue := &db.ScopedCloseValue{Kind: "superseded", PeerProjectUID: f.Projects[1].UID, PeerIssueUID: nextPeer.UID}
	_, next, _, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: issue.ID, Actor: "member", Reason: "superseded", CloseValue: nextValue})
	require.NoError(t, err)
	require.NoError(t, s.MaterializeFederatedProject(t.Context(), f.Projects[0].ID))
	current := ScopedCurrentClose(t, s, issue.UID)
	require.Equal(t, next[0].UID, current.StatusIntentUID)
	require.Equal(t, nextValue, current.Value, "genuine later C2 can replace sticky invalidated C1")
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func newScopedUnboundRootProjects(t *testing.T, s db.Storage) [2]db.Project {
	t.Helper()
	root := ScopedOwnerRootIdleFixture(t)
	var projects [2]db.Project
	for n, p := range root.View.Projects {
		var err error
		projects[n], err = s.CreateProjectWithUID(t.Context(), []string{"root-project", "peer-project"}[n], p.ProjectUID)
		require.NoError(t, err)
	}
	records := scopedAuthoritySnapshot(t, s)
	for _, r := range records {
		if m, ok := r.(*db.MetaKV); ok && m.Key == "instance_uid" {
			m.Value = root.View.AuthorityUID
		}
	}
	for _, r := range root.Records {
		switch r.(type) {
		case *db.RootKeyPin, *db.ScopedProjectExport, *db.ScopedProtocolCutoverExport:
			records = append(records, r)
		}
	}
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	for n, p := range projects {
		var err error
		projects[n], err = s.ProjectByUID(t.Context(), p.UID)
		require.NoError(t, err)
	}
	return projects
}

func RunScopedCloseLegalMove(t *testing.T, s db.Storage, open func(*testing.T) db.Storage) {
	t.Helper()
	projects := newScopedUnboundRootProjects(t, s)
	subject, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Author: "member", Title: "Retained subject"})
	require.NoError(t, err)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Author: "member", Title: "Moving peer"})
	require.NoError(t, err)
	value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: projects[0].UID, PeerIssueUID: peer.UID}
	subject, events, _, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: subject.ID, Actor: "member", Reason: "duplicate", CloseValue: value})
	require.NoError(t, err)
	original := ScopedCurrentClose(t, s, subject.UID)
	moved, err := s.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: peer.Revision, Actor: "member"})
	require.NoError(t, err, "real unbound retained root context passes native move gate without bypass")
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)), "legal move must rehome subject proof and invalidate derived peer membership")
	after := ScopedCurrentClose(t, s, subject.UID)
	require.Nil(t, after.Value)
	original.Value = nil
	require.Equal(t, original, after)
	_, err = s.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: projects[1].ID, ToProjectID: projects[0].ID, IfMatchRev: moved.Issue.Revision, Actor: "member"})
	require.NoError(t, err)
	require.Nil(t, ScopedCurrentClose(t, s, subject.UID).Value, "moving the same peer back never retargets or revives old evidence")
	// A real binding is installed only after legal unbound moves, preserving the
	// native prohibition on moves/merges once either project is federated.
	_, err = s.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: projects[0].ID, Role: db.FederationRoleHub, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, s.MaterializeFederatedProject(t.Context(), projects[0].ID))
	require.Nil(t, ScopedCurrentClose(t, s, subject.UID).Value)
	_, _, err = s.RefreshProjectFederationBaseline(t.Context(), projects[0].ID, "member")
	require.NoError(t, err)
	currentEvents, err := s.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: projects[0].ID, Limit: 1000})
	require.NoError(t, err)
	require.Nil(t, scopedNativeFold(currentEvents).Issues[subject.UID].CloseValue)
	for _, e := range currentEvents {
		if e.UID == events[0].UID {
			require.Equal(t, events[0].Payload, e.Payload)
			require.Equal(t, events[0].ContentHash, e.ContentHash)
		}
	}
	records := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, open(t).ImportReplay(t.Context(), records, db.ImportOptions{}))
	subject, err = s.IssueByUID(t.Context(), subject.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = s.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: subject.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: subject.Revision, Actor: "member"})
	require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
	_, err = s.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: projects[0].ID, TargetProjectID: projects[1].ID, Actor: "member"})
	require.ErrorIs(t, err, db.ErrProjectMergeFederationBinding)
	require.Equal(t, records, scopedAuthoritySnapshot(t, s))
}

func RunScopedCloseRemoteCurrent(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	history, err := s.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	var physical int64
	for _, e := range history {
		if e.HLCPhysicalMS > physical {
			physical = e.HLCPhysicalMS
		}
	}
	origin := scopedOwnerUID(950)
	next := func(n int, kind, body string) db.RemoteEvent {
		e := db.RemoteEvent{EventUID: scopedOwnerUID(951 + n), OriginInstanceUID: origin, ProjectUID: f.Projects[0].UID, ProjectName: f.Projects[0].Name, IssueUID: &f.Issue.UID, Type: kind, Actor: "member", HLCPhysicalMS: physical + int64(n) + 1000, CreatedAt: time.Now().UTC(), Payload: jsontext.Value(body)}
		e.ContentHash, err = db.RemoteEventContentHash(e)
		require.NoError(t, err)
		return e
	}
	ingest := func(n int, e db.RemoteEvent) db.FederationIngestResult {
		result, err := s.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: f.Projects[0].ID, SpokeInstanceUID: origin, BoundActor: "member", Events: []db.FederationIngestEvent{{SourceEventID: int64(n), Event: e}}})
		require.NoError(t, err)
		return result
	}
	closed := next(1, "issue.closed", `{"reason":"done"}`)
	require.Equal(t, 1, ingest(1, closed).Accepted)
	proof := ScopedCurrentClose(t, s, f.Issue.UID)
	closedRow, err := s.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "closed", closedRow.Status)
	require.Greater(t, closedRow.Revision, f.Issue.Revision)
	require.Equal(t, closed.EventUID, proof.StatusIntentUID, "actual accepted native materialization persists current close proof")
	require.Equal(t, closed.EventUID, proof.StatusClock.EventUID)
	// A newly accepted explicit same-state close with the same native row
	// values must still replace private proof before SQLite's row pruning.
	restatement := next(2, "issue.closed", `{"reason":"done"}`)
	restatement.CreatedAt = closed.CreatedAt
	restatement.ContentHash, err = db.RemoteEventContentHash(restatement)
	require.NoError(t, err)
	require.Equal(t, 1, ingest(2, restatement).Accepted)
	proof = ScopedCurrentClose(t, s, f.Issue.UID)
	restatedRow, err := s.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, closedRow.Revision, restatedRow.Revision, "same-value native SQL may prune the row while explicit proof changes")
	require.Equal(t, restatement.EventUID, proof.StatusIntentUID)
	require.Equal(t, restatement.EventUID, proof.StatusClock.EventUID, "same-value row pruning cannot discard new private proof clock")
	require.Equal(t, 1, ingest(2, restatement).Duplicates)
	require.Equal(t, proof, ScopedCurrentClose(t, s, f.Issue.UID))
	reopened := next(3, "issue.reopened", `{}`)
	require.Equal(t, 1, ingest(3, reopened).Accepted)
	restated := next(4, "issue.updated", `{"status":"closed"}`)
	require.Equal(t, 1, ingest(4, restated).Accepted)
	proof = ScopedCurrentClose(t, s, f.Issue.UID)
	require.Empty(t, proof.StatusIntentUID)
	require.Equal(t, restated.EventUID, proof.StatusClock.EventUID)
	require.Equal(t, reopened.EventUID, proof.CloseBoundaryClock.EventUID)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCaptureRootOnlyDirty(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, operation := range []string{"rename", "bound_author_rewrite_refusal"} {
		t.Run(operation, func(t *testing.T) {
			s := open(t)
			f := NewScopedCaptureFixture(t, s)
			_, err := s.(interface {
				CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
			}).CaptureScopedView(t.Context(), f.Params.View.Key)
			require.NoError(t, err)
			before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
			switch operation {
			case "rename":
				_, event, changed, err := s.RenameProjectAndEvent(t.Context(), f.Projects[0].ID, "renamed-project", "member")
				require.NoError(t, err)
				require.True(t, changed)
				require.NotNil(t, event)
			case "bound_author_rewrite_refusal":
				_, err := s.RewriteAuthorIdentity(t.Context(), db.RewriteAuthorIdentityParams{ProjectID: f.Projects[0].ID, Actor: "member", From: "member", To: "updated-label"})
				var federated *db.ProjectFederatedError
				require.ErrorAs(t, err, &federated)
				require.Equal(t, before, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "existing native binding refusal remains atomic")
				return
			}
			after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
			require.Greater(t, after.DirtyRevision, before.DirtyRevision, "root_only routing class still has current canonical effects")
			require.Equal(t, before.CaptureBytes, after.CaptureBytes)
			require.Equal(t, before.CaptureUID, after.CaptureUID)
		})
	}
}

func RunScopedCaptureLateArtifact(t *testing.T, s db.Storage, complete func(context.Context, string, string, int64) (bool, error)) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	capture := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	clean := func() {
		state, err := capture.CaptureScopedView(t.Context(), f.Params.View.Key)
		require.NoError(t, err)
		work := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
		done, err := complete(t.Context(), f.Params.View.Key.ViewUID, state.CaptureUID, *work.CapturedRevision)
		require.NoError(t, err)
		require.True(t, done)
	}
	clean()
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Equal(t, "idle", before.State)
	identity := embedding.ArtifactIdentity{ProjectUID: f.Projects[0].UID, IssueUID: scopedOwnerUID(990), ProducerInstanceUID: s.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("c", 64)}
	staged, err := embedding.NewArtifact(identity, embedding.EmbedText("Future issue", ""), [][]float32{{1, 0}})
	require.NoError(t, err)
	artifacts := s.(db.EmbeddingArtifactStorage)
	durable, err := artifacts.RetainEmbeddingArtifact(t.Context(), staged)
	require.NoError(t, err)
	require.False(t, durable)
	require.Equal(t, before, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "pre-content staging is not current public artifact state")
	identity.IssueUID = f.Issue.UID
	current, err := embedding.NewArtifact(identity, embedding.EmbedText(f.Issue.Title, f.Issue.Body), [][]float32{{0, 1}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(t.Context(), current)
	require.NoError(t, err)
	require.True(t, durable)
	after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Greater(t, after.DirtyRevision, before.DirtyRevision, "late eligible artifact arrival must wake clean canonical work without a source event")
	durable, err = artifacts.RetainEmbeddingArtifact(t.Context(), current)
	require.NoError(t, err)
	require.True(t, durable)
	require.Equal(t, after, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "identical durable retry has no new current effect")
	stale, err := embedding.NewArtifact(identity, embedding.EmbedText("Old input", ""), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(t.Context(), stale)
	require.NoError(t, err)
	require.True(t, durable)
	require.Equal(t, after, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "durable bytes for stale input are not eligible current artifact state")
	first, err := capture.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	require.Equal(t, []embedding.ArtifactManifest{current.Manifest()}, first.Artifacts)
	work := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	done, err := complete(t.Context(), f.Params.View.Key.ViewUID, first.CaptureUID, *work.CapturedRevision)
	require.NoError(t, err)
	require.True(t, done)
	_, _, err = s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[0].ID, UID: staged.IssueUID, Author: "member", Title: "Future issue"})
	require.NoError(t, err)
	clean()
	before = ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	durable, err = artifacts.RetainEmbeddingArtifact(t.Context(), staged)
	require.NoError(t, err)
	require.True(t, durable)
	after = ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Greater(t, after.DirtyRevision, before.DirtyRevision, "promotion after a clean content cut changes eligible current artifacts")
	durable, err = artifacts.RetainEmbeddingArtifact(t.Context(), staged)
	require.NoError(t, err)
	require.True(t, durable)
	require.Equal(t, after, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCaptureConcurrentViews(t *testing.T, s db.Storage, openWriter func() db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	second := f.Params
	second.View.Key.ViewUID = scopedOwnerUID(1010)
	second.CatalogEntryUID = scopedOwnerUID(1011)
	grant, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), second)
	require.NoError(t, err)
	require.Equal(t, f.Grant.Enrollment.ID, grant.Enrollment.ID)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Concurrent peer"})
	require.NoError(t, err)
	capture := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	for _, key := range []db.ScopedViewKey{f.Params.View.Key, second.View.Key} {
		_, err := capture.CaptureScopedView(t.Context(), key)
		require.NoError(t, err)
	}
	before := []db.ScopedWorkExport{ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), ScopedCaptureWork(t, s, second.View.Key.ViewUID)}
	writer := openWriter()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start, finished := make(chan struct{}), make(chan error, 2)
	for n, target := range []struct {
		store   db.Storage
		issueID int64
	}{{s, f.Issue.ID}, {writer, peer.ID}} {
		go func(n int) {
			<-start
			_, err := target.store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: target.issueID, Actor: "member", Patch: map[string]jsontext.Value{[]string{"first.example", "second.example"}[n]: jsontext.Value(`true`)}})
			finished <- err
		}(n)
	}
	close(start)
	require.NoError(t, <-finished)
	require.NoError(t, <-finished)
	for _, old := range before {
		now := ScopedCaptureWork(t, s, old.ViewUID)
		require.Equal(t, old.DirtyRevision+2, now.DirtyRevision)
		require.Equal(t, old.CaptureUID, now.CaptureUID)
		require.Equal(t, old.CaptureBytes, now.CaptureBytes)
	}
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCaptureEndpointDirty(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	second := f.Params
	second.View.Key.ViewUID = scopedOwnerUID(1020)
	second.CatalogEntryUID = scopedOwnerUID(1021)
	second.View.Projects = append([]db.ScopedProjectPin(nil), second.View.Projects[1:]...)
	second.View.Route = append([]db.ScopedHopBinding(nil), second.View.Route...)
	second.View.Route[0].BindingUID = scopedOwnerUID(1022)
	second.AnchorProjectID = f.Projects[1].ID
	second.Token = "peer-view-transport-test-token"
	_, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), second)
	require.NoError(t, err)
	peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Endpoint peer"})
	require.NoError(t, err)
	capture := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	_, err = capture.CaptureScopedView(t.Context(), second.View.Key)
	require.NoError(t, err)
	before := ScopedCaptureWork(t, s, second.View.Key.ViewUID)
	params := db.LinkEventParams{EventType: "issue.linked", EventIssueID: f.Issue.ID, FromShortID: f.Issue.ShortID, FromUID: f.Issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "member"}
	link, _, err := s.CreateLinkAndEvent(t.Context(), db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: peer.ID, Type: "related", Author: "member"}, params)
	require.NoError(t, err)
	after := ScopedCaptureWork(t, s, second.View.Key.ViewUID)
	require.Greater(t, after.DirtyRevision, before.DirtyRevision, "a view selecting only the OTHER endpoint must wake")
	require.Equal(t, before.CaptureBytes, after.CaptureBytes)
	params.EventType = "issue.unlinked"
	_, err = s.DeleteLinkAndEvent(t.Context(), link, params)
	require.NoError(t, err)
	require.Greater(t, ScopedCaptureWork(t, s, second.View.Key.ViewUID).DirtyRevision, after.DirtyRevision)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func RunScopedCaptureIngestDirty(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	_, err := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	history, err := s.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	var physical int64
	for _, e := range history {
		if e.HLCPhysicalMS > physical {
			physical = e.HLCPhysicalMS
		}
	}
	origin := scopedOwnerUID(1030)
	event := db.RemoteEvent{EventUID: scopedOwnerUID(1031), OriginInstanceUID: origin, ProjectUID: f.Projects[0].UID, ProjectName: f.Projects[0].Name, IssueUID: &f.Issue.UID, Type: "issue.updated", Actor: "member", HLCPhysicalMS: physical + 1000, CreatedAt: time.Now().UTC(), Payload: jsontext.Value(`{"title":"Accepted remote title"}`)}
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	input := db.FederationIngestParams{ProjectID: f.Projects[0].ID, SpokeInstanceUID: origin, BoundActor: "member", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
	result, err := s.IngestFederationEvents(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	issue, err := s.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Accepted remote title", issue.Title, "native accepted materialization control")
	after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Greater(t, after.DirtyRevision, before.DirtyRevision, "direct accepted materialization bypasses local event insertion")
	require.Equal(t, before.CaptureBytes, after.CaptureBytes)
	result, err = s.IngestFederationEvents(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, 1, result.Duplicates)
	require.Equal(t, after, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "duplicate retention is not another current mutation")
	require.NoError(t, s.MaterializeFederatedProject(t.Context(), f.Projects[0].ID))
	require.Equal(t, after, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "unchanged history rebuild is not fresh mutation evidence")
}

func RunScopedCaptureImportDirty(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	_, err := s.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	stamp := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	batch := db.ImportBatchParams{ProjectID: f.Projects[0].ID, Source: "example:source", Actor: "member", Items: []db.ImportItem{{ExternalID: "current-import", Title: "Imported current issue", Author: "creator", Status: "open", CreatedAt: stamp, UpdatedAt: stamp}}}
	result, _, err := s.ImportBatch(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Created)
	after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Greater(t, after.DirtyRevision, before.DirtyRevision)
	require.Equal(t, before.CaptureBytes, after.CaptureBytes)
	result, _, err = s.ImportBatch(t.Context(), batch)
	require.NoError(t, err)
	require.Zero(t, result.Updated)
	require.Equal(t, after, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "identical native import retry emits no current mutation")
	batch.Items[0].Title = "Updated imported current issue"
	batch.Items[0].UpdatedAt = stamp.Add(time.Minute)
	result, _, err = s.ImportBatch(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Greater(t, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID).DirtyRevision, after.DirtyRevision)
	records := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(records))
	work := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	require.Equal(t, work, ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "full owner replay retains exact imported work/capture rather than inventing data-merge work")
}

func RunScopedCaptureDirtyQuota(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, headroom := range []int64{0, 1} {
		t.Run([]string{"exact_limit_refuses_atomically", "one_byte_headroom_allows_dirty"}[headroom], func(t *testing.T) {
			s := open(t)
			f := NewScopedCaptureFixture(t, s)
			records := scopedAuthoritySnapshot(t, s)
			var work *db.ScopedWorkExport
			for _, r := range records {
				if w, ok := r.(*db.ScopedWorkExport); ok {
					work = w
					w.State = "idle"
				}
			}
			require.NotNil(t, work)
			principal := db.CustodyPrincipal{AuthorityUID: f.Params.View.AuthorityUID, BindingUID: f.Params.View.Route[0].BindingUID, Epoch: 1, AccountableActor: f.Parent.Actor, RouteDigest: f.Params.View.RouteDigest}
			request := db.ScopedControlRequest{RequestUID: scopedOwnerUID(1050), Digest: strings.Repeat("d", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: f.Projects[0].UID, IssueUID: f.Issue.UID}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: s.InstanceUID(), Holder: "member", ClientKind: "cli"}}
			control := &db.ScopedControlCustodyExport{RequestUID: request.RequestUID, RequestDigest: request.Digest, BindingUID: principal.BindingUID, PrincipalBytes: scopedOwnerBytes(t, principal), RequestBytes: scopedOwnerBytes(t, request), State: "pending"}
			padding := db.MaxScopedRecoveryBytes - headroom - db.ScopedRetainedBytes(work) - db.ScopedRetainedBytes(control)
			require.Positive(t, padding)
			control.RequestBytes = append(control.RequestBytes, bytes.Repeat([]byte{' '}, int(padding))...)
			require.EqualValues(t, db.MaxScopedRecoveryBytes-headroom, db.ScopedRetainedBytes(work)+db.ScopedRetainedBytes(control))
			records = append(records, control)
			require.NoError(t, db.ValidateImportRecords(records), "valid idle owner snapshot at exact private limit")
			require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
			issue, err := s.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			before := scopedAuthoritySnapshot(t, s)
			_, err = s.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"quota.example": jsontext.Value(`true`)}})
			if headroom == 0 {
				if err == nil {
					require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)), "a successful native mutation must leave a restorable owner graph")
				}
				require.Error(t, err, "idle->dirty costs one retained byte and must refuse at exact private limit")
				// PostgreSQL nextval allocation is intentionally nontransactional.
				// Compare every canonical/scoped/custody row, excluding only
				// allocator high-water metadata; no event row may survive.
				domainRows := func(records []db.ImportRecord) []db.ImportRecord {
					var rows []db.ImportRecord
					for _, record := range records {
						if _, allocator := record.(*db.SequenceExport); !allocator {
							rows = append(rows, record)
						}
					}
					return rows
				}
				after := scopedAuthoritySnapshot(t, s)
				require.Equal(t, domainRows(before), domainRows(after), "canonical row, source event, proof and all scoped work roll back together")
				require.NoError(t, db.ValidateImportRecords(after))
			} else {
				require.NoError(t, err)
				require.Equal(t, "dirty", ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID).State)
				require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
				// No further retained-byte growth: the next counter increment is fixed8.
				_, err = s.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"quota.example": jsontext.Value(`false`)}})
				require.NoError(t, err)
				require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
			}
		})
	}
}

func scopedV1ClosePayload(t *testing.T, snapshot bool, subject string, value db.ScopedCloseValue, origin string, physical int64) jsontext.Value {
	t.Helper()
	var payload any
	if snapshot {
		clock := db.FoldClock{HLCPhysicalMS: physical - 1, OriginInstanceUID: origin, EventUID: scopedOwnerUID(1090)}
		payload = struct {
			UID      string                 `json:"uid"`
			Title    string                 `json:"title"`
			Status   string                 `json:"status"`
			Author   string                 `json:"author"`
			Metadata jsontext.Value         `json:"metadata"`
			State    db.CanonicalCloseState `json:"close_state"`
		}{subject, "Current closed snapshot", "closed", "relay-member", jsontext.Value(`{}`), db.CanonicalCloseState{IssueUID: subject, StatusIntentUID: clock.EventUID, StatusClock: clock, CloseBoundaryClock: &clock, Value: &value}}
	} else {
		payload = struct {
			Reason string              `json:"reason"`
			Value  db.ScopedCloseValue `json:"close_value"`
		}{"duplicate", value}
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return raw
}

func RunScopedV1CloseIngress(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, snapshot := range []bool{false, true} {
		for _, target := range []string{"cross_declared_missing", "same_declared_foreign_actual", "same_project", "same_project_deferred"} {
			name := "original/" + target
			if snapshot {
				name = "snapshot/" + target
			}
			t.Run(name, func(t *testing.T) {
				s := open(t)
				ctx := t.Context()
				f := newRelayLinkBoundaryFixture(t, s, "machine-boundary")
				local, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.project.ID, Author: "relay-member", Title: "Local peer"})
				require.NoError(t, err)
				foreign, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.sibling.ID, Author: "relay-member", Title: "Foreign peer"})
				require.NoError(t, err)
				subject, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.project.ID, Author: "relay-member", Title: "Subject"})
				require.NoError(t, err)
				origin := scopedOwnerUID(1080)
				grant, err := s.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.project.ID, ParentTokenID: f.parentTokenID, SpokeInstanceUID: origin, ProtocolVersion: db.RelayProtocolVersion, Token: "machine-boundary-transport-test-token"})
				require.NoError(t, err)
				value := db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.project.UID, PeerIssueUID: local.UID}
				switch target {
				case "cross_declared_missing":
					value.PeerProjectUID = f.sibling.UID
					value.PeerIssueUID = scopedOwnerUID(1081)
				case "same_declared_foreign_actual":
					value.PeerIssueUID = foreign.UID
				case "same_project_deferred":
					value.PeerIssueUID = scopedOwnerUID(1081)
				}
				history, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
				require.NoError(t, err)
				var physical int64
				for _, e := range history {
					if e.HLCPhysicalMS >= physical {
						physical = e.HLCPhysicalMS + 1000
					}
				}
				kind := "issue.closed"
				subjectUID := subject.UID
				if snapshot {
					kind = "issue.created"
					subjectUID = scopedOwnerUID(1082)
				}
				event := newRemoteEvent(t, f.project, &subjectUID, kind, "relay-member", origin, physical, scopedV1ClosePayload(t, snapshot, subjectUID, value, origin, physical))
				body, err := db.EncodeRelaySourceEvent(event)
				require.NoError(t, err)
				envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: f.project.UID, AuthorityUID: s.InstanceUID(), SenderInstanceUID: origin, ReceiverInstanceUID: s.InstanceUID(), Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{origin}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
				require.NoError(t, err)
				writeCtx := db.WithRootAttribution(ctx, f.signer, "relay-member")
				accepted, err := s.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
				denied := target == "cross_declared_missing" || target == "same_declared_foreign_actual"
				if denied {
					require.Error(t, err, "v1 must refuse declared or actual cross-project typed machine peer")
				} else {
					require.NoError(t, err, "same-project typed machine peer remains native-valid, including deferred peer")
					require.EqualValues(t, 1, accepted.Through)
				}
				events, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
				require.NoError(t, err)
				found := false
				for _, stored := range events {
					if stored.UID == event.EventUID {
						found = true
						require.Equal(t, event.Payload, jsontext.Value(stored.Payload))
						require.Equal(t, event.ContentHash, stored.ContentHash)
					}
				}
				require.Equal(t, !denied, found, "refused original must not be retained; accepted original remains exact")
			})
		}
	}
}

func RunScopedV1CloseBootstrap(t *testing.T, s db.Storage) {
	t.Helper()
	ctx := t.Context()
	f := newRelayLinkBoundaryFixture(t, s, "machine-bootstrap")
	subject, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.project.ID, Author: "relay-member", Title: "Historical subject"})
	require.NoError(t, err)
	peer, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.sibling.ID, Author: "relay-member", Title: "Private peer"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, f.signer, "relay-member")
	_, closed, changed, err := s.CloseIssueGuarded(writeCtx, db.CloseIssueParams{IssueID: subject.ID, Actor: "relay-member", Reason: "duplicate", CloseValue: &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.sibling.UID, PeerIssueUID: peer.UID}})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, closed)
	grant, err := s.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.project.ID, ParentTokenID: f.parentTokenID, SpokeInstanceUID: scopedOwnerUID(1083), ProtocolVersion: db.RelayProtocolVersion, Token: "machine-bootstrap-transport-test-token"})
	require.NoError(t, err)
	required, err := s.(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.True(t, required, "cross-project typed original is unsafe retained v1 bootstrap history")
	pending, err := s.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1024)
	require.NoError(t, err)
	for _, offer := range pending {
		require.NotEqual(t, closed[0].UID, offer.SourceUID, "private original must never be seeded into v1 event stream")
	}
	history, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	found := false
	for _, event := range history {
		if event.UID == closed[0].UID {
			found = true
			require.Equal(t, closed[0].Payload, event.Payload)
			require.Equal(t, closed[0].ContentHash, event.ContentHash)
		}
	}
	require.True(t, found, "owner retains original bytes")
}

func RunScopedV1CloseReset(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, snapshotCarrier := range []bool{false, true} {
		for _, cross := range []bool{false, true} {
			name := "original/same_project"
			if snapshotCarrier {
				name = "snapshot/same_project"
			}
			if cross {
				name += "_cross_declared"
			}
			t.Run(name, func(t *testing.T) {
				s := open(t)
				ctx := t.Context()
				project, err := s.CreateProject(ctx, "machine-reset")
				require.NoError(t, err)
				rootUID := scopedOwnerUID(1084)
				binding := scopedOwnerUID(1085)
				public, private, err := ed25519.GenerateKey(nil)
				require.NoError(t, err)
				pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
				require.NoError(t, s.PinRootAuthority(ctx, pin))
				_, err = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "relay-member", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				_, err = s.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: binding, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, s.InstanceUID()}, LocalActor: "relay-member", ServeDownstream: true, ResetEpoch: 1})
				require.NoError(t, err)
				root := &rootResetFixture{t: t, project: project, pin: pin, private: private, rootUID: rootUID, relayUID: s.InstanceUID(), binding: binding}
				created := root.created("Reset subject", 300, 1)
				value := db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: project.UID, PeerIssueUID: scopedOwnerUID(1086)}
				if cross {
					value.PeerProjectUID = scopedOwnerUID(1087)
				}
				kind := "issue.closed"
				if snapshotCarrier {
					kind = "issue.snapshot"
				}
				event := newRemoteEvent(t, project, created.event.IssueUID, kind, "relay-member", rootUID, 500, scopedV1ClosePayload(t, snapshotCarrier, *created.event.IssueUID, value, rootUID, 500))
				root.receipt(event, 2)
				manifest, state, translation := root.checkpoint(2, 2, created)
				first, err := db.EncodeRelaySourceEvent(created.event)
				require.NoError(t, err)
				second, err := db.EncodeRelaySourceEvent(event)
				require.NoError(t, err)
				state.Events, err = json.Marshal([][]byte{first, second})
				require.NoError(t, err)
				manifest, err = db.SignRootResetManifest(manifest, state, private)
				require.NoError(t, err)
				translation.SnapshotDigest = manifest.SnapshotDigest
				err = s.(db.RelayResetStore).InstallRelayReset(ctx, binding, manifest, state, translation)
				if cross {
					require.Error(t, err, "root-signed reset still cannot introduce cross-project machine peer into v1")
				} else {
					require.NoError(t, err, "same-project deferred machine peer stays valid in actual signed native reset")
				}
				live, err := s.FederationBindingByProject(ctx, project.ID)
				require.NoError(t, err)
				if cross {
					require.EqualValues(t, 1, live.RelayConfig.ResetEpoch)
				} else {
					require.EqualValues(t, 2, live.RelayConfig.ResetEpoch)
					issue, err := s.IssueByUID(ctx, *created.event.IssueUID, db.IncludeDeletedYes)
					require.NoError(t, err)
					require.Equal(t, "closed", issue.Status)
				}
			})
		}
	}
}

func RunScopedCaptureLegacyResetFenced(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, guarded := range []bool{false, true} {
		name := "canonical_root"
		if guarded {
			name = "grant_revoked_guarded_reset"
		}
		t.Run(name, func(t *testing.T) {
			s := open(t)
			f := NewScopedCaptureFixture(t, s)
			ctx := t.Context()
			_, closed, changed, err := s.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "done"})
			require.NoError(t, err)
			require.True(t, changed)
			require.NotEmpty(t, closed)
			_, err = s.(interface {
				CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
			}).CaptureScopedView(ctx, f.Params.View.Key)
			require.NoError(t, err)
			if guarded {
				require.NoError(t, s.RevokeFederationEnrollment(ctx, f.Grant.Enrollment.ID))
			}
			before := scopedAuthoritySnapshot(t, s)
			if guarded {
				high, err := s.MaxEventID(ctx)
				require.NoError(t, err)
				err = s.(interface {
					ResetFederatedProjectIfNoPendingPush(context.Context, int64, int64, int64, string, int64) error
				}).ResetFederatedProjectIfNoPendingPush(ctx, f.Projects[0].ID, high, high, s.InstanceUID(), high)
				require.ErrorIs(t, err, db.ErrRelayResetRequiresRootProof)
			} else {
				require.ErrorIs(t, s.ResetFederatedProject(ctx, f.Projects[0].ID, 0, 0), db.ErrRelayResetRequiresRootProof)
			}
			require.Equal(t, before, scopedAuthoritySnapshot(t, s), "retained floor2 keeps original bodies/current proof/owed capture and native rows exact")
			require.NoError(t, db.ValidateImportRecords(before))
		})
	}
}

// ScopedPredicateABAFixture uses real native baseline writes and one valid
// remote batch whose final SQL values equal the baseline. Its original writers
// remain distinct evidence even when native materialization prunes equal rows.
type ScopedPredicateABAFixture struct {
	ScopedCaptureFixture
	Batch                     db.FederationIngestParams
	Current                   db.Issue
	CurrentProject            db.Project
	Parent                    db.Issue
	HistoricalIssueMetadata   db.RemoteEvent
	HistoricalProjectMetadata db.RemoteEvent
}

func NewScopedPredicateABAFixture(t *testing.T, s db.Storage) ScopedPredicateABAFixture {
	t.Helper()
	ctx := t.Context()
	f := NewScopedCaptureFixture(t, s)
	b, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Author: "member", Title: "Parent B"})
	require.NoError(t, err)
	c, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Author: "member", Title: "Parent C"})
	require.NoError(t, err)
	_, _, err = s.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: b.ID, Type: "parent", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: f.Issue.ID, FromUID: f.Issue.UID, FromShortID: f.Issue.ShortID, ToUID: b.UID, ToShortID: b.ShortID, Actor: "member"})
	require.NoError(t, err)
	_, err = s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"bucket": jsontext.Value(`{"x":"B","y":0}`)}})
	require.NoError(t, err)
	_, err = s.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: f.Projects[0].ID, Actor: "member", Patch: map[string]jsontext.Value{"bucket": jsontext.Value(`{"x":"B","y":0}`)}})
	require.NoError(t, err)
	current, err := s.IssueByUID(ctx, f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	project, err := s.ProjectByUID(ctx, f.Projects[0].UID)
	require.NoError(t, err)
	history, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	var physical int64
	for _, e := range history {
		if e.HLCPhysicalMS >= physical {
			physical = e.HLCPhysicalMS + 1000
		}
	}
	origin := scopedOwnerUID(1110)
	batch := db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "member"}
	for n, kind := range []string{"issue.metadata_updated", "issue.linked", "project.metadata_updated", "issue.metadata_updated", "issue.linked", "project.metadata_updated"} {
		body := `{"diff":{"bucket":{"from":{"x":"B","y":0},"to":{"x":"C","y":0}}}}`
		if n >= 3 {
			body = `{"diff":{"bucket":{"from":{"x":"C","y":0},"to":{"x":"B","y":0}}}}`
		}
		if kind == "issue.linked" {
			parent := c.UID
			if n >= 3 {
				parent = b.UID
			}
			body = `{"from_uid":"` + f.Issue.UID + `","to_uid":"` + parent + `","type":"parent"}`
		}
		issueUID := &f.Issue.UID
		if kind == "project.metadata_updated" {
			issueUID = nil
		}
		event := newRemoteEvent(t, project, issueUID, kind, "member", origin, physical+int64(n), jsontext.Value(body))
		event.EventUID = scopedOwnerUID(1120 + n)
		event.CreatedAt = current.UpdatedAt // native same-value row pruning control
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		batch.Events = append(batch.Events, db.FederationIngestEvent{SourceEventID: int64(n + 1), Event: event})
	}
	// Accept unrelated LATER nested-key writers before the delayed batch.
	// New x20/x30 ABA must remain visible beside existing bucket.y40, rather
	// than being hidden by one subject-wide last Clock40.
	historyBatch := db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "member"}
	var historical []db.RemoteEvent
	for n, kind := range []string{"issue.metadata_updated", "project.metadata_updated"} {
		issueUID := &f.Issue.UID
		if n == 1 {
			issueUID = nil
		}
		event := newRemoteEvent(t, project, issueUID, kind, "member", origin, physical+100+int64(n), jsontext.Value(`{"diff":{"bucket":{"from":{"x":"B","y":0},"to":{"x":"B","y":1}}}}`))
		event.EventUID = scopedOwnerUID(1140 + n)
		event.CreatedAt = current.UpdatedAt
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		historical = append(historical, event)
		historyBatch.Events = append(historyBatch.Events, db.FederationIngestEvent{SourceEventID: int64(n + 1), Event: event})
	}
	accepted, err := s.IngestFederationEvents(ctx, historyBatch)
	require.NoError(t, err)
	require.Equal(t, 2, accepted.Accepted)
	for n := range batch.Events {
		batch.Events[n].SourceEventID += 2
	}
	current, err = s.IssueByUID(ctx, f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	project, err = s.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	return ScopedPredicateABAFixture{ScopedCaptureFixture: f, Batch: batch, Current: current, CurrentProject: project, Parent: b, HistoricalIssueMetadata: historical[0], HistoricalProjectMetadata: historical[1]}
}

func AssertScopedPredicateABAAccepted(t *testing.T, s db.Storage, f ScopedPredicateABAFixture) {
	t.Helper()
	result, err := s.IngestFederationEvents(t.Context(), f.Batch)
	require.NoError(t, err)
	require.Equal(t, 6, result.Accepted)
	current, err := s.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Current.Revision, current.Revision, "effective ABA evidence survives when native final issue row is unchanged")
	require.Equal(t, f.Current.Metadata, current.Metadata)
	project, err := s.ProjectByUID(t.Context(), f.CurrentProject.UID)
	require.NoError(t, err)
	require.Equal(t, f.CurrentProject.Revision, project.Revision)
	links, err := s.LinksByIssue(t.Context(), f.Issue.ID)
	require.NoError(t, err)
	found := false
	for _, link := range links {
		if link.Type == "parent" && link.FromIssueUID == f.Issue.UID {
			require.Equal(t, f.Parent.UID, link.ToIssueUID)
			found = true
		}
	}
	require.True(t, found)
	result, err = s.IngestFederationEvents(t.Context(), f.Batch)
	require.NoError(t, err)
	require.Equal(t, 6, result.Duplicates)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

// RunScopedPredicateCronWitness characterizes existing native winner semantics;
// it does not install the later incarnation updater or change cron SQL.
func RunScopedPredicateCronWitness(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	ctx := t.Context()
	in := db.PutCronWorkflow{ProjectID: f.Projects[0].ID, Name: "Current workflow", Actor: "creator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}}
	first, firstEvent, err := s.PutCronWorkflow(ctx, in)
	require.NoError(t, err)
	require.Equal(t, firstEvent.UID, first.DefinitionEventUID)
	in.UID, in.ExpectedEventUID, in.Actor = first.UID, first.DefinitionEventUID, "editor"
	updated, updatedEvent, err := s.PutCronWorkflow(ctx, in)
	require.NoError(t, err)
	require.Equal(t, first.Definition, updated.Definition, "identical document CAS remains an explicit definition write")
	require.Equal(t, first.Revision+1, updated.Revision)
	require.Equal(t, updatedEvent.UID, updated.DefinitionEventUID)
	require.NotEqual(t, first.DefinitionEventUID, updated.DefinitionEventUID)
	require.Equal(t, db.CronDefinitionHLC{Version: 1, PhysicalMS: updatedEvent.HLCPhysicalMS, Counter: updatedEvent.HLCCounter, OriginInstanceUID: updatedEvent.OriginInstanceUID}, updated.DefinitionHLC)
	require.Equal(t, first.Author, updated.Author, "latest editor is not original creator")
	history, err := s.EventsAfter(ctx, db.EventsAfterParams{ProjectID: f.Projects[0].ID, Limit: 1000})
	require.NoError(t, err)
	_, _, err = s.PutCronWorkflow(ctx, in) // stale original expected winner
	require.ErrorIs(t, err, db.ErrCronConflict)
	unchanged, err := s.CronWorkflow(ctx, first.ProjectID, first.UID)
	require.NoError(t, err)
	require.Equal(t, updated, unchanged)
	afterConflict, err := s.EventsAfter(ctx, db.EventsAfterParams{ProjectID: f.Projects[0].ID, Limit: 1000})
	require.NoError(t, err)
	require.Equal(t, history, afterConflict, "failed CAS emits no durable source original")
	in.ExpectedEventUID, in.Deleted = updated.DefinitionEventUID, true
	deleted, deletedEvent, err := s.PutCronWorkflow(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, deleted.DeletedAt)
	require.Equal(t, deletedEvent.UID, deleted.DefinitionEventUID)
	require.Equal(t, "cron.workflow.deleted", deletedEvent.Type)
	in.ExpectedEventUID, in.Deleted = deleted.DefinitionEventUID, false
	restored, restoredEvent, err := s.PutCronWorkflow(ctx, in)
	require.NoError(t, err)
	require.Nil(t, restored.DeletedAt)
	require.Equal(t, restoredEvent.UID, restored.DefinitionEventUID)
	require.Equal(t, "cron.workflow.restored", restoredEvent.Type)
	job, _, err := s.PutCronJob(ctx, db.PutCronJob{UID: first.UID, ProjectID: first.ProjectID, Name: "Independent typed job", Actor: "job-creator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect example work"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example work"}, Overlap: "forbid", Catchup: "skip"}})
	require.NoError(t, err)
	require.NotEqual(t, restored.DefinitionEventUID, job.DefinitionEventUID)
	_, changed, err := s.RefreshProjectFederationBaseline(ctx, first.ProjectID, "member")
	require.NoError(t, err)
	require.True(t, changed)
	events, err := s.EventsAfter(ctx, db.EventsAfterParams{ProjectID: first.ProjectID, Limit: 1000})
	require.NoError(t, err)
	snapshots := map[string]db.CronDefinitionEvent{}
	var snapshotEvents []db.Event
	for _, e := range events {
		if e.Type != "cron.job.snapshot" && e.Type != "cron.workflow.snapshot" {
			continue
		}
		var payload db.CronDefinitionEvent
		require.NoError(t, json.Unmarshal([]byte(e.Payload), &payload))
		if payload.UID != first.UID {
			continue
		}
		snapshots[e.Type] = payload
		snapshotEvents = append(snapshotEvents, e)
		require.NotEqual(t, e.UID, payload.DefinitionEventUID, "snapshot envelope is not original winner")
	}
	require.Len(t, snapshots, 2, "typed same-UID job/workflow both survive native snapshot construction")
	require.Equal(t, restored.DefinitionEventUID, snapshots["cron.workflow.snapshot"].DefinitionEventUID)
	require.Equal(t, &restored.DefinitionHLC, snapshots["cron.workflow.snapshot"].DefinitionHLC)
	require.Equal(t, job.DefinitionEventUID, snapshots["cron.job.snapshot"].DefinitionEventUID)
	require.Equal(t, &job.DefinitionHLC, snapshots["cron.job.snapshot"].DefinitionHLC)
	folded := scopedNativeFold(snapshotEvents)
	require.Equal(t, restored.DefinitionEventUID, folded.CronWorkflows[first.UID].DefinitionEventUID)
	require.Equal(t, restored.DefinitionHLC, folded.CronWorkflows[first.UID].DefinitionHLC)
	require.Equal(t, job.DefinitionEventUID, folded.CronJobs[first.UID].DefinitionEventUID)
	require.Equal(t, job.DefinitionHLC, folded.CronJobs[first.UID].DefinitionHLC)
	current, err := s.CronWorkflow(ctx, first.ProjectID, first.UID)
	require.NoError(t, err)
	require.Equal(t, restored, current, "baseline snapshots never advance native original winner/revision")
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

// RunScopedReplicaCloseLifetime exercises actual pull/materialization and a
// validated complete owner replay of compacted history. clear is only the
// private native replacement primitive, not a verified v2 reset endpoint.
func RunScopedReplicaCloseLifetime(t *testing.T, open func(*testing.T) db.Storage, clear func(context.Context, db.Storage, int64, bool) error) {
	t.Helper()
	for _, operation := range []string{"stale_prune", "private_clear", "private_clear_rollback", "leave_preserves", "legacy_reset_refuses", "stale_prune_root_dirty", "private_clear_root_dirty"} {
		t.Run(operation, func(t *testing.T) {
			rootDirty := strings.HasSuffix(operation, "_root_dirty")
			operation := strings.TrimSuffix(operation, "_root_dirty")
			root, replica := open(t), open(t)
			f := NewScopedCaptureFixture(t, root)
			_, _, _, err := root.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "member"})
			require.NoError(t, err)
			subject, _, err := root.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Retained close subject"})
			require.NoError(t, err)
			value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: f.Projects[0].UID, PeerIssueUID: f.Issue.UID}
			_, closeEvents, _, err := root.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: subject.ID, Reason: "duplicate", Actor: "member", CloseValue: value})
			require.NoError(t, err)
			view, projects := NewScopedIncomingAuthorityFixture(t, replica)
			InstallScopedIncomingAuthority(t, replica, view, projects)
			// Independent pending custody bytes use the accepted typed owner fixture;
			// they are not evidence of runtime control admission or activation.
			records := scopedAuthoritySnapshot(t, replica)
			control := ScopedOwnerBackupFixture(t).Control
			records = append(records, control)
			require.NoError(t, db.ValidateImportRecords(records))
			require.NoError(t, replica.ImportReplay(t.Context(), records, db.ImportOptions{}))
			for n := range projects {
				projects[n], err = replica.ProjectByUID(t.Context(), projects[n].UID)
				require.NoError(t, err)
			}
			originals := map[string]db.RemoteEvent{}
			for n, p := range f.Projects {
				require.Equal(t, p.UID, projects[n].UID)
				events, err := root.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: p.ID, Limit: 1000})
				require.NoError(t, err)
				var portable []db.RemoteEvent
				for _, e := range events {
					if e.IssueUID == nil {
						continue
					}
					remote := db.RemoteEventFromStored(e)
					portable = append(portable, remote)
					originals[e.UID] = remote
				}
				require.NotEmpty(t, portable)
				inserted, err := replica.InsertRemoteEvents(t.Context(), projects[n].ID, portable)
				require.NoError(t, err, "actual native pull admission preflight")
				for _, fresh := range inserted {
					require.True(t, fresh)
				}
				require.NoError(t, replica.MaterializeFederatedProject(t.Context(), projects[n].ID))
			}
			var localSubject db.Issue
			var localParams db.CreateScopedRelayEnrollmentParams
			var localBefore db.ScopedWorkExport
			var localProof db.CanonicalCloseState
			if rootDirty {
				localSubject, localParams = newScopedLocalRootPeer(t, replica, f.Issue.UID, projects[0].UID)
				for n := range projects {
					projects[n], err = replica.ProjectByUID(t.Context(), projects[n].UID)
					require.NoError(t, err)
				}
				_, err := replica.(interface {
					CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
				}).CaptureScopedView(t.Context(), localParams.View.Key)
				require.NoError(t, err, "real separate local canonical root capture preflight")
				localBefore = ScopedCaptureWork(t, replica, localParams.View.Key.ViewUID)
				require.NotNil(t, localBefore.CaptureUID)
				localProof = ScopedCurrentClose(t, replica, localSubject.UID)
				require.Equal(t, f.Issue.UID, localProof.Value.PeerIssueUID)
			}
			before := scopedAuthoritySnapshot(t, replica)
			require.NoError(t, db.ValidateImportRecords(before), "native owned-replica proof is valid before removal")
			proof := ScopedCurrentClose(t, replica, subject.UID)
			require.Equal(t, value, proof.Value)
			require.Equal(t, closeEvents[0].UID, proof.StatusIntentUID)
			require.Equal(t, control, scopedReplicaControl(t, before, control.RequestUID))
			if operation == "leave_preserves" {
				_, err := replica.LeaveFederationReplica(t.Context(), projects[0].ID)
				require.NoError(t, err)
				require.Equal(t, proof, ScopedCurrentClose(t, replica, subject.UID))
				_, err = replica.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err, "leave intentionally retains replica rows and their proof")
			} else if operation == "legacy_reset_refuses" {
				require.ErrorIs(t, replica.ResetFederatedProject(t.Context(), projects[0].ID, 0, 0), db.ErrRelayResetRequiresRootProof)
				require.Equal(t, before, scopedAuthoritySnapshot(t, replica), "floor2 legacy reset refusal is atomic")
			} else {
				compacted := make([]db.ImportRecord, 0, len(before))
				removed := 0
				for _, r := range before {
					if event, ok := r.(*db.EventExport); ok && event.ProjectID == projects[0].ID {
						removed++
						continue
					}
					compacted = append(compacted, r)
				}
				require.Positive(t, removed)
				require.NoError(t, db.ValidateImportRecords(compacted), "complete compacted owner representation is valid before native prune")
				require.NoError(t, replica.ImportReplay(t.Context(), compacted, db.ImportOptions{}))
				for n := range projects {
					projects[n], err = replica.ProjectByUID(t.Context(), projects[n].UID)
					require.NoError(t, err)
				}
				current := projects[0]
				if operation == "stale_prune" {
					require.NoError(t, replica.MaterializeFederatedProject(t.Context(), current.ID))
				} else {
					require.NoError(t, clear(t.Context(), replica, current.ID, operation == "private_clear_rollback"))
				}
				if operation == "private_clear_rollback" {
					require.Equal(t, compacted, scopedAuthoritySnapshot(t, replica), "private clear rolls back proof, rows and all custody together")
					return
				}
				_, err = replica.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
				require.ErrorIs(t, err, db.ErrNotFound, "native removal really deleted the subject row")
				after := scopedAuthoritySnapshot(t, replica)
				for _, r := range after {
					if close, ok := r.(*db.ScopedCanonicalCloseExport); ok {
						require.NotEqual(t, f.Issue.UID, close.IssueUID, "removed replica subject cannot leave an orphan proof")
					}
				}
				invalidated := ScopedCurrentClose(t, replica, subject.UID)
				require.Nil(t, invalidated.Value, "current peer disposition is invalidated when replica peer disappears")
				require.Equal(t, proof.StatusIntentUID, invalidated.StatusIntentUID)
				require.Equal(t, proof.StatusClock, invalidated.StatusClock)
				require.Equal(t, proof.CloseBoundaryClock, invalidated.CloseBoundaryClock)
				if rootDirty {
					localNow := ScopedCurrentClose(t, replica, localSubject.UID)
					require.Nil(t, localNow.Value)
					localProof.Value = nil
					require.Equal(t, localProof, localNow)
					work := ScopedCaptureWork(t, replica, localParams.View.Key.ViewUID)
					require.Greater(t, work.DirtyRevision, localBefore.DirtyRevision, "replica peer invalidation wakes independently retained local canonical root work")
					require.Equal(t, localBefore.CaptureUID, work.CaptureUID)
					require.Equal(t, localBefore.CaptureBytes, work.CaptureBytes, "owed capture is immutable while greater dirty work is recorded")
				}
				require.NoError(t, replica.MaterializeFederatedProject(t.Context(), projects[1].ID))
				require.Equal(t, invalidated, ScopedCurrentClose(t, replica, subject.UID), "retained original C1 cannot resurrect invalidated current peer")
				// Peer returns under exactly its original retained bytes; known-null stays
				// sticky until a genuinely new explicit status intent replaces C1.
				for _, remote := range originals {
					if remote.ProjectUID != current.UID {
						continue
					}
					_, err := replica.InsertRemoteEvent(t.Context(), current.ID, remote)
					require.NoError(t, err)
				}
				require.NoError(t, replica.MaterializeFederatedProject(t.Context(), current.ID))
				require.NoError(t, replica.MaterializeFederatedProject(t.Context(), projects[1].ID))
				require.Equal(t, invalidated, ScopedCurrentClose(t, replica, subject.UID))
				_, reopened, _, err := root.ReopenIssue(t.Context(), subject.ID, "member")
				require.NoError(t, err)
				_, nextClose, _, err := root.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: subject.ID, Reason: "duplicate", Actor: "member", CloseValue: value})
				require.NoError(t, err)
				require.NotNil(t, reopened)
				for _, e := range append([]db.Event{*reopened}, nextClose...) {
					_, err := replica.InsertRemoteEvent(t.Context(), projects[1].ID, db.RemoteEventFromStored(e))
					require.NoError(t, err)
				}
				require.NoError(t, replica.MaterializeFederatedProject(t.Context(), projects[1].ID))
				require.Equal(t, value, ScopedCurrentClose(t, replica, subject.UID).Value)
				require.Equal(t, nextClose[0].UID, ScopedCurrentClose(t, replica, subject.UID).StatusIntentUID)
			}
			after := scopedAuthoritySnapshot(t, replica)
			require.Equal(t, control, scopedReplicaControl(t, after, control.RequestUID), "independent owed control bytes remain exact")
			require.NoError(t, db.ValidateImportRecords(after))
			fresh := open(t)
			require.NoError(t, fresh.ImportReplay(t.Context(), after, db.ImportOptions{}))
			require.Equal(t, after, scopedAuthoritySnapshot(t, fresh), "complete owner replay preserves current proof and independent custody")
			native, err := replica.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
			require.NoError(t, err)
			for _, e := range native {
				if original, exists := originals[e.UID]; exists {
					require.Equal(t, original, db.RemoteEventFromStored(e), "retained native original body/hash never rewritten by derived cleanup")
				}
			}
		})
	}
}

func scopedReplicaControl(t *testing.T, records []db.ImportRecord, uid string) *db.ScopedControlCustodyExport {
	t.Helper()
	for _, r := range records {
		if control, ok := r.(*db.ScopedControlCustodyExport); ok && control.RequestUID == uid {
			return control
		}
	}
	t.Fatal("independent retained control missing")
	return nil
}

// newScopedLocalRootPeer adds a separate genuine local root alongside incoming
// replicas. Only retained cutover context is staged; grant/capture and current
// close mutation still use the real native APIs and current authority checks.
func newScopedLocalRootPeer(t *testing.T, s db.Storage, peerUID, peerProjectUID string) (db.Issue, db.CreateScopedRelayEnrollmentParams) {
	t.Helper()
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "local-canonical-project")
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(ctx, project.ID, "admin")
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{77}, 32))
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: s.InstanceUID(), PublicKey: []byte(key.Public().(ed25519.PublicKey))}
	pin.KeyID = db.RootPublicKeyID(pin.PublicKey)
	require.NoError(t, s.PinRootAuthority(ctx, pin))
	readiness, suspension := []byte("[]"), []byte("[]")
	certificate := db.ScopedProtocolCutoverCertificate{Version: 1, Cutover: db.ScopedProtocolCutoverExpectation{UID: scopedOwnerUID(1500), AuthorityUID: s.InstanceUID(), RequestDigest: strings.Repeat("d", 64), ProjectUIDs: []string{project.UID}, Floor: 2, ReadinessDigest: db.ScopedOfferedDigest(readiness), SuspendedBindingsDigest: db.ScopedOfferedDigest(suspension)}}
	raw, err := db.ScopedCutoverCertificateSigningBytes(certificate)
	require.NoError(t, err)
	certificate.Signatures = []db.ScopedSignature{{ProjectUID: project.UID, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	records := scopedAuthoritySnapshot(t, s)
	records = append(records, &db.ScopedProjectExport{ProjectUID: project.UID, ProtocolFloor: 2, RootCutoverUID: &certificate.Cutover.UID}, &db.ScopedProtocolCutoverExport{CutoverUID: certificate.Cutover.UID, RequestDigest: certificate.Cutover.RequestDigest, ProjectUIDsBytes: scopedOwnerBytes(t, certificate.Cutover.ProjectUIDs), Floor: 2, ReadinessBytes: readiness, SuspendedBindingsBytes: suspension, CertificateBytes: scopedOwnerBytes(t, certificate), State: "committed"})
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(ctx, records, db.ImportOptions{}))
	project, err = s.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	team, _, err := s.CreateTeam(ctx, "local-canonical-team", "admin")
	require.NoError(t, err)
	_, _, err = s.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
	require.NoError(t, err)
	parent, _, err := s.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "local-canonical-parent", Actor: "local-root-member", AdminActor: "admin", TeamUIDs: []string{team.UID}})
	require.NoError(t, err)
	v := db.ScopedViewDescriptor{Version: 2, Schema: 1, Key: db.ScopedViewKey{ViewUID: scopedOwnerUID(1501), Generation: 1}, AuthorityUID: s.InstanceUID(), DestinationUID: scopedOwnerUID(1502), RouteDigest: strings.Repeat("e", 64), RequestDigest: strings.Repeat("f", 64), RootCutoverUID: certificate.Cutover.UID, Route: []db.ScopedHopBinding{{BindingUID: scopedOwnerUID(1503), InstanceUID: scopedOwnerUID(1502), Epoch: 1}}, Projects: []db.ScopedProjectPin{{ProjectUID: project.UID, Pin: pin}}}
	params := db.CreateScopedRelayEnrollmentParams{View: v, CatalogEntryUID: scopedOwnerUID(1504), ParentTokenID: parent.ID, AnchorProjectID: project.ID, Token: "local-canonical-transport"}
	_, err = s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(ctx, params)
	require.NoError(t, err)
	subject, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "local-root-member", Title: "Local root with replica peer"})
	require.NoError(t, err)
	subject, _, changed, err := s.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: subject.ID, Actor: "local-root-member", Reason: "duplicate", CloseValue: &db.ScopedCloseValue{Kind: "duplicate", PeerIssueUID: peerUID, PeerProjectUID: peerProjectUID}})
	require.NoError(t, err)
	require.True(t, changed, "native permission path really admits local-root disposition to replica peer")
	return subject, params
}
