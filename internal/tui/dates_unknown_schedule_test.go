package tui

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDatesFormRejectsUnknownScheduleRules(t *testing.T) {
	h := newDatesHarness(t, nil, viewDetail)
	// The daemon now refuses to read such an issue, so give the model the stored
	// value directly, as an older daemon or a cached row would.
	saved := jsontext.Value(`{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}`)
	h.m.detail.issue.Metadata.ScheduledOn = saved
	h.m.list.issues = []Issue{*h.m.detail.issue}
	h.openForm()
	require.Contains(t, h.m.input.err, "unsupported schedule tzdata version")
	before := h.revision()
	h.fill("", "2026-12-01")
	require.Nil(t, h.save(), "a save over unreadable dates writes nothing")
	require.Equal(t, before, h.revision())
	require.Contains(t, h.m.input.err, "unsupported schedule tzdata version")
}
