package dbtest

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// repeatedImportHub is one hub project that already holds a first import of a
// spoke project, plus the snapshot events that import pushed.
type repeatedImportHub struct {
	hub       db.Storage
	target    db.Project
	first     []db.Event
	firstUID  string
	firstID   int64
	issueUID  string
	otherUID  string
	commentAt time.Time
}

func newRepeatedImportHub(t *testing.T, source db.Storage, backend Backend) repeatedImportHub {
	t.Helper()
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	owner := "bob"
	priority := int64(2)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "First import", Body: "old body", Author: "alice",
		Labels: []string{"bug", "stale"}, Owner: &owner, Priority: &priority,
	})
	require.NoError(t, err)
	other, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Untouched", Author: "alice"})
	require.NoError(t, err)
	comment, _, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "alice", Body: "first"})
	require.NoError(t, err)

	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })
	target, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, target.ID, "operator")
	require.NoError(t, err)
	enrollment, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: source.InstanceUID(), ProjectID: &target.ID, Actor: "operator",
		Capabilities: "pull,push", AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	adopted, err := source.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: target.ID,
		HubProjectUID: target.UID, Actor: "operator",
	})
	require.NoError(t, err)
	events, err := source.PendingFederationPushEvents(ctx, project.ID, source.InstanceUID(), adopted.Binding.PushCursorEventID, 100)
	require.NoError(t, err)
	batch := make([]db.RemoteEvent, 0, len(events))
	for _, event := range events {
		batch = append(batch, portableNativeEvent(event))
	}
	result, err := pushRepeatedImportBaseline(t, hub, target.ID, enrollment.Enrollment.ID, source.InstanceUID(), batch)
	require.NoError(t, err)
	require.Equal(t, len(events), result.Accepted)
	return repeatedImportHub{
		hub: hub, target: target, first: events, firstUID: source.InstanceUID(),
		firstID: enrollment.Enrollment.ID, issueUID: issue.UID, otherUID: other.UID,
		commentAt: comment.CreatedAt,
	}
}

// pushRepeatedImportBaseline sends events as one complete adoption baseline,
// the shape a migrate or adoption push uses.
func pushRepeatedImportBaseline(t *testing.T, hub db.Storage, projectID, enrollmentID int64, spokeUID string, events []db.RemoteEvent) (db.FederationIngestResult, error) {
	t.Helper()
	batch := make([]db.FederationIngestEvent, 0, len(events))
	for index, event := range events {
		batch = append(batch, db.FederationIngestEvent{SourceEventID: int64(index + 1), Event: event})
	}
	return hub.IngestFederationEvents(t.Context(), db.FederationIngestParams{
		EventFeatures: db.CronEventFeature, ProjectID: projectID, FederationEnrollmentID: enrollmentID,
		SpokeInstanceUID: spokeUID, BoundActor: "operator", AllowSnapshotAuthorPreservation: true,
		AdoptionBaseline:                 db.FederationAdoptionBaselineComplete,
		AdoptionBaselineEndSourceEventID: int64(len(batch)), Events: batch,
	})
}

// repeatedImportCopy builds the adoption snapshots another Kata home would
// push for its own copy of the same project. edit changes each issue's
// snapshot payload; the events get that home's origin, fresh UIDs and a later
// clock. It also enrolls the home on the hub project.
func (h repeatedImportHub) repeatedImportCopy(t *testing.T, target db.Project, edit func(issueUID string, payload map[string]any)) (string, int64, []db.RemoteEvent) {
	t.Helper()
	origin, err := uid.New()
	require.NoError(t, err)
	enrollment, err := h.hub.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: origin, ProjectID: &target.ID, Actor: "operator",
		Capabilities: "pull,push", AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	copied := make([]db.RemoteEvent, 0, len(h.first))
	for _, source := range h.first {
		if source.Type != "issue.snapshot" {
			continue
		}
		event := portableNativeEvent(source)
		event.EventUID, err = uid.New()
		require.NoError(t, err)
		event.OriginInstanceUID = origin
		event.ProjectUID = target.UID
		event.ProjectName = target.Name
		event.HLCPhysicalMS += 60_000
		event.CreatedAt = event.CreatedAt.Add(time.Minute)
		payload := map[string]any{}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		edit(*event.IssueUID, payload)
		event.Payload = mustNativeJSON(t, payload)
		resignNativeEvent(t, &event)
		copied = append(copied, event)
	}
	return origin, enrollment.Enrollment.ID, copied
}

func repeatedImportShiftUpdatedAt(t *testing.T, payload map[string]any, delta time.Duration) {
	t.Helper()
	current, err := time.Parse(time.RFC3339Nano, payload["updated_at"].(string))
	require.NoError(t, err)
	payload["updated_at"] = current.Add(delta).UTC().Format(db.EventTimestampFormat)
}

func repeatedImportLabels(t *testing.T, hub db.Storage, issueID int64) []string {
	t.Helper()
	rows, err := hub.LabelsByIssue(t.Context(), issueID)
	require.NoError(t, err)
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		labels = append(labels, row.Label)
	}
	return labels
}

// A hub accepts a repeated adoption or migrate push for issues it already
// holds: newer copies update the issue, older and identical copies change
// nothing, and comments merge by UID with their original author and time.
func checkFederationRepeatedImport(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	h := newRepeatedImportHub(t, source, backend)
	hub := h.hub

	laterComment := h.commentAt.Add(2 * time.Hour).UTC().Format(db.EventTimestampFormat)
	newIssueUID, err := uid.New()
	require.NoError(t, err)
	newerOrigin, newerEnrollment, newer := h.repeatedImportCopy(t, h.target, func(issueUID string, payload map[string]any) {
		if issueUID != h.issueUID {
			return
		}
		repeatedImportShiftUpdatedAt(t, payload, 24*time.Hour)
		payload["title"] = "Current copy"
		payload["body"] = "new body"
		payload["status"] = "closed"
		payload["closed_reason"] = "done"
		payload["closed_at"] = payload["updated_at"]
		delete(payload, "owner")
		delete(payload, "priority")
		payload["labels"] = []string{"bug", "shipped"}
		payload["metadata"] = map[string]any{"team": "core"}
		comments := payload["comments"].([]any)
		// The current copy re-saved the known comment without changing its text.
		require.NotEmpty(t, comments)
		known, ok := comments[0].(map[string]any)
		require.True(t, ok)
		known["edited_at"] = laterComment
		payload["comments"] = append(comments, map[string]any{
			"comment_uid": "01ARZ3NDEKTSV4RRFFQ69G5FC2", "author": "carol",
			"body": "second", "created_at": laterComment,
		})
	})
	// The newer copy also has a task the first import never saw.
	require.NotEmpty(t, newer)
	extra := newer[0]
	extra.EventUID, err = uid.New()
	require.NoError(t, err)
	extra.IssueUID = &newIssueUID
	extra.Payload = mustNativeJSON(t, map[string]any{
		"uid": newIssueUID, "title": "Only in the current copy", "body": "", "author": "carol",
		"status": "open", "created_at": laterComment, "updated_at": laterComment,
	})
	resignNativeEvent(t, &extra)
	newer = append(newer, extra)

	result, err := pushRepeatedImportBaseline(t, hub, h.target.ID, newerEnrollment, newerOrigin, newer)
	require.NoError(t, err, "a repeated import of known issues is accepted")
	require.Equal(t, len(newer), result.Accepted)

	issue, err := hub.IssueByUID(ctx, h.issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Current copy", issue.Title)
	require.Equal(t, "new body", issue.Body)
	require.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	require.Equal(t, "done", *issue.ClosedReason)
	require.NotNil(t, issue.ClosedAt)
	require.Nil(t, issue.Owner)
	require.Nil(t, issue.Priority)
	require.JSONEq(t, `{"team":"core"}`, string(issue.Metadata))
	require.ElementsMatch(t, []string{"bug", "shipped"}, repeatedImportLabels(t, hub, issue.ID))
	comments, err := hub.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	require.Equal(t, "alice", comments[0].Author)
	require.Equal(t, "first", comments[0].Body)
	require.True(t, h.commentAt.Equal(comments[0].CreatedAt), "a known comment keeps its created time")
	require.NotNil(t, comments[0].EditedAt, "a newer copy's edit time is stored even when the text is unchanged")
	require.Equal(t, laterComment, comments[0].EditedAt.UTC().Format(db.EventTimestampFormat))
	require.Equal(t, "carol", comments[1].Author)
	require.Equal(t, laterComment, comments[1].CreatedAt.UTC().Format(db.EventTimestampFormat))
	added, err := hub.IssueByUID(ctx, newIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Only in the current copy", added.Title)
	untouched, err := hub.IssueByUID(ctx, h.otherUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Untouched", untouched.Title)

	// The same push again is a no-op that still succeeds.
	result, err = pushRepeatedImportBaseline(t, hub, h.target.ID, newerEnrollment, newerOrigin, newer)
	require.NoError(t, err)
	require.Zero(t, result.Accepted)
	require.Equal(t, len(newer), result.Duplicates)
	again, err := hub.IssueByUID(ctx, h.issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, issue.Revision, again.Revision)

	// An older copy must not overwrite the newer hub state.
	olderOrigin, olderEnrollment, older := h.repeatedImportCopy(t, h.target, func(_ string, payload map[string]any) {
		repeatedImportShiftUpdatedAt(t, payload, -time.Hour)
		payload["title"] = "Stale copy"
		payload["labels"] = []string{"ancient"}
	})
	_, err = pushRepeatedImportBaseline(t, hub, h.target.ID, olderEnrollment, olderOrigin, older)
	require.NoError(t, err, "an older copy is accepted and skipped")
	kept, err := hub.IssueByUID(ctx, h.issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Current copy", kept.Title)
	require.Equal(t, "closed", kept.Status)
	require.ElementsMatch(t, []string{"bug", "shipped"}, repeatedImportLabels(t, hub, kept.ID))
	untouched, err = hub.IssueByUID(ctx, h.otherUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Untouched", untouched.Title)

	// Outside an adoption baseline the create-only guard still applies.
	for _, eventType := range []string{"issue.snapshot", "issue.created"} {
		fresh := portableNativeEvent(h.first[0])
		for _, event := range h.first {
			if event.Type == "issue.snapshot" && *event.IssueUID == h.issueUID {
				fresh = portableNativeEvent(event)
			}
		}
		fresh.EventUID, err = uid.New()
		require.NoError(t, err)
		fresh.Type = eventType
		fresh.HLCPhysicalMS += 120_000
		// Use the bound actor throughout so the event reaches the guard.
		payload := map[string]any{}
		require.NoError(t, json.Unmarshal(fresh.Payload, &payload))
		payload["author"] = "operator"
		delete(payload, "comments")
		fresh.Payload = mustNativeJSON(t, payload)
		resignNativeEvent(t, &fresh)
		_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
			EventFeatures: db.CronEventFeature, ProjectID: h.target.ID, FederationEnrollmentID: h.firstID,
			SpokeInstanceUID: h.firstUID, BoundActor: "operator",
			Events: []db.FederationIngestEvent{{SourceEventID: 1000, Event: fresh}},
		})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		require.ErrorContains(t, err, "fresh "+eventType+" targets existing issue "+h.issueUID)
	}
	return nil
}

// An issue UID is unique across the hub. A snapshot for a UID that another
// project holds is refused with a validation error that names the conflict,
// including when that project is archived.
func checkFederationImportIssueHeldElsewhere(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	h := newRepeatedImportHub(t, source, backend)
	hub := h.hub
	replacement, err := hub.CreateProject(ctx, "replacement-project")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, replacement.ID, "operator")
	require.NoError(t, err)

	for _, archived := range []bool{false, true} {
		if archived {
			_, _, err = hub.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: h.target.ID, Actor: "operator", Force: true})
			require.NoError(t, err)
		}
		origin, enrollment, copied := h.repeatedImportCopy(t, replacement, func(string, map[string]any) {})
		_, err = pushRepeatedImportBaseline(t, hub, replacement.ID, enrollment, origin, copied)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, "archived=%v", archived)
		require.NotErrorIs(t, err, db.ErrNotFound, "archived=%v", archived)
		require.ErrorContains(t, err, "another project on this hub")
		var named bool
		for _, issueUID := range []string{h.issueUID, h.otherUID} {
			named = named || strings.Contains(err.Error(), issueUID)
		}
		require.True(t, named, "the error names the conflicting issue: %v", err)
		_, lookupErr := hub.IssueByUID(ctx, h.issueUID, db.IncludeDeletedYes)
		require.NoError(t, lookupErr, "the refused push leaves the held issue in place")
	}
	return nil
}
