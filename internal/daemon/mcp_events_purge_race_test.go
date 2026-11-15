package daemon_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
)

// purgeDuringResetCheckStore commits real mutations right after the scan's
// own purge check, before it reads its event page.
type purgeDuringResetCheckStore struct {
	db.Storage
	afterCheck func()
}

func (s *purgeDuringResetCheckStore) PurgeResetCheck(ctx context.Context, after, projectID int64) (int64, error) {
	reset, err := s.Storage.PurgeResetCheck(ctx, after, projectID)
	if err == nil && after > 0 && s.afterCheck != nil {
		run := s.afterCheck
		s.afterCheck = nil
		run()
	}
	return reset, err
}

func TestMCPEventsScanDoesNotSkipAConcurrentPurge(t *testing.T) {
	for name, scoped := range map[string]bool{"unscoped": false, "scoped": true} {
		t.Run(name, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				f := newEventsFixture(t, store)
				interleaved := &purgeDuringResetCheckStore{Storage: store}
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, interleaved, f, got.receive)
				principal := f.member
				if scoped {
					principal = f.scoped
				}
				subscribeEvents(t, service, principal, mcpevents.Arguments{For: "example-user"})
				// Give the scan something to read so it makes its purge check.
				_, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: f.publicIssue.ID, Title: new("Before"), Actor: "coordinator"})
				require.NoError(t, err)
				interleaved.afterCheck = func() {
					_, err := store.PurgeIssue(ctx, f.childIssue.ID, "admin", nil)
					require.NoError(t, err)
					_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: f.publicIssue.ID, Title: new("After"), Actor: "coordinator"})
					require.NoError(t, err)
				}
				require.NoError(t, service.Drain(ctx))
				assert.Nil(t, interleaved.afterCheck, "the purge ran during the scan")
				assert.Equal(t, int64(1), got.pings.Load(), "the purge requires an inbox reload")
			})
		})
	}
}
