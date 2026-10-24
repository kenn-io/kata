package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedNativeCurrentClose(t *testing.T) {
	dbtest.RunScopedNativeCurrentClose(t, openUIStore(t))
}

func TestScopedFirstNativeCapture(t *testing.T) {
	dbtest.RunScopedFirstNativeCapture(t, openUIStore(t))
}

func TestScopedNativeTypedClose(t *testing.T) {
	dbtest.RunScopedNativeTypedClose(t, openUIStore(t))
}

func TestScopedNativeImportStatusBoundary(t *testing.T) {
	dbtest.RunScopedNativeImportStatusBoundary(t, openUIStore(t))
}

func TestScopedCaptureLocalDirty(t *testing.T) { dbtest.RunScopedCaptureLocalDirty(t, openUIStore(t)) }

func TestScopedCaptureCompletion(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCaptureCompletion(t, s, func(ctx context.Context, view, capture string, revision int64) (bool, error) {
		var done bool
		err := s.relayTx(ctx, func(tx *sql.Tx) error {
			var err error
			done, err = completeScopedCaptureWorkTx(ctx, tx, view, capture, revision)
			return err
		})
		return done, err
	})
}

func TestScopedCapturePinnedCompletion(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCapturePinnedCompletion(t, s, func(ctx context.Context, view, capture string, revision int64) (bool, error) {
		var done bool
		err := s.relayTx(ctx, func(tx *sql.Tx) error {
			var err error
			done, err = completeScopedCaptureWorkTx(ctx, tx, view, capture, revision)
			return err
		})
		return done, err
	})
}
func TestScopedCaptureIndexedHopQuota(t *testing.T) {
	dbtest.RunScopedCaptureIndexedHopQuota(t, openUIStore(t))
}

func TestScopedCaptureConsistentRetry(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCaptureConsistentRetry(t, s, func() db.Storage {
		writer, err := Open(t.Context(), s.Path())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = writer.Close() })
		return writer
	})
}
func TestScopedCaptureGenerationAssociation(t *testing.T) {
	dbtest.RunScopedCaptureGenerationAssociation(t, openUIStore(t))
}

func TestScopedOrdinaryCloseCompatibility(t *testing.T) {
	dbtest.RunScopedOrdinaryCloseCompatibility(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
func TestScopedCloseParticipation(t *testing.T) {
	dbtest.RunScopedCloseParticipation(t, openUIStore(t))
}
func TestScopedCloseAfterGrantLoss(t *testing.T) {
	dbtest.RunScopedCloseAfterGrantLoss(t, openUIStore(t))
}

func TestScopedObservedStatusBoundary(t *testing.T) {
	dbtest.RunScopedObservedStatusBoundary(t, openUIStore(t))
}

func TestScopedCloseLegalPurge(t *testing.T) {
	dbtest.RunScopedCloseLegalPurge(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
func TestScopedCloseCurrentSnapshot(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCloseCurrentSnapshot(t, s, func(ctx context.Context, issue db.Issue) (string, error) {
		tx, err := s.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return "", err
		}
		defer func() { _ = tx.Rollback() }()
		return s.federationIssueSnapshotPayload(ctx, tx, issue)
	})
}

func TestScopedCloseSnapshotReplay(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCloseSnapshotReplay(t, s, func(t *testing.T) db.Storage { return openUIStore(t) }, func(ctx context.Context, project, before int64) error {
		_, err := s.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND id<$2`, project, before)
		return err
	})
}

func TestScopedCloseLegalMove(t *testing.T) {
	dbtest.RunScopedCloseLegalMove(t, openUIStore(t), func(t *testing.T) db.Storage { return openUIStore(t) })
}
func TestScopedCloseRemoteCurrent(t *testing.T) {
	dbtest.RunScopedCloseRemoteCurrent(t, openUIStore(t))
}

func TestScopedCaptureRootOnlyDirty(t *testing.T) {
	dbtest.RunScopedCaptureRootOnlyDirty(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
func TestScopedCaptureLateArtifact(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCaptureLateArtifact(t, s, func(ctx context.Context, view, capture string, revision int64) (bool, error) {
		var done bool
		err := s.relayTx(ctx, func(tx *sql.Tx) error {
			var err error
			done, err = completeScopedCaptureWorkTx(ctx, tx, view, capture, revision)
			return err
		})
		return done, err
	})
}

func TestScopedCaptureConcurrentViews(t *testing.T) {
	s := openUIStore(t)
	dbtest.RunScopedCaptureConcurrentViews(t, s, func() db.Storage {
		writer, err := Open(t.Context(), s.Path())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = writer.Close() })
		return writer
	})
}

func TestScopedCaptureEndpointDirty(t *testing.T) {
	dbtest.RunScopedCaptureEndpointDirty(t, openUIStore(t))
}
func TestScopedCaptureIngestDirty(t *testing.T) {
	dbtest.RunScopedCaptureIngestDirty(t, openUIStore(t))
}

func TestScopedCaptureImportDirty(t *testing.T) {
	dbtest.RunScopedCaptureImportDirty(t, openUIStore(t))
}

func TestScopedCaptureDirtyRollbackAndAudit(t *testing.T) {
	s := openUIStore(t)
	f := dbtest.NewScopedCaptureFixture(t, s)
	_, err := s.CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	before := dbtest.ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID)
	rollback := errors.New("rollback native current mutation")
	uid := "00000000000000000000001040"
	err = s.relayTx(t.Context(), func(tx *sql.Tx) error {
		_, _, err := s.createIssueTx(t.Context(), tx, db.CreateIssueParams{ProjectID: f.Projects[0].ID, UID: uid, Author: "member", Title: "Rolled back issue"})
		if err != nil {
			return err
		}
		var revision, proofs int64
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT dirty_revision FROM federation_scoped_work WHERE view_uid=$1`, f.Params.View.Key.ViewUID).Scan(&revision))
		require.Greater(t, revision, before.DirtyRevision, "native mutation dirties work before commit")
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM federation_canonical_close_state WHERE issue_uid=$1`, uid).Scan(&proofs))
		require.EqualValues(t, 1, proofs, "native current proof is in same mutation transaction")
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	require.Equal(t, before, dbtest.ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID))
	_, err = s.IssueByUID(t.Context(), uid, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	err = s.relayTx(t.Context(), func(tx *sql.Tx) error {
		_, err := s.insertEventTx(t.Context(), tx, eventInsert{ProjectID: f.Projects[0].ID, ProjectName: f.Projects[0].Name, IssueID: &f.Issue.ID, Type: "issue.scheduled_reached", Actor: "member", Payload: `{}`})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, before, dbtest.ScopedCaptureWork(t, s, f.Params.View.Key.ViewUID), "private scheduled audit is retained without public current work")
}

func TestScopedCaptureDirtyQuota(t *testing.T) {
	dbtest.RunScopedCaptureDirtyQuota(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedV1CloseIngress(t *testing.T) {
	dbtest.RunScopedV1CloseIngress(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
func TestScopedV1CloseBootstrap(t *testing.T) { dbtest.RunScopedV1CloseBootstrap(t, openUIStore(t)) }
func TestScopedV1CloseReset(t *testing.T) {
	dbtest.RunScopedV1CloseReset(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedCaptureLegacyResetFenced(t *testing.T) {
	dbtest.RunScopedCaptureLegacyResetFenced(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedPredicateNativeABA(t *testing.T) {
	s := openUIStore(t)
	clock := func(e db.RemoteEvent) db.FoldClock {
		return db.FoldClock{EventUID: e.EventUID, OriginInstanceUID: e.OriginInstanceUID, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter}
	}
	f := dbtest.NewScopedPredicateABAFixture(t, s)
	err := s.relayTx(t.Context(), func(tx *sql.Tx) error {
		prepared := make([]preparedFederationIngestEvent, 0, len(f.Batch.Events))
		for _, input := range f.Batch.Events {
			prepared = append(prepared, preparedFederationIngestEvent{SourceEventID: input.SourceEventID, Event: input.Event})
		}
		m, err := prepareFederationMaterializationTx(t.Context(), tx, f.Projects[0].ID, prepared)
		if err != nil {
			return err
		}
		require.NotContains(t, m.projection.Issues, f.Issue.UID, "actual native unchanged issue row was pruned")
		key := db.FoldLinkKey{FromUID: f.Issue.UID, ToUID: f.Parent.UID, Type: "parent"}
		require.NotContains(t, m.links.Links, key, "actual native unchanged parent row was pruned")
		facts := m.projection.ScopedPredicateFacts()
		require.Equal(t, []db.FoldClock{clock(f.Batch.Events[3].Event), clock(f.HistoricalIssueMetadata)}, facts.IssueMetadata[f.Issue.UID])
		require.Equal(t, []db.FoldClock{clock(f.Batch.Events[5].Event), clock(f.HistoricalProjectMetadata)}, facts.ProjectMetadata[f.Projects[0].UID])
		group := m.links.ScopedPredicateFacts()
		require.Equal(t, f.Parent.UID, group.Parents[f.Issue.UID].ParentUID)
		require.Equal(t, f.Batch.Events[4].Event.EventUID, group.Parents[f.Issue.UID].Clock.EventUID, "effective GROUP parent writer survives pruning")
		return nil
	})
	require.NoError(t, err)
	dbtest.AssertScopedPredicateABAAccepted(t, s, f)
}

func TestScopedPredicateCronWitness(t *testing.T) {
	dbtest.RunScopedPredicateCronWitness(t, openUIStore(t))
}

func TestScopedReplicaCloseLifetime(t *testing.T) {
	dbtest.RunScopedReplicaCloseLifetime(t, func(t *testing.T) db.Storage { return openUIStore(t) }, func(ctx context.Context, store db.Storage, projectID int64, rollback bool) error {
		sentinel := errors.New("private replacement rollback")
		err := store.(*Store).relayTx(ctx, func(tx *sql.Tx) error {
			if err := clearFederatedProjection(ctx, tx, projectID); err != nil {
				return err
			}
			if rollback {
				return sentinel
			}
			return nil
		})
		if rollback && errors.Is(err, sentinel) {
			return nil
		}
		return err
	})
}
