//go:build rcpl_private

package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RunScopedProjectorCaptureIntegration joins the real native capture API to the
// PR03 pure helpers. Native fixtures own admission and SQL consistency; this
// helper neither constructs capture facts nor publishes or completes work.
func RunScopedProjectorCaptureIntegration(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, sameProject := range []bool{false, true} {
		name := "first_cut_cross_project_cron_identity"
		if sameProject {
			name = "first_cut_same_project_cron_identity"
		}
		t.Run(name, func(t *testing.T) {
			runScopedProjectorFirstCapture(t, open(t), sameProject)
		})
	}
	t.Run("exact_reuse_after_native_edit", func(t *testing.T) {
		s := open(t)
		f := NewScopedCaptureFixture(t, s)
		assertScopedProjectorNoCapture(t, s, f)
		capturer := scopedProjectorNativeCapturer(t, s)
		first, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
		require.NoError(t, err)
		before := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
		projected := scopedProjectorCapturedCollections(t, first, f.Params.View)
		signers, pins := scopedProjectorCaptureKeys(t, s, f)
		checkpoint := scopedProjectorCaptureCheckpoint(t, first, f.Params.View, projected, signers, pins)

		newTitle := "Native edit after retained capture"
		_, err = s.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Issue.ID, Actor: "later-editor", Title: &newTitle})
		require.NoError(t, err)
		current, err := s.IssueByID(t.Context(), f.Issue.ID)
		require.NoError(t, err)
		require.Equal(t, newTitle, current.Title)
		dirty := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
		require.Greater(t, dirty.DirtyRevision, before.DirtyRevision)
		again, err := capturer.CaptureScopedView(t.Context(), f.Params.View.Key)
		require.NoError(t, err)
		require.Equal(t, first, again, "owed capture is not a fresh current read")
		after := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
		require.Equal(t, before.CaptureUID, after.CaptureUID)
		require.Equal(t, before.CapturedRevision, after.CapturedRevision)
		require.Equal(t, before.CaptureBytes, after.CaptureBytes)
		require.Equal(t, before.CaptureDigest, after.CaptureDigest)
		reused := scopedProjectorCapturedCollections(t, again, f.Params.View)
		require.Equal(t, scopedProjectorCaptureJSON(t, projected), scopedProjectorCaptureJSON(t, reused))
		require.Equal(t, checkpoint, scopedProjectorCaptureCheckpoint(t, again, f.Params.View, reused, signers, pins))
		effects, err := db.DiffScopedState(projected, reused, 0)
		require.NoError(t, err)
		require.Empty(t, effects, "exact retained reuse allocates no new public effect")
	})
}

func runScopedProjectorFirstCapture(t *testing.T, s db.Storage, sameProject bool) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	assertScopedProjectorNoCapture(t, s, f)
	ctx := t.Context()
	projectA, projectB := f.Projects[0], f.Projects[1]
	writeCtx := db.WithRootAttribution(ctx, ScopedCaptureSigner(t, f, projectA.UID), "member")
	issue, created, err := s.CreateIssue(writeCtx, db.CreateIssueParams{
		ProjectID: projectA.ID, Title: "Original title", Body: "Original body", Author: "original-creator",
		Metadata: map[string]jsontext.Value{"example.flag": jsontext.Value(`true`)},
	})
	require.NoError(t, err)
	comment, commented, err := s.CreateComment(writeCtx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "comment-creator", Teammate: "fixture-helper", Body: "Original comment",
	})
	require.NoError(t, err)
	reply, _, err := s.CreateComment(writeCtx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "reply-creator", Body: "Current reply", ReplyToUID: comment.UID, ReplyKind: "reply", ValidateReply: true,
	})
	require.NoError(t, err)
	_, _, changed, err := s.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "comment-editor", Body: "Current comment"})
	require.NoError(t, err)
	require.True(t, changed)
	_, _, err = s.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "current-label", Actor: "editor"})
	require.NoError(t, err)
	_, err = s.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: projectA.ID, Actor: "editor", Patch: map[string]jsontext.Value{"example.project": jsontext.Value(`"current"`)}})
	require.NoError(t, err)

	const hiddenTitle = "hidden-capture-peer-title-marker"
	const hiddenBody = "hidden-capture-peer-body-marker"
	peer, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectB.ID, Title: hiddenTitle, Body: hiddenBody, Author: "peer-creator"})
	require.NoError(t, err)
	_, err = s.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "editor", SetParent: &f.Issue.ID, AddBlocks: []int64{peer.ID}})
	require.NoError(t, err)

	artifactStore, ok := s.(db.EmbeddingArtifactStorage)
	require.True(t, ok, "native portable artifact retention required")
	identity := embedding.ArtifactIdentity{
		ProjectUID: projectA.UID, IssueUID: issue.UID, ProducerInstanceUID: s.InstanceUID(),
		Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none",
		Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64),
	}
	stale, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifactStore.RetainEmbeddingArtifact(ctx, stale)
	require.NoError(t, err)
	require.True(t, durable)
	title, body := "Current title", "Current body"
	_, err = s.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "latest-editor", Title: &title, Body: &body})
	require.NoError(t, err)
	currentArtifact, err := embedding.NewArtifact(identity, embedding.EmbedText(title, body), [][]float32{{0, 1}})
	require.NoError(t, err)
	durable, err = artifactStore.RetainEmbeddingArtifact(ctx, currentArtifact)
	require.NoError(t, err)
	require.True(t, durable)
	identity.ProjectUID, identity.IssueUID = projectB.UID, peer.UID
	hiddenArtifact, err := embedding.NewArtifact(identity, embedding.EmbedText(hiddenTitle, hiddenBody), [][]float32{{1, 1}})
	require.NoError(t, err)
	durable, err = artifactStore.RetainEmbeddingArtifact(ctx, hiddenArtifact)
	require.NoError(t, err)
	require.True(t, durable)
	storedCurrent, err := artifactStore.StoredEmbeddingArtifact(ctx, projectA.UID, currentArtifact.Digest)
	require.NoError(t, err)
	require.Equal(t, currentArtifact, storedCurrent, "native retention owns exact portable artifact bytes")

	workflow, _, err := s.PutCronWorkflow(ctx, db.PutCronWorkflow{
		ProjectID: projectA.ID, Name: "Allowed workflow", Actor: "workflow-creator",
		Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}},
	})
	require.NoError(t, err)
	jobProject := projectB
	if sameProject {
		jobProject = projectA
	}
	const jobName = "cron-job-name-marker"
	const jobBody = "cron-job-body-marker"
	const jobCreator = "cron-job-creator-marker"
	job, _, err := s.PutCronJob(ctx, db.PutCronJob{
		ProjectID: jobProject.ID, UID: workflow.UID, Name: jobName, Actor: jobCreator,
		Definition: cron.JobDefinition{
			Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect current work"},
			Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Fixture occurrence", Body: jobBody}, Overlap: "forbid", Catchup: "skip",
		},
	})
	require.NoError(t, err)
	readWorkflow, err := s.CronWorkflow(ctx, projectA.ID, workflow.UID)
	require.NoError(t, err)
	readJob, err := s.CronJob(ctx, jobProject.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, workflow.UID, job.UID, "native kind namespaces permit identical UIDs")
	require.Equal(t, "Allowed workflow", readWorkflow.Name)
	require.Equal(t, jobName, readJob.Name)
	require.Equal(t, jobBody, readJob.Definition.Issue.Body)

	value := &db.ScopedCloseValue{Kind: "superseded", PeerProjectUID: projectB.UID, PeerIssueUID: peer.UID}
	closed, closeEvents, changed, err := s.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: issue.ID, Reason: "superseded", Actor: "closer", CloseValue: value})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, closeEvents)
	proof := ScopedCurrentClose(t, s, issue.UID)
	require.Equal(t, closeEvents[0].UID, proof.StatusIntentUID)
	require.Equal(t, &proof.StatusClock, proof.CloseBoundaryClock, "new explicit close retains its original private boundary")
	require.Equal(t, value, proof.Value)
	require.NoError(t, db.ValidateScopedCanonicalCloseState(proof, issue.UID, "closed"))
	_, err = s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "metadata-editor", Patch: map[string]jsontext.Value{"example.current": jsontext.Value(`"retained"`)}})
	require.NoError(t, err)
	require.Equal(t, proof, ScopedCurrentClose(t, s, issue.UID))

	// Literal current native reads anchor extraction independently of the oracle.
	readIssue, err := s.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, title, readIssue.Title)
	require.Equal(t, body, readIssue.Body)
	require.Equal(t, "closed", readIssue.Status)
	require.Equal(t, closed.UID, readIssue.UID)
	comments, err := s.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	for _, row := range comments {
		if row.UID == comment.UID {
			require.Equal(t, "Current comment", row.Body)
			require.Equal(t, "comment-creator", row.Author)
		}
		if row.UID == reply.UID {
			require.Equal(t, comment.UID, row.ReplyToUID)
			require.Equal(t, "reply", row.ReplyKind)
		}
	}
	issueReceipt, err := s.EntityAttribution(ctx, projectA.UID, "issue", issue.UID)
	require.NoError(t, err)
	commentReceipt, err := s.EntityAttribution(ctx, projectA.UID, "comment", comment.UID)
	require.NoError(t, err)
	require.Equal(t, created.UID, issueReceipt.EventUID)
	require.Equal(t, commented.UID, commentReceipt.EventUID)
	require.Equal(t, "original-creator", issueReceipt.SourceActor)
	require.Equal(t, "comment-creator", commentReceipt.SourceActor)
	signers, pins := scopedProjectorCaptureKeys(t, s, f)
	for _, pin := range pins {
		if pin.ProjectUID == projectA.UID {
			require.NoError(t, db.VerifyRootReceipt(pin, issueReceipt))
			require.NoError(t, db.VerifyRootReceipt(pin, commentReceipt))
		}
	}

	assertScopedProjectorNoCapture(t, s, f)
	before, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	capture, err := scopedProjectorNativeCapturer(t, s).CaptureScopedView(ctx, f.Params.View.Key)
	require.NoError(t, err)
	require.NotEmpty(t, capture.CaptureUID)
	require.Len(t, capture.ProjectCuts, 2)
	require.Contains(t, capture.ProjectCuts, projectA.UID)
	require.Contains(t, capture.ProjectCuts, projectB.UID)
	work := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Equal(t, &capture.CaptureUID, work.CaptureUID)
	require.NotNil(t, work.CaptureDigest)
	require.NotNil(t, work.CapturedRevision)
	require.Equal(t, db.ScopedOfferedDigest(work.CaptureBytes), *work.CaptureDigest)
	decoded, err := db.DecodeScopedCanonicalCapture(work.CaptureBytes)
	require.NoError(t, err)
	require.Equal(t, capture, decoded)
	current := capture.Fold.Issues[issue.UID]
	require.Equal(t, title, current.Title)
	require.Equal(t, body, current.Body)
	require.Equal(t, proof.StatusIntentUID, current.StatusIntentUID)
	require.Equal(t, proof.StatusClock, current.StatusClock)
	require.Equal(t, proof.CloseBoundaryClock, current.CloseBoundaryClock)
	require.Equal(t, value, current.CloseValue)
	require.Contains(t, capture.Close, proof)
	require.Equal(t, "Current comment", capture.Fold.Comments[comment.UID].Body)
	require.Equal(t, reply.ReplyToUID, capture.Fold.Comments[reply.UID].ReplyToUID)
	require.Equal(t, hiddenTitle, capture.Fold.Issues[peer.UID].Title)
	require.Equal(t, hiddenBody, capture.Fold.Issues[peer.UID].Body)
	require.Equal(t, []embedding.ArtifactManifest{currentArtifact.Manifest(), hiddenArtifact.Manifest()}, capture.Artifacts)
	after, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	require.Equal(t, before, after, "capture does not manufacture or rewrite source originals")

	broad := scopedProjectorCapturedCollections(t, capture, f.Params.View)
	var visibleIssue db.ScopedIssueState
	for _, row := range broad.Issues {
		if row.UID == issue.UID {
			visibleIssue = row
		}
	}
	require.Equal(t, issue.UID, visibleIssue.UID)
	require.Equal(t, readIssue.Title, visibleIssue.Title)
	require.Equal(t, readIssue.Body, visibleIssue.Body)
	require.Equal(t, "original-creator", visibleIssue.Author)
	require.JSONEq(t, `{"example.flag":true,"example.current":"retained"}`, string(visibleIssue.Metadata))
	var visibleComment db.ScopedCommentState
	for _, row := range broad.Comments {
		if row.UID == comment.UID {
			visibleComment = row
		}
	}
	require.Equal(t, comment.UID, visibleComment.UID)
	require.Equal(t, "Current comment", visibleComment.Body)
	require.Equal(t, "comment-creator", visibleComment.Author)
	require.Equal(t, "fixture-helper", visibleComment.Teammate)
	require.Contains(t, broad.Labels, db.ScopedLabelState{ProjectUID: projectA.UID, IssueUID: issue.UID, Label: "current-label"})
	for _, row := range broad.Projects {
		if row.UID == projectA.UID {
			require.JSONEq(t, `{"example.project":"current"}`, string(row.Metadata))
		}
	}
	closeRef := scopedProjectorCaptureReference(t, broad, "close", issue.UID, "")
	require.True(t, closeRef.Present)
	require.Equal(t, value, closeRef.Close)
	parent := scopedProjectorCaptureReference(t, broad, "parent", issue.UID, "")
	require.Equal(t, &db.ScopedEndpoint{ProjectUID: projectA.UID, IssueUID: f.Issue.UID}, parent.Peer)
	link := scopedProjectorCaptureReference(t, broad, "link", issue.UID, "")
	require.Equal(t, "blocks", link.Relation)
	require.Equal(t, &db.ScopedEndpoint{ProjectUID: projectB.UID, IssueUID: peer.UID}, link.Peer)
	replyRef := scopedProjectorCaptureReference(t, broad, "reply", issue.UID, reply.UID)
	require.Equal(t, comment.UID, replyRef.PeerEntityUID)
	issueCreation := scopedProjectorCaptureProvenance(t, broad, projectA.UID, "issue", issue.UID)
	require.Equal(t, "original-creator", issueCreation.Creator)
	require.Equal(t, "verified", issueCreation.AttributionStatus)
	commentCreation := scopedProjectorCaptureProvenance(t, broad, projectA.UID, "comment", comment.UID)
	require.Equal(t, "comment-creator", commentCreation.Creator)
	require.Equal(t, "fixture-helper", commentCreation.Teammate)
	require.Equal(t, "verified", commentCreation.AttributionStatus)
	require.Len(t, broad.Definitions, 2)
	for _, row := range broad.Definitions {
		require.Equal(t, workflow.UID, row.UID)
		switch row.Kind {
		case "workflow":
			require.Equal(t, projectA.UID, row.ProjectUID)
			require.Equal(t, readWorkflow.Name, row.Name)
			require.Equal(t, &readWorkflow.Definition, row.Workflow)
			require.Nil(t, row.Job)
		case "job":
			require.Equal(t, jobProject.UID, row.ProjectUID)
			require.Equal(t, readJob.Name, row.Name)
			require.Equal(t, &readJob.Definition, row.Job)
			require.Nil(t, row.Workflow)
		default:
			t.Fatalf("unexpected current definition kind %q", row.Kind)
		}
	}
	workflowCreation := scopedProjectorCaptureProvenance(t, broad, projectA.UID, "cron_workflow", workflow.UID)
	jobCreation := scopedProjectorCaptureProvenance(t, broad, jobProject.UID, "cron_job", job.UID)
	require.Equal(t, "workflow-creator", workflowCreation.Creator)
	require.Equal(t, jobCreator, jobCreation.Creator)
	require.Equal(t, "legacy", workflowCreation.AttributionStatus)
	require.Equal(t, "legacy", jobCreation.AttributionStatus)
	signedClose, err := db.SignScopedReference(f.Params.View, closeRef, signers)
	require.NoError(t, err)
	require.Len(t, signedClose.Signatures, 2)
	require.NoError(t, db.VerifyScopedReference(f.Params.View, signedClose, pins))
	scopedProjectorCaptureCheckpoint(t, capture, f.Params.View, broad, signers, pins)

	narrow, err := db.RestrictScopedState(broad, []string{projectA.UID}, nil)
	require.NoError(t, err)
	want, err := ScopedOracle(capture, []string{projectA.UID}, nil)
	require.NoError(t, err)
	require.Equal(t, scopedProjectorCaptureJSON(t, want), scopedProjectorCaptureJSON(t, narrow))
	require.Equal(t, []embedding.ArtifactManifest{currentArtifact.Manifest()}, narrow.Artifacts)
	nullClose := scopedProjectorCaptureReference(t, narrow, "close", issue.UID, "")
	require.False(t, nullClose.Present)
	require.Nil(t, nullClose.Close)
	require.Equal(t, closeRef.Slot, nullClose.Slot)
	subjectSigner := db.RootSigners{projectA.UID: signers[projectA.UID]}
	signedNull, err := db.SignScopedReference(f.Params.View, nullClose, subjectSigner)
	require.NoError(t, err)
	require.Len(t, signedNull.Signatures, 1)
	require.Equal(t, projectA.UID, signedNull.Signatures[0].ProjectUID)
	require.NoError(t, db.VerifyScopedReference(f.Params.View, signedNull, pins))
	_, err = db.SignScopedReference(f.Params.View, closeRef, subjectSigner)
	require.Error(t, err, "present cross-project disposition needs both enrolled keys")
	broadBytes := scopedProjectorCaptureJSON(t, broad)
	narrowBytes := scopedProjectorCaptureJSON(t, narrow)
	for _, marker := range []string{hiddenTitle, hiddenBody, peer.UID, hiddenArtifact.Digest} {
		require.Contains(t, string(broadBytes), marker, "broad fixture must contain the marker tested for non-disclosure")
		require.NotContains(t, string(narrowBytes), marker)
	}
	if sameProject {
		require.Len(t, narrow.Definitions, 2, "same-project job/workflow remain separate typed rows")
	} else {
		require.Len(t, narrow.Definitions, 1)
		require.Equal(t, "workflow", narrow.Definitions[0].Kind)
		for _, marker := range []string{jobName, jobBody, jobCreator} {
			require.Contains(t, string(broadBytes), marker)
			require.NotContains(t, string(narrowBytes), marker)
		}
	}
	issueOnly, err := db.RestrictScopedState(broad, []string{projectA.UID}, []string{issue.UID})
	require.NoError(t, err)
	require.Empty(t, issueOnly.Definitions, "issue authority never implies definition authority")
	issueOracle, err := ScopedOracle(capture, []string{projectA.UID}, []string{issue.UID})
	require.NoError(t, err)
	require.Equal(t, scopedProjectorCaptureJSON(t, issueOracle), scopedProjectorCaptureJSON(t, issueOnly))
}

type scopedProjectorCapturer interface {
	CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
}

func scopedProjectorNativeCapturer(t *testing.T, s db.Storage) scopedProjectorCapturer {
	t.Helper()
	native, ok := s.(scopedProjectorCapturer)
	require.True(t, ok, "real native CaptureScopedView required")
	return native
}

func assertScopedProjectorNoCapture(t *testing.T, s db.Storage, f ScopedCaptureFixture) {
	t.Helper()
	require.Equal(t, f.Params.View.AuthorityUID, s.InstanceUID())
	require.Equal(t, &f.Parent.ID, f.Grant.Enrollment.ParentTokenID)
	work := ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	require.Nil(t, work.CaptureUID)
	require.Nil(t, work.CapturedRevision)
	require.Nil(t, work.CaptureBytes)
	require.Nil(t, work.CaptureDigest)
}

func scopedProjectorCapturedCollections(t *testing.T, capture db.CanonicalViewState, view db.ScopedViewDescriptor) db.ScopedCollections {
	t.Helper()
	got, err := db.ProjectScopedState(capture, view)
	require.NoError(t, err)
	require.NoError(t, got.Validate())
	projects := make([]string, 0, len(view.Projects))
	for _, p := range view.Projects {
		projects = append(projects, p.ProjectUID)
	}
	want, err := ScopedOracle(capture, projects, nil)
	require.NoError(t, err)
	require.Equal(t, scopedProjectorCaptureJSON(t, want), scopedProjectorCaptureJSON(t, got))
	return got
}

func scopedProjectorCaptureKeys(t *testing.T, s db.Storage, f ScopedCaptureFixture) (db.RootSigners, []db.RootKeyPin) {
	t.Helper()
	signers := db.RootSigners{}
	pins := []db.RootKeyPin{}
	for _, p := range f.Params.View.Projects {
		pin, err := s.RootAuthority(t.Context(), p.ProjectUID)
		require.NoError(t, err)
		require.Equal(t, p.Pin, pin, "trusted native enrolled pin, not incoming signature metadata")
		history, err := s.RootKeyTransitions(t.Context(), p.ProjectUID)
		require.NoError(t, err)
		require.Empty(t, history, "this fresh native fixture does not rotate keys")
		pins = append(pins, pin)
		signers[p.ProjectUID] = ScopedCaptureSigner(t, f, p.ProjectUID)
	}
	return signers, pins
}

func scopedProjectorCaptureCheckpoint(t *testing.T, capture db.CanonicalViewState, view db.ScopedViewDescriptor, collections db.ScopedCollections, signers db.RootSigners, pins []db.RootKeyPin) db.ScopedCheckpoint {
	t.Helper()
	// Sequence is a candidate signing context only. No activation, publication,
	// materialization, completion or delivery acknowledgement occurs here.
	checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{
		View: view, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: collections, KeyHistory: []db.RootKeyTransition{},
	}, signers)
	require.NoError(t, err)
	require.NoError(t, db.VerifyScopedCheckpoint(checkpoint, view, pins))
	for _, reference := range checkpoint.Collections.References {
		require.NoError(t, db.VerifyScopedReference(view, reference, pins))
	}
	for _, provenance := range checkpoint.Collections.Provenance {
		require.NoError(t, db.VerifyScopedProvenance(view, provenance, pins))
	}
	return checkpoint
}

func scopedProjectorCaptureReference(t *testing.T, c db.ScopedCollections, kind, issueUID, entityUID string) db.ScopedReference {
	t.Helper()
	var matches []db.ScopedReference
	for _, r := range c.References {
		if r.Kind == kind && r.Subject.IssueUID == issueUID && r.SubjectEntityUID == entityUID {
			matches = append(matches, r)
		}
	}
	require.Len(t, matches, 1, "one current typed slot")
	return matches[0]
}

func scopedProjectorCaptureProvenance(t *testing.T, c db.ScopedCollections, project, kind, entity string) db.ScopedProvenance {
	t.Helper()
	var matches []db.ScopedProvenance
	for _, p := range c.Provenance {
		if p.ProjectUID == project && p.Kind == kind && p.EntityUID == entity {
			matches = append(matches, p)
		}
	}
	require.Len(t, matches, 1, "one exact project/kind/entity creation attribution")
	return matches[0]
}

func scopedProjectorCaptureJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	canonical := jsontext.Value(raw)
	require.NoError(t, canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)))
	return canonical
}
