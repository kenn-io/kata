package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type previewProjectStorage struct{ db.Storage }

func (previewProjectStorage) ProjectByID(context.Context, int64) (db.Project, error) {
	return db.Project{ID: 1}, nil
}

type cancelDuringPreview struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringPreview) Err() error {
	c.checks++
	// Allow the scheduler to enter its occurrence loop before canceling.
	if c.checks == 8 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRecurrencePreviewStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	previewCtx := &cancelDuringPreview{Context: ctx, cancel: cancel}
	in := &api.RecurrencePreviewRequest{ProjectID: 1, RRule: "FREQ=SECONDLY", DTStart: "2026-01-01", Timezone: "UTC"}
	_, err := previewRecurrenceHandler(ServerConfig{DB: previewProjectStorage{}})(previewCtx, in)
	require.ErrorIs(t, err, context.Canceled)
}
