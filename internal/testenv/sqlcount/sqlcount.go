// Package sqlcount counts the SQL statements one request runs through a
// database/sql driver. A store opened through a wrapped driver counts only
// statements whose context carries a Counter, so concurrent background work on
// the same pool does not inflate a request's count.
package sqlcount

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync/atomic"
)

// Counter records the statements, including BEGIN, COMMIT and ROLLBACK, run
// under contexts returned by Context.
type Counter struct{ n atomic.Int64 }

type counterKey struct{}

// Context returns ctx marked so wrapped connections count its statements.
func (c *Counter) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, counterKey{}, c)
}

// Count returns the statements recorded since the last Reset.
func (c *Counter) Count() int64 { return c.n.Load() }

// Reset clears the count.
func (c *Counter) Reset() { c.n.Store(0) }

func record(ctx context.Context) *Counter {
	c, _ := ctx.Value(counterKey{}).(*Counter)
	if c != nil {
		c.n.Add(1)
	}
	return c
}

// WrapDriver counts statements on every connection inner opens.
func WrapDriver(inner driver.Driver) driver.Driver { return countingDriver{inner} }

// WrapConnector counts statements on every connection inner connects.
func WrapConnector(inner driver.Connector) driver.Connector { return countingConnector{inner} }

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return wrapConn(conn)
}

type countingConnector struct{ inner driver.Connector }

func (c countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wrapConn(conn)
}

func (c countingConnector) Driver() driver.Driver { return countingDriver{c.inner.Driver()} }

// innerConn is the context-aware surface both pgx and modernc SQLite
// connections implement; the wrapper forwards all of it.
type innerConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
}

func wrapConn(conn driver.Conn) (driver.Conn, error) {
	inner, ok := conn.(innerConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("sqlcount: driver connection lacks context support")
	}
	if checker, ok := conn.(driver.NamedValueChecker); ok {
		return &checkingConn{conn0: conn0{inner}, checker: checker}, nil
	}
	return &conn0{inner}, nil
}

type conn0 struct{ inner innerConn }

// checkingConn keeps the driver's own argument conversion, which pgx needs.
type checkingConn struct {
	conn0
	checker driver.NamedValueChecker
}

func (c *checkingConn) CheckNamedValue(v *driver.NamedValue) error {
	return c.checker.CheckNamedValue(v)
}

func (c *conn0) Prepare(query string) (driver.Stmt, error) { return c.inner.Prepare(query) }
func (c *conn0) Close() error                              { return c.inner.Close() }

func (c *conn0) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn0) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	counter := record(ctx)
	tx, err := c.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return countingTx{inner: tx, counter: counter}, nil
}

func (c *conn0) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.inner.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return countingStmt{stmt}, nil
}

func (c *conn0) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	record(ctx)
	return c.inner.ExecContext(ctx, query, args)
}

func (c *conn0) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	record(ctx)
	return c.inner.QueryContext(ctx, query, args)
}

func (c *conn0) Ping(ctx context.Context) error         { return c.inner.Ping(ctx) }
func (c *conn0) ResetSession(ctx context.Context) error { return c.inner.ResetSession(ctx) }

// IsValid forwards when the driver reports validity; pgx does not.
func (c *conn0) IsValid() bool {
	validator, ok := c.inner.(driver.Validator)
	return !ok || validator.IsValid()
}

type countingTx struct {
	inner   driver.Tx
	counter *Counter
}

func (t countingTx) Commit() error {
	if t.counter != nil {
		t.counter.n.Add(1)
	}
	return t.inner.Commit()
}

func (t countingTx) Rollback() error {
	if t.counter != nil {
		t.counter.n.Add(1)
	}
	return t.inner.Rollback()
}

// countingStmt counts each execution of a prepared statement.
type countingStmt struct{ driver.Stmt }

func (s countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	record(ctx)
	exec, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("sqlcount: prepared statement lacks context support")
	}
	return exec.ExecContext(ctx, args)
}

func (s countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	record(ctx)
	query, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("sqlcount: prepared statement lacks context support")
	}
	return query.QueryContext(ctx, args)
}
