package pgstore

import (
	"context"
	"database/sql"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"testing"
)

func TestScopedOwnerRestoreReadModelAtomic(t *testing.T) {
	dbtest.RunScopedOwnerRestoreReadModelAtomic(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedAtomicBaselineAndReferenceCoverage(t *testing.T) {
	dbtest.RunScopedAtomicBaselineAndReferenceCoverage(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedFenceRetry(t *testing.T) {
	for _, mode := range []string{"root_same_reason", "root_parent_revoked", "root_policy_loss", "incoming_same_reason", "incoming_policy_loss"} {
		t.Run(mode, func(t *testing.T) {
			root := openUIStore(t)
			target := root
			if mode == "incoming_same_reason" || mode == "incoming_policy_loss" {
				target = openUIStore(t)
			}
			dbtest.RunScopedFenceRetry(t, root, target, mode, func(ctx context.Context, key db.ScopedViewKey) error {
				return scopedMoveNativeCheck(ctx, target, key)
			})
		})
	}
}

func TestScopedFenceRecovery(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedFenceRecovery(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedCommitBatch(t *testing.T) {
	dbtest.RunScopedCommitBatch(t, openUIStore(t))
}

func TestScopedViewDeliveryFencedRoot(t *testing.T) {
	for _, mode := range []string{"fresh_fenced", "emitted_fenced", "emitted_revoked"} {
		t.Run(mode, func(t *testing.T) {
			dbtest.RunScopedViewDeliveryFencedRoot(t, openUIStore(t), mode)
		})
	}
}

func TestScopedViewDeliverySharedStream(t *testing.T) {
	dbtest.RunScopedViewDeliverySharedStream(t, openUIStore(t))
}

func TestScopedViewDelivery(t *testing.T) {
	dbtest.RunScopedViewDelivery(t, openUIStore(t))
}

func TestScopedCaptureObligationWorker(t *testing.T) {
	for _, mode := range []string{"no_effect_completion", "publication_ack"} {
		t.Run(mode, func(t *testing.T) {
			dbtest.RunScopedCaptureObligationWorker(t, openUIStore(t), mode)
		})
	}
}

func TestScopedNoEffectCaptureCompletion(t *testing.T) {
	dbtest.RunScopedNoEffectCaptureCompletion(t, openUIStore(t))
}

func TestScopedCheckpointBytes(t *testing.T) {
	dbtest.RunScopedCheckpointBytes(t, openUIStore(t))
}

func TestScopedApplyBatch(t *testing.T) {
	dbtest.RunScopedApplyBatch(t, openUIStore(t), openUIStore(t))
}

func TestScopedApplyCurrentObservers(t *testing.T) {
	dbtest.RunScopedApplyCurrentObservers(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedApplyBatchBytes(t *testing.T) {
	dbtest.RunScopedApplyBatchBytes(t, openUIStore(t), openUIStore(t))
}

func TestScopedApplyRemoval(t *testing.T) {
	dbtest.RunScopedApplyRemoval(t, openUIStore(t), openUIStore(t))
}

func TestScopedApplyClosePeerLifetime(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyClosePeerLifetime(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedApplyClosePeerMoveLifetime(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyClosePeerMoveLifetime(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedApplyMove(t *testing.T) {
	dbtest.RunScopedApplyMove(t, openUIStore(t), openUIStore(t))
}

func TestScopedApplyCommentMove(t *testing.T) {
	for _, owed := range []bool{false, true} {
		name := "clean"
		if owed {
			name = "owed_comment"
		}
		t.Run(name, func(t *testing.T) {
			root, receiver := openUIStore(t), openUIStore(t)
			dbtest.RunScopedApplyCommentMove(t, root, receiver, owed, func(ctx context.Context, key db.ScopedViewKey) error {
				return scopedMoveNativeCheck(ctx, receiver, key)
			})
		})
	}
}

func TestScopedApplyOwedRemoval(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyOwedRemoval(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return receiver.scopedReadTransaction(ctx, func(tx *sql.Tx) error {
			if _, err := receiver.scopedAuthorityTx(ctx, tx, key, "pull"); err != nil {
				return err
			}
			current, err := receiver.loadScopedMaterializationTx(ctx, tx, key)
			if err != nil {
				return err
			}
			return receiver.checkScopedOwnerFenceTx(ctx, tx, current.Fence)
		})
	})
}

func TestScopedApplyMoveUnownedCollision(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyMoveUnownedCollision(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedApplyMoveForeignCollision(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyMoveForeignCollision(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedApplyMoveOwed(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyMoveOwed(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func scopedMoveNativeCheck(ctx context.Context, store *Store, key db.ScopedViewKey) error {
	return store.scopedReadTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := store.scopedAuthorityTx(ctx, tx, key, "pull"); err != nil {
			return err
		}
		current, err := store.loadScopedMaterializationTx(ctx, tx, key)
		if err != nil {
			return err
		}
		if err := store.checkScopedOwnerFenceTx(ctx, tx, current.Fence); err != nil {
			return err
		}
		return store.validateScopedOwnedTx(ctx, tx, current)
	})
}

func TestScopedApplyCommentRemoval(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyCommentRemoval(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedApplyCommentPurge(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyCommentPurge(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedApplyCommentOwedRemoval(t *testing.T) {
	root, receiver := openUIStore(t), openUIStore(t)
	dbtest.RunScopedApplyCommentOwedRemoval(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, receiver, key)
	})
}

func TestScopedCommentGenerationOmission(t *testing.T) {
	for _, owed := range []bool{false, true} {
		name := "clean"
		if owed {
			name = "owed"
		}
		t.Run(name, func(t *testing.T) {
			root, receiver := openUIStore(t), openUIStore(t)
			dbtest.RunScopedCommentGenerationOmission(t, root, receiver, owed, func(ctx context.Context, key db.ScopedViewKey) error {
				return scopedMoveNativeCheck(ctx, receiver, key)
			})
		})
	}
}

func TestScopedOwnerRestoreCommentsAtomic(t *testing.T) {
	dbtest.RunScopedOwnerRestoreCommentsAtomic(t, func(t *testing.T) db.Storage { return openUIStore(t) }, func(ctx context.Context, store db.Storage, key db.ScopedViewKey) error {
		return scopedMoveNativeCheck(ctx, store.(*Store), key)
	})
}

func TestScopedOwnerRestoreRaisedSequenceAtomic(t *testing.T) {
	dbtest.RunScopedOwnerRestoreRaisedSequenceAtomic(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
