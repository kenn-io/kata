package sqlitestore

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Cron state is independent of issue state. A cron-containing push or pull
// must not prepare unrelated issue rows just because it also has an issue delta.
func TestCronPreparationBoundsIssueRows(t *testing.T) {
	for _, pull := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			for _, history := range []int{100, 10_000} {
				t.Run(fmt.Sprintf("pull=%t/mixed=%t/history=%d", pull, mixed, history), func(t *testing.T) {
					d, p, issue := incrementalTestStore(t)
					seedCronPullHistory(t, d, p, history)
					events := []db.RemoteEvent{cronPullWorkflow(t, p, "workflow", 9_000_001_000_001)}
					want := 0
					if mixed {
						events = append(events, incrementalRemoteEvent(t, p, issue.UID, "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":"mixed changed"}`, issue.UID), 9_000_001_000_002))
						want = 1
					}
					var uids []string
					prepared := make([]preparedFederationIngestEvent, 0, len(events))
					for _, ev := range events {
						uids = append(uids, ev.EventUID)
						prepared = append(prepared, preparedFederationIngestEvent{Event: ev})
					}
					if pull {
						_, err := d.InsertRemoteEvents(t.Context(), p.ID, events)
						require.NoError(t, err)
					}
					tx, err := d.BeginTx(t.Context(), nil)
					require.NoError(t, err)
					defer func() { _ = tx.Rollback() }()
					var m federationMaterialization
					if pull {
						m, err = preparePulledFederationMaterialization(t.Context(), tx, p.ID, uids, pullPrepareComplete)
					} else {
						m, err = prepareFederationMaterializationTx(t.Context(), tx, p.ID, prepared)
					}
					require.NoError(t, err)
					require.False(t, m.fullReplay)
					require.Equal(t, want, len(m.issueRows), "cron must not widen issue row preparation")
					require.Len(t, m.projection.CronWorkflows, 1)
				})
			}
		}
	}
}

func TestCronMixedMaterializationMatchesFullFold(t *testing.T) {
	for _, pull := range []bool{false, true} {
		for _, scenario := range []string{"newer", "older", "snapshot", "reset", "project-wide", "retained-move", "observation", "links"} {
			t.Run(fmt.Sprintf("pull=%t/%s", pull, scenario), func(t *testing.T) {
				ctx := t.Context()
				d, p, issue := incrementalTestStore(t)
				other, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "untouched", Author: "tester", Labels: []string{"retained"}})
				require.NoError(t, err)
				_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "tester", Body: "retained comment"})
				require.NoError(t, err)
				first := cronPullWorkflow(t, p, "retained winner", 9_000_001_000_100)
				baseline := []db.RemoteEvent{first}
				if scenario == "snapshot" {
					first = cronPullWorkflow(t, p, "created", 9_000_001_000_100)
					edit := incrementalRemoteEvent(t, p, "", "cron.workflow.updated", string(cronPullWorkflow(t, p, "retained winner", 9_000_001_000_101).Payload), 9_000_001_000_101)
					baseline = []db.RemoteEvent{first, edit}
				}
				_, err = d.InsertRemoteEvents(ctx, p.ID, baseline)
				require.NoError(t, err)
				require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
				var movedProject db.Project
				var localAlias string
				if scenario == "retained-move" {
					movedProject, err = d.CreateProject(ctx, "move-peer")
					require.NoError(t, err)
					_, err = d.EnableProjectFederation(ctx, movedProject.ID, "tester")
					require.NoError(t, err)
					current, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					away, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: p.ID, ToProjectID: movedProject.ID, IfMatchRev: current.Revision, Actor: "tester"})
					require.NoError(t, err)
					back, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: away.Issue.ID, FromProjectID: movedProject.ID, ToProjectID: p.ID, IfMatchRev: away.Issue.Revision, Actor: "tester"})
					require.NoError(t, err)
					localAlias = strings.ToLower(issue.UID[len(issue.UID)-8:])
					var moveUID string
					require.NoError(t, d.QueryRowContext(ctx, `SELECT uid FROM events WHERE type='issue.moved' AND issue_uid=? ORDER BY id DESC LIMIT 1`, issue.UID).Scan(&moveUID))
					tx, err := d.BeginTx(ctx, nil)
					require.NoError(t, err)
					_, err = d.insertEventTx(ctx, tx, eventInsert{ProjectID: movedProject.ID, ProjectUID: movedProject.UID, ProjectName: movedProject.Name, IssueUID: &issue.UID, Type: db.ProjectScopeResetEventType, Actor: "system", Payload: fmt.Sprintf(`{"from_project_uid":%q,"from_short_id":%q,"move_event_uid":%q}`, movedProject.UID, localAlias, moveUID), SkipRelay: true})
					require.NoError(t, err)
					require.NoError(t, tx.Commit())
					require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
					require.Equal(t, p.ID, back.Issue.ProjectID)
				}
				if scenario == "reset" {
					require.NoError(t, d.ResetFederatedProject(ctx, p.ID, 1, 1))
				}
				clock := int64(9_000_001_000_200)
				wantName := "incoming winner"
				if scenario == "older" {
					clock = 9_000_001_000_050
					wantName = "retained winner"
				}
				cronEvent := incrementalRemoteEvent(t, p, "", "cron.workflow.updated", strings.ReplaceAll(string(cronPullWorkflow(t, p, "incoming winner", clock).Payload), `"author":"tester"`, `"author":"editor"`), clock)
				if scenario == "reset" {
					cronEvent = cronPullWorkflow(t, p, "incoming winner", clock)
				}
				if scenario == "observation" {
					retained := cronMixedObservation(t, p, issue.UID, first.EventUID, "failed", 2, 9_000_001_000_130)
					_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{retained})
					require.NoError(t, err)
					require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
					cronEvent = cronMixedObservation(t, p, issue.UID, first.EventUID, "succeeded", 1, 9_000_001_000_120)
					wantName = "retained winner"
				}
				if scenario == "snapshot" {
					var doc db.CronDefinitionEvent
					require.NoError(t, json.Unmarshal(first.Payload, &doc))
					doc.DefinitionEventUID = first.EventUID
					doc.DefinitionHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: first.HLCPhysicalMS, OriginInstanceUID: first.OriginInstanceUID}
					payload, err := json.Marshal(doc)
					require.NoError(t, err)
					// The snapshot envelope is newer, but its original clock cannot beat
					// the later retained edit.
					cronEvent = incrementalRemoteEvent(t, p, "", "cron.workflow.snapshot", string(payload), 9_000_001_000_300)
					wantName = "retained winner"
				}
				issueEvent := incrementalRemoteEvent(t, p, "", "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":"mixed changed"}`, issue.UID), 9_000_001_000_400)
				if scenario == "reset" {
					issueEvent = incrementalRemoteEvent(t, p, issue.UID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"mixed changed","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z"}`, issue.UID, issue.ShortID), 9_000_001_000_400)
				}
				page := []db.RemoteEvent{issueEvent, cronEvent}
				if scenario == "reset" {
					page = []db.RemoteEvent{cronEvent, issueEvent}
				}
				if scenario == "links" {
					page = append(page, incrementalRemoteEvent(t, p, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, other.UID), 9_000_001_000_450))
				}
				if scenario == "project-wide" {
					page = append(page, incrementalRemoteEvent(t, p, "", "project.metadata_updated", `{"diff":{"wide":{"from":null,"to":true}}}`, 9_000_001_000_500))
				}
				reference := cloneIncrementalStore(t, d)
				if pull {
					var uids []string
					for _, ev := range page {
						uids = append(uids, ev.EventUID)
					}
					for _, store := range []*Store{d, reference} {
						_, err := store.InsertRemoteEvents(ctx, p.ID, page)
						require.NoError(t, err)
					}
					require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, uids))
					require.NoError(t, reference.MaterializeFederatedProject(ctx, p.ID))
				} else {
					inputs := make([]db.FederationIngestEvent, len(page))
					for i, ev := range page {
						inputs[i] = db.FederationIngestEvent{SourceEventID: int64(i + 1), Event: ev}
					}
					params := db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: page[0].OriginInstanceUID, EventFeatures: db.SupportedEventFeatures, Events: inputs}
					result, err := d.IngestFederationEvents(ctx, params)
					require.NoError(t, err)
					require.Equal(t, len(page), result.Accepted)
					_, err = ingestFullRebuildReference(ctx, reference, params)
					require.NoError(t, err)
				}
				got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "mixed changed", got.Title)
				if scenario == "links" {
					links, err := d.LinksByIssue(ctx, got.ID)
					require.NoError(t, err)
					require.Len(t, links, 1, "untouched link endpoints remain visible")
				}
				workflow, err := (db.CronSQL{Query: d}).Workflow(ctx, p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
				require.NoError(t, err)
				require.Equal(t, wantName, workflow.Name)
				require.Equal(t, "tester", workflow.Author)
				if scenario == "observation" {
					run, err := (db.CronSQL{Query: d}).Run(ctx, p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FBB")
					require.NoError(t, err)
					require.Equal(t, "failed", run.Status, "an older observation must not replace retained evidence")
				}
				require.Equal(t, federationProjectionRows(t, reference, p.ID), federationProjectionRows(t, d, p.ID))
				require.Equal(t, cronPullRows(t, reference, p.ID), cronPullRows(t, d, p.ID))
				if scenario == "project-wide" {
					project, err := d.ProjectByID(ctx, p.ID)
					require.NoError(t, err)
					require.JSONEq(t, `{"wide":true}`, string(project.Metadata))
				}
				if movedProject.ID != 0 {
					uid, err := db.MovedIssueUID(ctx, d, movedProject.ID, localAlias, false)
					require.NoError(t, err)
					require.Equal(t, issue.UID, uid)
					require.Equal(t, federationProjectionRows(t, reference, movedProject.ID), federationProjectionRows(t, d, movedProject.ID))
				}
			})
		}
	}
}

func cronMixedObservation(t testing.TB, p db.Project, issueUID, definitionUID, status string, revision int, clock int64) db.RemoteEvent {
	t.Helper()
	payload := fmt.Sprintf(`{"project_uid":%q,"uid":"01ARZ3NDEKTSV4RRFFQ69G5FBB","workflow_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","workflow_definition_event_uid":%q,"issue_uid":%q,"actor":"tester","status":%q,"summary":{"version":1,"message":"synthetic"},"revision":%d,"created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:%02dZ"}`, p.UID, definitionUID, issueUID, status, revision, revision)
	return incrementalRemoteEvent(t, p, issueUID, "cron.run.observed", payload, clock)
}
