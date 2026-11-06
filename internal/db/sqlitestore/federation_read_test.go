package sqlitestore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestReadFederationWriteQueue(t *testing.T) {
	t.Parallel()
	for _, metadata := range []bool{false, true} {
		name := "events"
		if metadata {
			name = "metadata"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, project, _ := incrementalTestStore(t)
			binding, err := d.FederationBindingByProject(t.Context(), project.ID)
			require.NoError(t, err)

			// Keep the writer slot occupied while the federation read runs.
			writer, err := d.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			in := db.FederationReadParams{ProjectID: project.ID, Metadata: metadata, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100}
			page, err := d.ReadFederation(ctx, in)
			if metadata {
				require.ErrorIs(t, err, context.DeadlineExceeded, "metadata reads must wait for the writer slot")
			} else {
				require.NoError(t, err, "events reads must finish while the writer slot is occupied")
				require.NotEmpty(t, page.Events)
			}
			require.NoError(t, writer.Rollback())
			if metadata {
				page, err = d.ReadFederation(t.Context(), in)
				require.NoError(t, err)
				require.GreaterOrEqual(t, page.BaselineThroughEventID, binding.ReplayHorizonEventID)
			}
		})
	}
}
