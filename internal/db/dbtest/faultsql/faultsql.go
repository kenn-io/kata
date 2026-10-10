// Package faultsql supplies a test-only database/sql driver for read failures.
package faultsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

// Script injects query results while counting whole transaction lifecycles.
type Script struct {
	Query                               func(int64) (driver.Rows, error)
	Queries, Begins, Commits, Rollbacks atomic.Int64
}

// Open creates an in-memory driver pool; it never contacts a database server.
func Open(t *testing.T, script *Script) *sql.DB {
	t.Helper()
	pool := sql.OpenDB(connector{script})
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

type connector struct{ script *Script }

func (c connector) Connect(context.Context) (driver.Conn, error) { return connection(c), nil }
func (c connector) Driver() driver.Driver                        { return faultDriver{} }

type faultDriver struct{}

func (faultDriver) Open(string) (driver.Conn, error) { return nil, errors.ErrUnsupported }

type connection struct{ script *Script }

func (connection) Prepare(string) (driver.Stmt, error) { return nil, errors.ErrUnsupported }
func (connection) Close() error                        { return nil }
func (c connection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c connection) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.script.Begins.Add(1)
	return transaction(c), nil
}
func (c connection) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return c.script.Query(c.script.Queries.Add(1))
}

type transaction struct{ script *Script }

func (t transaction) Commit() error   { t.script.Commits.Add(1); return nil }
func (t transaction) Rollback() error { t.script.Rollbacks.Add(1); return nil }

// Rows can inject a row conversion failure or a driver iteration error.
type Rows struct {
	Names  []string
	Values [][]driver.Value
	Err    error
}

// Columns implements driver.Rows for the injected result.
func (r *Rows) Columns() []string { return r.Names }

// Close releases the in-memory result, which owns no external resources.
func (*Rows) Close() error { return nil }

// Next supplies one injected row or the configured iteration error.
func (r *Rows) Next(values []driver.Value) error {
	if len(r.Values) != 0 {
		copy(values, r.Values[0])
		r.Values = r.Values[1:]
		return nil
	}
	if r.Err != nil {
		return r.Err
	}
	return io.EOF
}
