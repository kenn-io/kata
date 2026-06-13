package daemon

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/db"
)

// chainParentStore fakes ParentOf over a synthetic parent chain
// 1 → 2 → … → chainTop (chainTop has no parent). Every other Storage
// method panics via the embedded nil interface, pinning
// parentReplaceWouldCycle to ParentOf lookups only.
type chainParentStore struct {
	db.Storage
	chainTop int64
}

func (s chainParentStore) ParentOf(_ context.Context, issueID int64) (db.Link, error) {
	if issueID >= s.chainTop {
		return db.Link{}, db.ErrNotFound
	}
	return db.Link{FromIssueID: issueID, ToIssueID: issueID + 1, Type: "parent"}, nil
}

// The pre-flight must refuse any chain the in-transaction guard would
// refuse, and it must do so before the handler unlinks the old parent.
// Both walks share db.MaxParentDepth; if the pre-flight tolerated deeper
// chains, a replace on a 1024+-deep valid chain would pass pre-flight,
// delete the old parent, then fail inside CreateLinkAndEvent — leaving
// the issue parentless.
func TestParentReplaceWouldCycle_DepthMatchesTxGuard(t *testing.T) {
	tests := []struct {
		name      string
		chainTop  int64
		childID   int64
		wantCycle bool
		wantErr   bool
	}{
		{
			name:     "short chain without cycle is allowed",
			chainTop: 10,
			childID:  9999,
		},
		{
			name:      "child already an ancestor is a cycle",
			chainTop:  10,
			childID:   5,
			wantCycle: true,
		},
		{
			name:     "chain deeper than the tx guard cap is refused",
			chainTop: db.MaxParentDepth + 10,
			childID:  9_999_999,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := chainParentStore{chainTop: tt.chainTop}
			cycle, err := parentReplaceWouldCycle(context.Background(), store, tt.childID, 1)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parentReplaceWouldCycle tolerated a %d-deep chain; the in-tx guard refuses at %d, so the replace would unlink the old parent and then fail", tt.chainTop, db.MaxParentDepth)
				}
				return
			}
			if err != nil {
				t.Fatalf("parentReplaceWouldCycle: %v", err)
			}
			if cycle != tt.wantCycle {
				t.Fatalf("parentReplaceWouldCycle cycle = %v, want %v", cycle, tt.wantCycle)
			}
		})
	}
}
