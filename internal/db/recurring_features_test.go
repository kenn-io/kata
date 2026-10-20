package db_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Progress and occurrence state must fence old peers even outside cron events.
func TestRecurringPayloadFeatures(t *testing.T) {
	for _, tc := range []struct {
		kind, payload string
		required      string
	}{
		{"cron.job.created", `{"definition":{"action":{"kind":"notify"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}}`, "cron_v1"},
		{"issue.created", `{"metadata":{"cron.occurrence":{"version":1}}}`, "cron_v1"},
		{"issue.metadata_updated", `{"diff":{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{"to":null}}}`, "cron_v1"},
		{"cron.job.deleted", `{"definition":{"action":{"kind":"notify"},"issue":{"kind":"per-run"}}}`, "cron_v1"},
		{"cron.job.created", `{"definition":{"action":{"kind":"execute"},"issue":{"kind":"per-run"}}}`, "cron_v1"},
		{"issue.created", `{"metadata":{"description":"cron.occurrence"}}`, ""},
		{"project.metadata_updated", `{"diff":{"cron.progress.custom":{"to":"value"}}}`, ""},
	} {
		features, e := db.PayloadRequiredFeatures(tc.kind, tc.payload)
		require.NoError(t, e)
		require.Equal(t, tc.required, features)
		if tc.required != "" {
			require.ErrorIs(t, db.RequireEventFeatures("", features), db.ErrUnsupportedEventFeatures)
		}
	}
}

func TestRecurringFeaturesCachePositiveProject(t *testing.T) {
	state := &featureProbeState{}
	database := sql.OpenDB(featureProbeConnector{state: state})
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	projectUID, err := uid.New()
	require.NoError(t, err)
	for range 2 {
		features, err := db.ProjectRequiredEventFeatures(t.Context(), database, projectUID)
		require.NoError(t, err)
		require.Equal(t, db.MergeEventFeatures(db.SupportedEventFeatures), features)
	}
	require.Equal(t, 1, state.probeQueries, "a retained native projection is scanned once per process")
}

type featureProbeState struct{ probeQueries int }

type featureProbeConnector struct{ state *featureProbeState }

func (c featureProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return &featureProbeConn{state: c.state}, nil
}
func (c featureProbeConnector) Driver() driver.Driver { return featureProbeDriver(c) }

type featureProbeDriver struct{ state *featureProbeState }

func (d featureProbeDriver) Open(string) (driver.Conn, error) {
	return &featureProbeConn{state: d.state}, nil
}

type featureProbeConn struct{ state *featureProbeState }

func (*featureProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unused prepare")
}
func (*featureProbeConn) Close() error              { return nil }
func (*featureProbeConn) Begin() (driver.Tx, error) { return nil, errors.New("unused begin") }
func (c *featureProbeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT EXISTS") {
		return &featureProbeRows{presence: true}, nil
	}
	if strings.Contains(query, "'projection.job'") {
		c.state.probeQueries++
		return &featureProbeRows{}, nil
	}
	return nil, errors.New("unexpected feature probe query")
}

type featureProbeRows struct{ presence, seen bool }

func (r *featureProbeRows) Columns() []string {
	if r.presence {
		return []string{"cron_present", "issue_moved"}
	}
	return []string{"kind", "payload"}
}
func (*featureProbeRows) Close() error { return nil }
func (r *featureProbeRows) Next(values []driver.Value) error {
	if r.seen {
		if r.presence {
			return io.EOF
		}
		return errors.New("unnecessary rows read after native requirement established")
	}
	r.seen = true
	if r.presence {
		values[0] = true
		values[1] = false
		return nil
	}
	values[0] = "projection.job"
	values[1] = `{"action":{"kind":"notify"},"issue":{"kind":"per-run"}}`
	return nil
}

// Escaping a native metadata key must not change the peer capability fence.
func FuzzRecurringMetadataFeatures(f *testing.F) {
	f.Add(uint64(0), false)
	f.Add(^uint64(0), true)
	f.Fuzz(func(t *testing.T, escapes uint64, progress bool) {
		key := "cron.occurrence"
		if progress {
			key = "cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV"
		}
		var encoded strings.Builder
		for i, c := range key {
			if escapes&(1<<i) != 0 {
				fmt.Fprintf(&encoded, `\u%04x`, c)
			} else {
				encoded.WriteRune(c)
			}
		}
		for _, field := range []string{"metadata", "diff"} {
			payload := `{"` + field + `":{"` + encoded.String() + `":null}}`
			features, err := db.PayloadRequiredFeatures("issue.metadata_updated", payload)
			require.NoError(t, err)
			require.Equal(t, "cron_v1", features)
			require.ErrorIs(t, db.RequireEventFeatures("", features), db.ErrUnsupportedEventFeatures)
		}
	})
}
