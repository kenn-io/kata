package daemon_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type teamCreateBarrierStore struct {
	db.Storage
	arrived atomic.Int32
	ready   chan struct{}
}

func (s *teamCreateBarrierStore) CreateTeam(ctx context.Context, name, actor string) (db.Team, db.Event, error) {
	if name == "concurrent-team" {
		if s.arrived.Add(1) == 2 {
			close(s.ready)
		}
		select {
		case <-s.ready:
		case <-ctx.Done():
			return db.Team{}, db.Event{}, ctx.Err()
		}
	}
	return s.Storage.CreateTeam(ctx, name, actor)
}

// Review16802: concurrent calls meet before the native create. Uniqueness
// must produce a stable conflict for the loser, never an internal error.
func TestConcurrentTeamCreateReturnsConflict(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		gated := &teamCreateBarrierStore{Storage: store, ready: make(chan struct{})}
		f := newProjectAccessFixture(t, gated)
		type outcome struct {
			status int
			body   []byte
			err    error
		}
		done := make(chan outcome, 2)
		for range 2 {
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/teams", bytes.NewBufferString(`{"name":"concurrent-team"}`))
				if err != nil {
					done <- outcome{err: err}
					return
				}
				req.Header.Set("Authorization", "Bearer bootstrap-test-token")
				req.Header.Set("Content-Type", "application/json")
				response, err := f.server.Client().Do(req)
				if err != nil {
					done <- outcome{err: err}
					return
				}
				body, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				if readErr == nil {
					readErr = closeErr
				}
				done <- outcome{status: response.StatusCode, body: body, err: readErr}
			}()
		}
		codes := []int{}
		for range 2 {
			result := <-done
			require.NoError(t, result.err)
			codes = append(codes, result.status)
			if result.status != http.StatusOK {
				require.Equal(t, http.StatusConflict, result.status, string(result.body))
				require.Contains(t, string(result.body), "team_exists")
			}
		}
		sort.Ints(codes)
		require.Equal(t, []int{http.StatusOK, http.StatusConflict}, codes)
		teams, err := store.ListTeams(t.Context())
		require.NoError(t, err)
		matches := 0
		for _, team := range teams {
			if team.Name == "concurrent-team" {
				matches++
			}
		}
		require.Equal(t, 1, matches)
	})
}
