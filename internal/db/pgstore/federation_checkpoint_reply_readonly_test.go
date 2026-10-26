package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type checkpointReplyProbe struct {
	readOnly bool
	queries  []string
}

type checkpointReplyConnector struct{ probe *checkpointReplyProbe }

func (c checkpointReplyConnector) Connect(context.Context) (driver.Conn, error) {
	return &checkpointReplyConn{probe: c.probe}, nil
}

func (checkpointReplyConnector) Driver() driver.Driver { return checkpointReplyDriver{} }

type checkpointReplyDriver struct{}

func (checkpointReplyDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type checkpointReplyConn struct{ probe *checkpointReplyProbe }

func (*checkpointReplyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (*checkpointReplyConn) Close() error              { return nil }
func (*checkpointReplyConn) Begin() (driver.Tx, error) { return nil, errors.New("BeginTx required") }
func (c *checkpointReplyConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.probe.readOnly = opts.ReadOnly
	return checkpointReplyTx{}, nil
}

func (c *checkpointReplyConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	upper := strings.ToUpper(query)
	c.probe.queries = append(c.probe.queries, query)
	if c.probe.readOnly && strings.Contains(upper, "FOR UPDATE") {
		return nil, errors.New("cannot execute SELECT FOR UPDATE in a read-only transaction")
	}
	switch {
	case strings.Contains(upper, "SELECT EXISTS(SELECT 1 FROM COMMENTS"):
		return &checkpointReplyRows{columns: []string{"exists"}, values: [][]driver.Value{{false}}}, nil
	case strings.Contains(upper, "FROM EVENTS E JOIN PURGE_LOG P"):
		return &checkpointReplyRows{columns: []string{"owners", "project_id"}, values: [][]driver.Value{{int64(1), int64(2)}}}, nil
	case strings.Contains(upper, "FROM FEDERATION_BINDINGS WHERE PROJECT_ID="):
		projectID := int64(1)
		if len(args) > 0 {
			projectID = args[0].Value.(int64)
		}
		return &checkpointReplyRows{columns: checkpointBindingColumns, values: [][]driver.Value{checkpointBindingRow(projectID)}}, nil
	case strings.Contains(upper, "FROM FEDERATION_BINDINGS WHERE ROLE="):
		return &checkpointReplyRows{columns: checkpointBindingColumns, values: [][]driver.Value{
			checkpointBindingRow(1), checkpointBindingRow(2),
		}}, nil
	case strings.Contains(upper, "SELECT P.UID,B.ROLE,M.VALUE"):
		projectID := args[0].Value.(int64)
		return &checkpointReplyRows{columns: []string{"uid", "role", "instance_uid"}, values: [][]driver.Value{{fmt.Sprintf("project-%d", projectID), "hub", "root-instance"}}}, nil
	case strings.Contains(upper, "FROM FEDERATION_ROOT_KEYS"):
		return &checkpointReplyRows{columns: []string{"project_uid", "authority_uid", "key_id", "public_key", "active"}}, nil
	default:
		return nil, fmt.Errorf("unexpected checkpoint reply query: %s", query)
	}
}

type checkpointReplyTx struct{}

func (checkpointReplyTx) Commit() error   { return nil }
func (checkpointReplyTx) Rollback() error { return nil }

type checkpointReplyRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r checkpointReplyRows) Columns() []string { return r.columns }
func (r checkpointReplyRows) Close() error      { return nil }
func (r *checkpointReplyRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var checkpointBindingColumns = []string{
	"project_id", "role", "hub_url", "hub_project_id", "hub_project_uid", "replay_horizon_event_id",
	"pull_cursor_event_id", "push_enabled", "push_cursor_event_id", "bound_actor", "allow_insecure",
	"enabled", "created_at", "updated_at", "last_sync_at", "relay_config",
}

func checkpointBindingRow(projectID int64) []driver.Value {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return []driver.Value{
		projectID, "hub", "", projectID, fmt.Sprintf("project-%d", projectID), int64(0),
		int64(0), int64(0), int64(0), "member", int64(0), int64(1), now, now, nil, nil,
	}
}

func TestRelayCheckpointPurgedReplyUsesReadOnlyGroupDiscovery(t *testing.T) {
	probe := &checkpointReplyProbe{}
	database := sql.OpenDB(checkpointReplyConnector{probe: probe})
	t.Cleanup(func() { _ = database.Close() })
	tx, err := database.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	omit, err := relayCheckpointCanOmitPurgedCommentReferenceTx(t.Context(), tx, 1, "source-issue", "reply-comment", "purged-target-comment", false)
	require.NoError(t, err)
	require.True(t, omit)
	require.True(t, probe.readOnly)
	groupQueryFound := false
	for _, query := range probe.queries {
		if strings.Contains(strings.ToUpper(query), "FROM FEDERATION_BINDINGS WHERE ROLE=") {
			groupQueryFound = true
			require.NotContains(t, strings.ToUpper(query), "FOR UPDATE")
		}
	}
	require.True(t, groupQueryFound, "cross-project purge authorization must discover the shared group")
}
