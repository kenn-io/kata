package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type readOnlyGroupProbe struct {
	readOnly bool
	query    string
}

type readOnlyGroupConnector struct{ probe *readOnlyGroupProbe }

func (c readOnlyGroupConnector) Connect(context.Context) (driver.Conn, error) {
	return &readOnlyGroupConn{probe: c.probe}, nil
}

func (readOnlyGroupConnector) Driver() driver.Driver { return readOnlyGroupDriver{} }

type readOnlyGroupDriver struct{}

func (readOnlyGroupDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type readOnlyGroupConn struct{ probe *readOnlyGroupProbe }

func (*readOnlyGroupConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (*readOnlyGroupConn) Close() error              { return nil }
func (*readOnlyGroupConn) Begin() (driver.Tx, error) { return nil, errors.New("BeginTx required") }
func (c *readOnlyGroupConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.probe.readOnly = opts.ReadOnly
	return readOnlyGroupTx{}, nil
}

func (c *readOnlyGroupConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.probe.query = query
	if c.probe.readOnly && strings.Contains(strings.ToUpper(query), "FOR UPDATE") {
		return nil, errors.New("cannot execute SELECT FOR UPDATE in a read-only transaction")
	}
	return readOnlyGroupRows{}, nil
}

type readOnlyGroupTx struct{}

func (readOnlyGroupTx) Commit() error   { return nil }
func (readOnlyGroupTx) Rollback() error { return nil }

type readOnlyGroupRows struct{}

func (readOnlyGroupRows) Columns() []string { return []string{"id"} }
func (readOnlyGroupRows) Close() error      { return nil }
func (readOnlyGroupRows) Next([]driver.Value) error {
	return io.EOF
}

func TestFederationBindingGroupProjectIDsLocksWriteTransactions(t *testing.T) {
	probe := &readOnlyGroupProbe{}
	database := sql.OpenDB(readOnlyGroupConnector{probe: probe})
	t.Cleanup(func() { _ = database.Close() })
	tx, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	_, err = federationBindingGroupProjectIDs(t.Context(), tx, db.FederationBinding{Role: db.FederationRoleHub})
	require.NoError(t, err)
	require.False(t, probe.readOnly)
	require.Contains(t, strings.ToUpper(probe.query), "FOR UPDATE")
}
