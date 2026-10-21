package tui

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

func TestOutcomesDroppedMutationErrors(t *testing.T) {
	for _, name := range []string{"old daemon", "old detail generation", "cancelled create", "cancelled close", "replaced form"} {
		t.Run(name, func(t *testing.T) {
			recorder := &recordingTelemetry{}
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = recorder })
			m := mFixtureForBar()
			m.api = newUndoClient(NewClient(env.URL, env.HTTP))
			m.detail.loading, m.list.loading = false, false
			m.detail.gen, m.connGen = 2, 2
			mut := mutationDoneMsg{origin: "form", kind: "create", formGen: 1, connGen: 2, err: errors.New("refused")}
			switch name {
			case "old daemon":
				mut.origin, mut.kind, mut.connGen = "list", "close", 1
			case "old detail generation":
				mut.origin, mut.kind, mut.gen = "detail", "close", 1
				m.pendingRefetch = true
			case "cancelled close":
				mut.kind = "close"
			case "replaced form":
				m.input = newNewIssueForm()
				m.input.formGen = 2
			}
			m, cmd := stepModel(m, mut)
			runCmd(cmd)
			require.Empty(t, m.input.err)
			require.Empty(t, m.detail.err)
			require.Empty(t, m.list.status)
			require.Empty(t, recorder.events, "a discarded result displays no error")
		})
	}
}
