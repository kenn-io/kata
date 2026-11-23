package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type previewProjectStorage struct{ db.Storage }

func (previewProjectStorage) ProjectByID(context.Context, int64) (db.Project, error) {
	return db.Project{ID: 1}, nil
}

func TestRecurrencePreviewStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	in := &api.RecurrencePreviewRequest{ProjectID: 1, RRule: "FREQ=SECONDLY", DTStart: "2026-01-01", Timezone: "UTC"}
	started := time.Now()
	_, err := previewRecurrenceHandler(ServerConfig{DB: previewProjectStorage{}})(ctx, in)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), 500*time.Millisecond)
}
