package sqlitestore

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Slow preparation must still reach the writer-held fallback after a concurrent
// commit invalidates its optimistic snapshot, without repeating the slow work
// optimistically. Fake time crosses the generic retry budget without making the
// real SQLite fixture wait thirty seconds.
func TestFederationSlowPreparationReachesLockedFallback(t *testing.T) {
	for _, direction := range []string{"push", "pull"} {
		t.Run(direction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				d, p, issue := incrementalTestStore(t)
				other, err := d.CreateProject(ctx, "other-project")
				require.NoError(t, err)
				ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated",
					fmt.Sprintf(`{"issue_uid":%q,"title":"updated"}`, issue.UID), 9_000_000_000_000)
				if direction == "pull" {
					_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
					require.NoError(t, err)
				}
				competingWrites := 0
				d.federationIngestPrepared = func() {
					competingWrites++
					_, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
						ProjectID: other.ID, Actor: "tester",
						Patch: map[string]jsontext.Value{"write": []byte(strconv.Itoa(competingWrites))},
					})
					require.NoError(t, err, "optimistic preparation must leave the writer available")
					time.Sleep(31 * time.Second)
				}
				if direction == "push" {
					result, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
						ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID,
						Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
					})
					require.NoError(t, err)
					require.Equal(t, 1, result.Accepted)
				} else {
					require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
				}
				require.Equal(t, 1, competingWrites, "a slow invalidated preparation must take the writer next")
				got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "updated", got.Title)
				var count int
				require.NoError(t, d.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE uid=?", ev.EventUID).Scan(&count))
				require.Equal(t, 1, count, "retries must not duplicate the event")
				project, err := d.ProjectByID(ctx, other.ID)
				require.NoError(t, err)
				require.JSONEq(t, `{"write":1}`, string(project.Metadata))
			})
		})
	}
}

func TestFederationPreparationStopsOnCancellation(t *testing.T) {
	for _, direction := range []string{"push", "pull"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", direction, deadline), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					d, p, issue := incrementalTestStore(t)
					ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated",
						fmt.Sprintf(`{"issue_uid":%q,"title":"updated"}`, issue.UID), 9_000_000_000_000)
					if direction == "pull" {
						_, err := d.InsertRemoteEvents(t.Context(), p.ID, []db.RemoteEvent{ev})
						require.NoError(t, err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					defer cancel()
					preparations := 0
					d.federationIngestPrepared = func() {
						preparations++
						if deadline {
							time.Sleep(2 * time.Minute)
						} else {
							cancel()
						}
					}
					var err error
					if direction == "push" {
						_, err = d.IngestFederationEvents(ctx, db.FederationIngestParams{
							ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID,
							Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
						})
					} else {
						err = d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID})
					}
					if deadline {
						require.ErrorIs(t, err, context.DeadlineExceeded)
					} else {
						require.ErrorIs(t, err, context.Canceled)
					}
					require.Equal(t, 1, preparations)
					got, err := d.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					require.Equal(t, "work", got.Title, "cancellation must leave the projection unchanged")
				})
			})
		}
	}
}

func TestFederationPreparationRechecksFenceOnRetry(t *testing.T) {
	for _, direction := range []string{"push", "pull"} {
		t.Run(direction, func(t *testing.T) {
			d, p, issue := incrementalTestStore(t)
			other, err := d.CreateProject(t.Context(), "other-project")
			require.NoError(t, err)
			ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated",
				fmt.Sprintf(`{"issue_uid":%q,"title":"updated"}`, issue.UID), 9_000_000_000_000)
			if direction == "pull" {
				_, err := d.InsertRemoteEvents(t.Context(), p.ID, []db.RemoteEvent{ev})
				require.NoError(t, err)
			}
			denied := errors.New("fence denied")
			reject := false
			denials := 0
			ctx := db.WithTransactionFence(t.Context(), func(context.Context, db.Transaction) error {
				if reject {
					denials++
					return denied
				}
				return nil
			})
			d.federationIngestPrepared = func() {
				_, err := d.PatchProjectMetadata(t.Context(), db.PatchProjectMetadataIn{
					ProjectID: other.ID, Actor: "tester", Patch: map[string]jsontext.Value{"write": []byte(`true`)},
				})
				require.NoError(t, err)
				reject = true
			}
			if direction == "push" {
				_, err = d.IngestFederationEvents(ctx, db.FederationIngestParams{
					ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID,
					Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
				})
			} else {
				err = d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID})
			}
			require.ErrorIs(t, err, denied)
			require.Equal(t, 1, denials, "permanent fence errors must not be retried")
			got, err := d.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "work", got.Title)
		})
	}
}
