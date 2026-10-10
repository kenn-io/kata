package sqlitestore

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

func incrementalTestStore(t testing.TB) (*Store, db.Project, db.Issue) {
	t.Helper()
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	p, err := d.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "work", Author: "tester"})
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)
	return d, p, issue
}

func incrementalRemoteEvent(t testing.TB, p db.Project, issueUID, typ, payload string, clock int64) db.RemoteEvent {
	t.Helper()
	eventUID, err := katauid.New()
	require.NoError(t, err)
	ev := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EZ", ProjectUID: p.UID, ProjectName: p.Name,
		Type: typ, Actor: "tester", Payload: []byte(payload), HLCPhysicalMS: clock, CreatedAt: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)}
	if issueUID != "" {
		ev.IssueUID = &issueUID
	}
	hash, err := db.EventContentHash(db.EventHashInput{UID: ev.EventUID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: p.UID, ProjectName: p.Name,
		IssueUID: ev.IssueUID, Type: typ, Actor: ev.Actor, Payload: ev.Payload, HLCPhysicalMS: clock, CreatedAt: ev.CreatedAt.Format(sqliteTimeFormat)})
	require.NoError(t, err)
	ev.ContentHash = hash
	return ev
}

func TestFederationMaterializationApplyRejectsNilReceiver(t *testing.T) {
	var materialization *federationMaterialization

	err := materialization.apply(context.Background(), nil, 1, nil)

	require.ErrorContains(t, err, "nil federation materialization")
}

// A competing commit after preparation must succeed before ingestion acquires
// the writer lock, then force ingestion to retry its entire stale snapshot.
func TestFederationIngestRetriesPreparedSnapshot(t *testing.T) {
	t.Parallel()
	d, p, issue := incrementalTestStore(t)
	ctx := context.Background()
	enrollment, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: "test-enrollment-token", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EZ", ProjectID: &p.ID, Capabilities: "pull,push", Actor: "tester"})
	require.NoError(t, err)
	ctx = db.WithTransactionFence(ctx, d.FederationEnrollmentTransactionFence(enrollment.Enrollment, p.ID, "push"))
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts != 1 {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err := d.PatchProjectMetadata(writeCtx, db.PatchProjectMetadataIn{ProjectID: p.ID, Actor: "tester", Patch: map[string]jsontext.Value{"retained": []byte(`true`)}})
		require.NoError(t, err, "fold preparation must not hold the SQLite writer")
	}
	ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_000_000_000)
	result, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}}})
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	require.Equal(t, 2, attempts)
	project, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"retained":true}`, string(project.Metadata))
}

func TestFederationPreparationExcludesUntouchedRows(t *testing.T) {
	t.Parallel()
	d, p, issue := incrementalTestStore(t)
	ctx := context.Background()
	other, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "untouched", Author: "tester"})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "tester", Body: "retained"})
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, other.ID, "retained", "tester")
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_000_000_000)
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	m, err := prepareFederationMaterializationTx(ctx, tx, p.ID, []preparedFederationIngestEvent{{Event: ev}})
	require.NoError(t, err)
	require.Len(t, m.projection.Issues, 1, "apply must visit changed issues only")
	require.Contains(t, m.projection.Issues, issue.UID)
	require.Empty(t, m.projection.Comments, "apply must not visit untouched comments")
	require.Empty(t, m.projection.Labels, "apply must not visit untouched labels")
}

func TestFederationStatusIntentUsesPayloadIssueUIDWhenEnvelopeOmitsIt(t *testing.T) {
	testFederationStatusIntentUsesPayloadIdentityWhenEnvelopeOmitsIt(t, "issue_uid")
}

func TestFederationStatusIntentUsesPayloadUIDWhenEnvelopeOmitsIt(t *testing.T) {
	testFederationStatusIntentUsesPayloadIdentityWhenEnvelopeOmitsIt(t, "uid")
}

func testFederationStatusIntentUsesPayloadIdentityWhenEnvelopeOmitsIt(t *testing.T, payloadKey string) {
	t.Parallel()
	d, p, issue := incrementalTestStore(t)
	ctx := context.Background()
	_, err := d.Exec(`INSERT INTO issue_sync_bindings(project_id,provider,source_key,remote_id,display_name,config_json,interval_seconds)
VALUES(?,'example','example:remote','remote','Remote','{"status_sync":"two-way"}',60)`, p.ID)
	require.NoError(t, err)
	res, err := d.Exec(`INSERT INTO import_mappings(source,external_id,object_type,project_id,issue_id)
VALUES('example:remote','remote-issue','issue',?,?)`, p.ID, issue.ID)
	require.NoError(t, err)
	mappingID, err := res.LastInsertId()
	require.NoError(t, err)

	ev := incrementalRemoteEvent(t, p, "", "issue.closed",
		`{"`+payloadKey+`":"`+issue.UID+`","reason":"done","closed_at":"2026-05-23T12:00:00.000Z"}`, 9_000_000_000_000)
	result, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	var status string
	require.NoError(t, d.QueryRow(`SELECT status FROM issues WHERE id=?`, issue.ID).Scan(&status))
	require.Equal(t, "closed", status, "the payload UID must identify the issue being materialized")

	var pending string
	require.NoError(t, d.QueryRow(`SELECT COALESCE(pending_event_uid,'') FROM import_mappings WHERE id=?`, mappingID).Scan(&pending))
	require.Equal(t, ev.EventUID, pending, "accepted status event should enqueue intent for its payload issue")

	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	binding, err := issueSyncBindingByProject(ctx, tx, p.ID)
	require.NoError(t, err)
	loaded, err := loadIssueStatusMappingTx(ctx, tx, binding, mappingID)
	require.NoError(t, err, "payload issue UID should validate a pending event with no envelope UID")
	require.NotNil(t, loaded.PendingEvent)
	require.Equal(t, ev.EventUID, loaded.PendingEvent.UID)
}

func TestFederationPreparationExcludesUntouchedLinks(t *testing.T) {
	t.Parallel()
	d, p, issue := incrementalTestStore(t)
	ctx := context.Background()
	other, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "peer", Author: "tester"})
	require.NoError(t, err)
	_, _, err = d.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: other.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: other.ShortID, ToUID: other.UID, Actor: "tester"})
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, issue.UID, "issue.soft_deleted", `{"issue_uid":"`+issue.UID+`","deleted_at":"2026-05-23T12:00:00.000Z"}`, 9_000_000_000_000)
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	m, err := prepareFederationMaterializationTx(ctx, tx, p.ID, []preparedFederationIngestEvent{{Event: ev}})
	require.NoError(t, err)
	require.Empty(t, m.links.Links, "apply must visit changed links only")
}

// The reference deliberately uses the internal materializer with accepted UIDs:
// the public recovery API does not enqueue newly accepted status intent.
func ingestFullRebuildReference(ctx context.Context, d *Store, p db.FederationIngestParams) (db.FederationIngestResult, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result := db.FederationIngestResult{}
	links := false
	validator := db.NewCronReplayValidator(tx, false)
	for _, in := range p.Events {
		inserted, err := insertFederationEventTx(ctx, tx, p.ProjectID, in.Event.ProjectName, in.Event, validator)
		if err != nil {
			return result, err
		}
		result.PushCursorEventID = max(result.PushCursorEventID, in.SourceEventID)
		if !inserted {
			result.Duplicates++
			continue
		}
		audit, err := d.annotateFederationIngestClaimWorkTx(ctx, tx, p.ProjectID, in.Event)
		if err != nil {
			return result, err
		}
		result.Accepted++
		result.InsertedEventUIDs = append(result.InsertedEventUIDs, in.Event.EventUID)
		for _, ev := range audit {
			result.InsertedEventUIDs = append(result.InsertedEventUIDs, ev.UID)
		}
		links = links || db.FederationEventAffectsLinks(in.Event.Type)
	}
	if result.Accepted > 0 {
		if err := d.materializeFederatedProjectTx(ctx, tx, p.ProjectID, links, result.InsertedEventUIDs, validator); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

func cloneIncrementalStore(t testing.TB, d *Store) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reference.db")
	_, err := d.Exec(`VACUUM INTO ?`, path)
	require.NoError(t, err)
	reference, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reference.Close()) })
	return reference
}

func federationProjectionRows(t testing.TB, d *Store, projectID int64) [][][]any {
	t.Helper()
	ctx := context.Background()
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, projectID))
	require.NoError(t, err)
	ids, err := federationBindingGroupProjectIDs(ctx, tx, binding)
	require.NoError(t, err)
	links, err := federationGroupFoldProjection(ctx, tx, ids)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	queries := []string{
		`SELECT uid,short_id,title,body,status,closed_reason,owner,CAST(assignment_expires_on AS TEXT),priority,author,metadata,revision,content_revision,CAST(created_at AS TEXT),CAST(updated_at AS TEXT),CAST(closed_at AS TEXT),CAST(deleted_at AS TEXT) FROM issues WHERE project_id=? ORDER BY uid`,
		`SELECT c.uid,i.uid,c.author,c.teammate,c.body,CAST(c.created_at AS TEXT) FROM comments c JOIN issues i ON i.id=c.issue_id WHERE i.project_id=? ORDER BY c.uid`,
		`SELECT i.uid,l.label,l.author FROM issue_labels l JOIN issues i ON i.id=l.issue_id WHERE i.project_id=? ORDER BY i.uid,l.label`,
		`SELECT f.uid,t.uid,l.type,l.author,CAST(l.created_at AS TEXT) FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.project_id=? OR t.project_id=? ORDER BY f.uid,t.uid,l.type`,
		`SELECT metadata,revision FROM projects WHERE id=?`,
		`SELECT m.external_id,i.uid,m.pending_event_uid FROM import_mappings m JOIN issues i ON i.id=m.issue_id WHERE m.project_id=? ORDER BY m.external_id`,
	}
	var out [][][]any
	for tableIndex, query := range queries {
		args := []any{projectID}
		if strings.Contains(query, "OR t.project_id") {
			args = append(args, projectID)
		}
		rows, err := d.Query(query, args...)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		var table [][]any
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			require.NoError(t, rows.Scan(targets...))
			if tableIndex == 3 {
				key := canonicalFederatedLinkKey(db.FoldLinkKey{FromUID: values[0].(string), ToUID: values[1].(string), Type: values[2].(string)})
				// Explicit snapshot dates remain exact. SQL-generated insertion times
				// naturally differ between independent database transactions.
				if links.Links[key].CreatedAt == "" {
					values[4] = "database-insertion-time"
				}
			}
			table = append(table, values)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		out = append(out, table)
	}
	for _, term := range []string{"work", "snapshot", "baseline", "comment", "edited"} {
		rows, err := d.Query(`SELECT i.uid FROM issues_fts f JOIN issues i ON i.id=f.rowid WHERE issues_fts MATCH ? AND i.project_id=? ORDER BY i.uid`, term, projectID)
		require.NoError(t, err)
		var matches [][]any
		for rows.Next() {
			var uid string
			require.NoError(t, rows.Scan(&uid))
			matches = append(matches, []any{uid})
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		out = append(out, matches)
	}
	return out
}

func assertIncrementalBatch(t testing.TB, d, reference *Store, p db.FederationIngestParams) {
	t.Helper()
	got, err := d.IngestFederationEvents(context.Background(), p)
	want, referenceErr := ingestFullRebuildReference(context.Background(), reference, p)
	require.NoError(t, err)
	require.NoError(t, referenceErr)
	require.Equal(t, want.Accepted, got.Accepted)
	require.Equal(t, want.Duplicates, got.Duplicates)
	require.Equal(t, want.PushCursorEventID, got.PushCursorEventID)
	require.Equal(t, federationProjectionRows(t, reference, p.ProjectID), federationProjectionRows(t, d, p.ProjectID))
}

func FuzzFederationIngestMatchesFullRebuild(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 1, 0, 9, 1, 2, 0, 2, 2, 3, 3, 7, 3})
	var all []byte
	for i := range 24 {
		value := byte(i)
		all = append(all, value, value%4, value*3, value%3)
	}
	f.Add(all)
	f.Fuzz(checkFederationSequence)
}

func TestFederationIncrementalRandomSequences(t *testing.T) {
	//nolint:gosec // A fixed seed makes the test inputs repeatable; they are not used for security.
	random := rand.New(rand.NewPCG(1, 2))
	for sequence := range 20 {
		data := make([]byte, 64)
		for i := range data {
			data[i] = byte(random.Uint32() & 0xff)
		}
		t.Run(fmt.Sprintf("sequence-%02d", sequence), func(t *testing.T) { checkFederationSequence(t, data) })
	}
}

func checkFederationSequence(t *testing.T, data []byte) {
	if len(data) > 128 {
		data = data[:128]
	}
	d, p, initial := incrementalTestStore(t)
	// Exercise status-intent pointers on one mapped issue in every run.
	_, err := d.Exec(`INSERT INTO issue_sync_bindings(project_id,provider,source_key,remote_id,display_name,config_json,interval_seconds) VALUES(?,'example','example','example','Example','{"status_sync":"two-way"}',60)`, p.ID)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO import_mappings(source,external_id,object_type,project_id,issue_id) VALUES('example','mapped','issue',?,?)`, p.ID, initial.ID)
	require.NoError(t, err)
	reference := cloneIncrementalStore(t, d)
	uids := []string{initial.UID, "01HZNQ7VFPK1XGD8R5MABC0001", "01HZNQ7VFPK1XGD8R5MABC0002", "01HZNQ7VFPK1XGD8R5MABC0003"}
	var baseline []db.FederationIngestEvent
	for i, uid := range uids[1:] {
		payload := fmt.Sprintf(`{"uid":%q,"title":"snapshot","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z","metadata":{},"comments":[{"comment_uid":%q,"author":"tester","body":"baseline","created_at":"2026-05-23T12:00:00.000Z"}],"labels":["baseline"]}`, uid, "01HZNQ7VFPK1XGD8R5MABD000"+strconv.Itoa(i+1))
		baseline = append(baseline, db.FederationIngestEvent{SourceEventID: int64(i + 1), Event: incrementalRemoteEvent(t, p, uid, "issue.snapshot", payload, 9_000_000_000_000)})
	}
	params := db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: baseline[0].Event.OriginInstanceUID, Events: baseline}
	assertIncrementalBatch(t, d, reference, params)
	var pending []db.FederationIngestEvent
	history := append([]db.FederationIngestEvent(nil), baseline...)
	for pos := 0; pos+3 < len(data); pos += 4 {
		uid := uids[int(data[pos+1])%len(uids)]
		peer := uids[(int(data[pos+1])+1)%len(uids)]
		typ, payload := incrementalFuzzPayload(data[pos], uid, peer, int(data[pos+2]))
		envelope := uid
		if typ == "project.metadata_updated" || data[pos+3]&8 != 0 {
			envelope = ""
		}
		ev := incrementalRemoteEvent(t, p, envelope, typ, payload, 9_000_000_000_100+int64(data[pos+2]%40))
		if data[pos+3]&16 != 0 {
			ev = history[int(data[pos+2])%len(history)].Event
		}
		in := db.FederationIngestEvent{SourceEventID: int64(pos + 10), Event: ev}
		history = append(history, in)
		pending = append(pending, in)
		if len(pending) >= int(data[pos+3]%3)+1 || pos+7 >= len(data) {
			params.Events = pending
			assertIncrementalBatch(t, d, reference, params)
			pending = nil
		}
	}
}

func incrementalFuzzPayload(kind byte, uid, peer string, value int) (string, string) {
	prefix := `"issue_uid":"` + uid + `",`
	stamp := `"2026-05-23T12:00:00.000Z"`
	comment := `"comment_uid":"01HZNQ7VFPK1XGD8R5MABE` + uid[len(uid)-4:] + `",`
	switch kind % 24 {
	case 0:
		return "issue.updated", `{` + prefix + fmt.Sprintf(`"title":"work %d","body":"body λ %d"}`, value, value)
	case 1:
		return "issue.closed", `{` + prefix + `"reason":"done","closed_at":` + stamp + `}`
	case 2:
		return "issue.reopened", `{` + prefix + `"reopened_at":` + stamp + `}`
	case 3:
		return "issue.soft_deleted", `{` + prefix + `"deleted_at":` + stamp + `}`
	case 4:
		return "issue.restored", `{` + prefix + `"restored_at":` + stamp + `}`
	case 5:
		return "issue.commented", `{` + prefix + comment + `"author":"tester","teammate":"worker-a","body":"comment","created_at":` + stamp + `}`
	case 6:
		return "issue.comment_edited", `{` + prefix + comment + `"body":"edited comment"}`
	case 7:
		return "issue.labeled", `{` + prefix + `"label":"area:db"}`
	case 8:
		return "issue.unlabeled", `{` + prefix + `"label":"area:db"}`
	case 9:
		return "issue.assigned", `{` + prefix + `"owner":"worker-a"}`
	case 10:
		return "issue.unassigned", `{` + prefix + `"owner":null}`
	case 11:
		return "issue.priority_set", `{` + prefix + `"priority":2}`
	case 12:
		return "issue.priority_cleared", `{` + prefix + `"old_priority":2}`
	case 13:
		return "issue.metadata_updated", `{` + prefix + fmt.Sprintf(`"diff":{"area":{"from":null,"to":"value-%d"}}}`, value)
	case 14:
		return "project.metadata_updated", fmt.Sprintf(`{"diff":{"area":{"from":null,"to":"value-%d"}}}`, value)
	case 15:
		return "issue.linked", `{` + prefix + `"from_uid":"` + uid + `","to_uid":"` + peer + `","type":"related","created_at":` + stamp + `}`
	case 16:
		return "issue.unlinked", `{` + prefix + `"from_uid":"` + uid + `","to_uid":"` + peer + `","type":"related"}`
	case 17:
		return "issue.links_changed", `{` + prefix + `"related_added_uids":["` + peer + `"]}`
	case 18:
		return "issue.links_changed", `{` + prefix + `"related_removed_uids":["` + peer + `"]}`
	case 19:
		return "issue.assignment_renewed", `{` + prefix + `"owner":"worker-a","assignment_expires_on":` + stamp + `}`
	case 20:
		return "issue.assignment_expired", `{` + prefix + `"owner":null,"previous_owner":"worker-a","assignment_expires_on":` + stamp + `}`
	case 21:
		return "issue.updated", `{` + prefix + `"status":"closed","closed_reason":"done","closed_at":` + stamp + `}`
	case 22:
		return "issue.updated", `{` + prefix + `"created_at":"2025-01-01T00:00:00.000Z"}`
	default:
		return "issue.external_field_resolved", `{` + prefix + `"field":"status"}`
	}
}

func TestFederationIncrementalNewEndpointsAndShortIDCollisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, p, _ := incrementalTestStore(t)
	peer, err := d.CreateProject(ctx, "peer-project")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, peer.ID, "tester")
	require.NoError(t, err)
	reference := cloneIncrementalStore(t, d)
	first := "01HZNQ7VFPK1XGD8R5MABC0001"
	second := "01HZNQ7VFPK1XGD8R5MABD0001"
	third := "01HZNQ7VFPK1XGD8R5MABE0001"
	makeSnapshot := func(project db.Project, uid, target string) db.RemoteEvent {
		links := "[]"
		if target != "" {
			links = fmt.Sprintf(`[{"type":"related","to_issue_uid":%q,"author":"tester","created_at":"2020-01-01T00:00:00.000Z"}]`, target)
		}
		payload := fmt.Sprintf(`{"uid":%q,"short_id":"0001","title":"snapshot","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z","links":%s}`, uid, links)
		return incrementalRemoteEvent(t, project, uid, "issue.snapshot", payload, 9_000_000_000_000)
	}
	events := []db.FederationIngestEvent{{SourceEventID: 1, Event: makeSnapshot(p, first, second)}, {SourceEventID: 2, Event: makeSnapshot(p, second, third)}}
	params := db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: events[0].Event.OriginInstanceUID, Events: events}
	assertIncrementalBatch(t, d, reference, params)
	params.ProjectID = peer.ID
	params.Events = []db.FederationIngestEvent{{SourceEventID: 3, Event: makeSnapshot(peer, third, "")}}
	assertIncrementalBatch(t, d, reference, params)
	require.Equal(t, federationProjectionRows(t, reference, p.ID), federationProjectionRows(t, d, p.ID))
	var shortIDs []string
	rows, err := d.Query(`SELECT short_id FROM issues WHERE uid IN (?,?) ORDER BY uid`, first, second)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		shortIDs = append(shortIDs, id)
	}
	require.NoError(t, rows.Close())
	require.Equal(t, "0001", shortIDs[0])
	require.Greater(t, len(shortIDs[1]), 4)
	var links int
	require.NoError(t, d.QueryRow(`SELECT count(*) FROM links`).Scan(&links))
	require.Equal(t, 2, links)
}
