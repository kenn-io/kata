package db

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/teammate"
)

func TestStampEventTeammateScope(t *testing.T) {
	ctx := teammate.WithRequest(t.Context(), "worker-1")
	for _, kind := range []string{"issue.created", "issue.updated", "issue.commented", "issue.comment_edited", "issue.assigned", "issue.unassigned", "issue.assignment_renewed", "issue.priority_set", "issue.priority_cleared", "issue.labeled", "issue.unlabeled", "issue.metadata_updated", "issue.linked", "issue.unlinked", "issue.links_changed", "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.moved", "issue.external_root_bound", "issue.external_root_unbound", "issue.external_root_paused", "issue.external_root_resumed", "issue.external_field_resolved", "issue.external_field_conflicted", "issue.external_comment_resolved", "claim.acquired", "claim.released", "claim.force_released", "claim.violated"} {
		t.Run(kind, func(t *testing.T) {
			got, err := StampEventTeammate(ctx, kind, "coordinator", `{ "teammate":"reply-recipient", "large":9007199254740993 }`)
			require.NoError(t, err)
			require.Contains(t, got, `"attributed_teammate":"worker-1"`)
			require.Contains(t, got, `"teammate":"reply-recipient"`)
			require.Contains(t, got, `9007199254740993`)
		})
	}
	for _, kind := range []string{"project.created", "cron.run.observed", "cron.run.snapshot", "issue.snapshot", "claim.expired", "issue.assignment_expired", "issue.scheduled_reached", "issue.future_action"} {
		got, err := StampEventTeammate(ctx, kind, "coordinator", `{"keep":true}`)
		require.NoError(t, err)
		require.Equal(t, `{"keep":true}`, got, kind)
	}
	for _, test := range []struct{ actor, handle string }{{"system", "worker-1"}} {
		got, err := StampEventTeammate(teammate.WithRequest(ctx, test.handle), "issue.created", test.actor, `{"keep":true}`)
		require.NoError(t, err)
		require.Equal(t, `{"keep":true}`, got)
	}
}

func TestStampEventTeammateSkipsCronOwnedMetadata(t *testing.T) {
	ctx := recurringWriteContext(teammate.WithRequest(t.Context(), "worker-1"))
	got, err := StampEventTeammate(ctx, "issue.metadata_updated", "coordinator", `{"diff":{}}`)
	require.NoError(t, err)
	require.Equal(t, `{"diff":{}}`, got)
}

func TestStampEventTeammateWritesExplicitEmptyDisplay(t *testing.T) {
	for _, tc := range []struct{ kind, payload string }{
		{"issue.created", `{"metadata":{"teammate":"route"}}`},
		{"issue.commented", `{"teammate":"route"}`},
	} {
		for _, explicitEmpty := range []bool{false, true} {
			name := "unselected"
			if explicitEmpty {
				name = "explicit-empty"
			}
			t.Run(tc.kind+"/"+name, func(t *testing.T) {
				ctx := t.Context()
				if explicitEmpty {
					ctx = teammate.WithRequest(ctx, "")
				}
				got, err := StampEventTeammate(ctx, tc.kind, "coordinator", tc.payload)
				require.NoError(t, err)
				require.Contains(t, got, `"attributed_teammate":""`)
				require.Contains(t, got, `"teammate":"route"`)
			})
		}
	}
}

func TestStampEventTeammateHonorsSuppression(t *testing.T) {
	payload := `{"teammate":"route"}`
	got, err := StampEventTeammate(WithoutEventTeammate(teammate.WithRequest(t.Context(), "worker-1")), "issue.commented", "coordinator", payload)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}
