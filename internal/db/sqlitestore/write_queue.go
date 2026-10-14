package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

// sqliteBusyTimeout bounds how long one write waits for SQLite's writer,
// both in the busy_timeout pragma and in the in-process write queue.
const sqliteBusyTimeout = 5 * time.Second

// writeQueue admits one in-process SQLite writer at a time, in arrival order.
//
// SQLite's busy handler makes a blocked writer sleep and poll, so whichever
// writer happens to be ready when the lock frees wins it. A writer that
// restarts as soon as it commits, such as back-to-back federation
// materialization, can then keep the lock while paced local writes exhaust
// their busy timeout. The queue hands the writer slot directly to the oldest
// waiter instead, so every queued writer gets a turn and no time is lost
// between turns. Writers in other processes still meet busy_timeout.
type writeQueue struct {
	mu      sync.Mutex
	held    bool
	waiters []chan struct{}
	maxWait time.Duration
}

func newWriteQueue(maxWait time.Duration) *writeQueue {
	return &writeQueue{maxWait: maxWait}
}

// acquire waits for the writer slot. It gives up when ctx ends or after
// maxWait, which reports busy contention so callers retry as they would for a
// SQLite busy error.
func (q *writeQueue) acquire(ctx context.Context) error {
	q.mu.Lock()
	if !q.held {
		q.held = true
		q.mu.Unlock()
		return nil
	}
	turn := make(chan struct{})
	q.waiters = append(q.waiters, turn)
	q.mu.Unlock()

	timer := time.NewTimer(q.maxWait)
	defer timer.Stop()
	var err error
	select {
	case <-turn:
		return nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = writeQueueBusyError{waited: q.maxWait}
	}
	q.mu.Lock()
	for i, waiter := range q.waiters {
		if waiter == turn {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			q.mu.Unlock()
			return err
		}
	}
	q.mu.Unlock()
	// release handed the slot over while this waiter gave up; pass it on.
	q.release()
	return err
}

// release hands the writer slot to the oldest waiter, or frees it.
func (q *writeQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiters) == 0 {
		q.held = false
		return
	}
	next := q.waiters[0]
	q.waiters = q.waiters[1:]
	close(next)
}

// writeQueueBusyError reports a write that waited maxWait without reaching
// the front of the queue. It carries SQLITE_BUSY so IsTransient retries it.
type writeQueueBusyError struct {
	waited time.Duration
}

func (e writeQueueBusyError) Error() string {
	return fmt.Sprintf("database is locked: waited %s for the write queue", e.waited)
}

func (writeQueueBusyError) Code() int { return sqlite3.SQLITE_BUSY }

type optimisticWriteKey struct{}

// withOptimisticWrite lets a write transaction begun with ctx read before it
// joins the write queue. Its first write then joins, and SQLite reports busy
// contention if another writer committed after its snapshot, so the caller
// must retry the whole transaction.
func withOptimisticWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, optimisticWriteKey{}, true)
}

// openQueuedSQLite opens dsn with the registered modernc driver behind a
// connector whose connections share one write queue.
func openQueuedSQLite(dsn string) (*sql.DB, error) {
	drv, err := registeredSQLiteDriver()
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(queuedConnector{dsn: dsn, driver: drv, queue: newWriteQueue(sqliteBusyTimeout)}), nil
}

// registeredSQLiteDriver returns the driver registered as "sqlite", keeping
// any functions, collations, and connection hooks registered on it.
var registeredSQLiteDriver = sync.OnceValues(func() (driver.Driver, error) {
	probe, err := sql.Open("sqlite", "")
	if err != nil {
		return nil, err
	}
	drv := probe.Driver()
	return drv, probe.Close()
})

// sqliteConn is the driver connection surface modernc implements and the
// queued connection forwards.
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type queuedConnector struct {
	dsn    string
	driver driver.Driver
	queue  *writeQueue
}

func (c queuedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	inner, ok := conn.(sqliteConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite driver connection %T lacks context support", conn)
	}
	return &queuedConn{inner: inner, queue: c.queue}, nil
}

func (c queuedConnector) Driver() driver.Driver { return c.driver }

// queuedConn admits each write transaction through the write queue. A
// transaction joins when it begins, before it reads: a snapshot read while
// waiting would be stale by its turn, and SQLite would then refuse its first
// write. Read-only transactions never join. A transaction begun with
// withOptimisticWrite reads without the queue and joins at its first write.
// Autocommit writes hold the queue for the single statement. database/sql
// never uses one driver connection concurrently, so the transaction state
// needs no lock.
type queuedConn struct {
	inner   sqliteConn
	queue   *writeQueue
	inTx    bool
	holding bool
}

func (c *queuedConn) Prepare(query string) (driver.Stmt, error) { return c.inner.Prepare(query) }

func (c *queuedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.inner.PrepareContext(ctx, query)
}

func (c *queuedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *queuedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if !opts.ReadOnly && ctx.Value(optimisticWriteKey{}) == nil {
		if err := c.join(ctx); err != nil {
			return nil, err
		}
	}
	tx, err := c.inner.BeginTx(ctx, opts)
	if err != nil {
		c.endTx()
		return nil, err
	}
	c.inTx = true
	return &queuedTx{conn: c, inner: tx}, nil
}

func (c *queuedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	switch classifyStatement(query) {
	case statementBegin:
		joined := !c.holding
		if err := c.join(ctx); err != nil {
			return nil, err
		}
		result, err := c.inner.ExecContext(ctx, query, args)
		if err != nil {
			if joined {
				c.holding = false
				c.queue.release()
			}
			return nil, err
		}
		c.inTx = true
		return result, nil
	case statementCommit:
		result, err := c.inner.ExecContext(ctx, query, args)
		if err == nil {
			c.endTx()
		}
		return result, err
	case statementRollback:
		result, err := c.inner.ExecContext(ctx, query, args)
		c.endTx()
		return result, err
	case statementWrite:
		if !c.inTx {
			if err := c.queue.acquire(ctx); err != nil {
				return nil, err
			}
			defer c.queue.release()
			return c.inner.ExecContext(ctx, query, args)
		}
		if err := c.join(ctx); err != nil {
			return nil, err
		}
	}
	return c.inner.ExecContext(ctx, query, args)
}

// QueryContext joins the queue for writes inside a transaction. Autocommit
// queries pass straight through; a write among them still meets busy_timeout.
func (c *queuedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.inTx && classifyStatement(query) == statementWrite {
		if err := c.join(ctx); err != nil {
			return nil, err
		}
	}
	return c.inner.QueryContext(ctx, query, args)
}

func (c *queuedConn) Ping(ctx context.Context) error { return c.inner.Ping(ctx) }

func (c *queuedConn) ResetSession(ctx context.Context) error { return c.inner.ResetSession(ctx) }

func (c *queuedConn) IsValid() bool { return c.inner.IsValid() }

func (c *queuedConn) Close() error {
	c.endTx()
	return c.inner.Close()
}

// join takes the writer slot for the open transaction unless it already
// holds it.
func (c *queuedConn) join(ctx context.Context) error {
	if c.holding {
		return nil
	}
	if err := c.queue.acquire(ctx); err != nil {
		return err
	}
	c.holding = true
	return nil
}

func (c *queuedConn) endTx() {
	c.inTx = false
	if c.holding {
		c.holding = false
		c.queue.release()
	}
}

// queuedTx releases the writer slot when the transaction ends. modernc rolls
// back a failed commit, so either call always ends the transaction.
type queuedTx struct {
	conn  *queuedConn
	inner driver.Tx
}

func (t *queuedTx) Commit() error {
	defer t.conn.endTx()
	return t.inner.Commit()
}

func (t *queuedTx) Rollback() error {
	defer t.conn.endTx()
	return t.inner.Rollback()
}

type statementKind int

const (
	statementRead statementKind = iota
	statementWrite
	statementBegin
	statementCommit
	statementRollback
)

// classifyStatement decides from a statement's leading keywords whether it
// needs SQLite's writer lock. Unknown statements count as writes: wrongly
// queuing a read only costs concurrency, while a write that skips the queue
// falls back to busy_timeout polling.
func classifyStatement(query string) statementKind {
	first, rest := nextSQLWord(query)
	switch strings.ToUpper(first) {
	case "", "SELECT", "VALUES", "EXPLAIN":
		return statementRead
	case "WITH":
		for word := ""; rest != ""; {
			word, rest = nextSQLWord(rest)
			switch strings.ToUpper(word) {
			case "INSERT", "UPDATE", "DELETE", "REPLACE":
				return statementWrite
			}
		}
		return statementRead
	case "BEGIN":
		return statementBegin
	case "COMMIT", "END":
		return statementCommit
	case "ROLLBACK":
		// ROLLBACK [TRANSACTION] TO rewinds a savepoint without ending the
		// transaction.
		second, rest := nextSQLWord(rest)
		third, _ := nextSQLWord(rest)
		if strings.EqualFold(second, "TO") || strings.EqualFold(third, "TO") {
			return statementWrite
		}
		return statementRollback
	default:
		return statementWrite
	}
}

// nextSQLWord returns the next bare word in query, skipping comments, and the
// text after it. Quoted identifiers and literals are not parsed; a keyword
// inside one can only make a read look like a write.
func nextSQLWord(query string) (string, string) {
	for i := 0; i < len(query); {
		switch {
		case strings.HasPrefix(query[i:], "--"):
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				return "", ""
			}
			i += end + 1
		case strings.HasPrefix(query[i:], "/*"):
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return "", ""
			}
			i += end + 4
		case isSQLWordByte(query[i]):
			start := i
			for i < len(query) && isSQLWordByte(query[i]) {
				i++
			}
			return query[start:i], query[i:]
		default:
			i++
		}
	}
	return "", ""
}

func isSQLWordByte(b byte) bool {
	return b == '_' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}
