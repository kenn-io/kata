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
	for _, tt := range []struct {
		name     string
		metadata bool
		wantErr  error
	}{
		{name: "events"},
		{name: "metadata", metadata: true, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			in := db.FederationReadParams{ProjectID: project.ID, Metadata: tt.metadata, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100}
			page, err := d.ReadFederation(ctx, in)
			require.ErrorIs(t, err, tt.wantErr)
			if tt.wantErr == nil {
				require.NotEmpty(t, page.Events)
			}
		})
	}
}
