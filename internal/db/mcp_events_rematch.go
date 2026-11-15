package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MetaKeyMCPEventsRematch holds "<from>:<through>:<token>": the range of
// pulled events federation pulls have applied since an MCP Events worker last
// matched them again. Every write takes a fresh token, so a worker changes
// only the record it read. Like the scan position, it never travels in a
// backup.
const MetaKeyMCPEventsRematch = "mcp_events_rematch"

// RecordMCPEventsRematch notes, inside the transaction that applies pulled
// events, that the MCP Events worker must match them again. A pull commits
// remote events before it applies them, so a worker on any daemon sharing the
// store may already have matched them against projections the pull had not
// updated. Recording it in the applying transaction means neither a crash nor
// an Events-disabled pulling daemon can lose it.
func RecordMCPEventsRematch(ctx context.Context, tx *sql.Tx, eventUIDs []string) error {
	var from, through int64
	const chunk = 500
	for start := 0; start < len(eventUIDs); start += chunk {
		part := eventUIDs[start:min(start+chunk, len(eventUIDs))]
		args := make([]any, len(part))
		places := make([]string, len(part))
		for i, uid := range part {
			args[i], places[i] = uid, "$"+strconv.Itoa(i+1)
		}
		var low, high sql.NullInt64
		//nolint:gosec // Placeholders are generated; all values are bound.
		if err := tx.QueryRowContext(ctx, "SELECT MIN(id), MAX(id) FROM events WHERE uid IN ("+strings.Join(places, ",")+")", args...).Scan(&low, &high); err != nil {
			return fmt.Errorf("find applied events for mcp events rematch: %w", err)
		}
		if low.Valid && (from == 0 || low.Int64 < from) {
			from = low.Int64
		}
		if high.Valid {
			through = max(through, high.Int64)
		}
	}
	if from == 0 {
		return nil
	}
	// PostgreSQL reads committed data, so a read before the write would miss
	// a concurrent pull or acknowledgment. The insert waits for another
	// uncommitted insert, and the locking update for any uncommitted change;
	// the read after them sees what that writer committed. A record
	// acknowledged away in between is inserted again.
	for range 8 {
		value, err := mcpEventsRematchValue(from, through)
		if err != nil {
			return err
		}
		inserted, err := execAffected(ctx, tx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, MetaKeyMCPEventsRematch, value)
		if err != nil || inserted == 1 {
			return mcpEventsRematchErr(err)
		}
		locked, err := execAffected(ctx, tx, `UPDATE meta SET value=value WHERE key=$1`, MetaKeyMCPEventsRematch)
		if err != nil {
			return mcpEventsRematchErr(err)
		}
		if locked == 0 {
			continue
		}
		current, _, err := readMCPEventsRematch(ctx, tx)
		if err != nil {
			return err
		}
		value, err = mcpEventsRematchValue(min(from, current.from), max(through, current.through))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE meta SET value=$2 WHERE key=$1`, MetaKeyMCPEventsRematch, value)
		return mcpEventsRematchErr(err)
	}
	return errors.New("record mcp events rematch: the record kept changing")
}

// execAffected runs one statement and reports how many rows it changed.
func execAffected(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func mcpEventsRematchErr(err error) error {
	if err != nil {
		return fmt.Errorf("record mcp events rematch: %w", err)
	}
	return nil
}

type mcpEventsRematch struct {
	from, through int64
	token         string
}

func parseMCPEventsRematch(value string) (mcpEventsRematch, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return mcpEventsRematch{}, false
	}
	from, err1 := strconv.ParseInt(parts[0], 10, 64)
	through, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || from <= 0 || through < from {
		return mcpEventsRematch{}, false
	}
	return mcpEventsRematch{from: from, through: through, token: value}, true
}

func readMCPEventsRematch(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (mcpEventsRematch, bool, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, MetaKeyMCPEventsRematch).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpEventsRematch{}, false, nil
	}
	if err != nil {
		return mcpEventsRematch{}, false, fmt.Errorf("read mcp events rematch: %w", err)
	}
	record, ok := parseMCPEventsRematch(value)
	if !ok {
		return mcpEventsRematch{}, false, fmt.Errorf("mcp events rematch %q is malformed", value)
	}
	return record, true, nil
}

// mcpEventsRematchValue encodes a range with a fresh token.
func mcpEventsRematchValue(from, through int64) (string, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return strconv.FormatInt(from, 10) + ":" + strconv.FormatInt(through, 10) + ":" + hex.EncodeToString(nonce), nil
}

// Rematch reads the pending rematch record: the range of applied pulled
// events and the token acknowledging it requires.
func (a MCPEventSQL) Rematch(ctx context.Context) (from, through int64, token string, ok bool, err error) {
	record, ok, err := readMCPEventsRematch(ctx, a.Query)
	return record.from, record.through, record.token, ok, err
}

// AcknowledgeRematch records that a worker matched the applied events through
// matchedThrough again. The record is removed when that covers it, or else
// starts after matchedThrough, and only while it still holds token: a pull
// applied meanwhile wrote a new token, so its record survives untouched. The
// token is the value itself, so the conditional write is the whole check.
func (a MCPEventSQL) AcknowledgeRematch(ctx context.Context, token string, matchedThrough int64) (bool, error) {
	record, ok := parseMCPEventsRematch(token)
	if !ok || matchedThrough < record.from {
		return false, nil
	}
	var done bool
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		var n int64
		var err error
		if matchedThrough >= record.through {
			n, err = execAffected(ctx, tx, `DELETE FROM meta WHERE key=$1 AND value=$2`, MetaKeyMCPEventsRematch, token)
		} else {
			var value string
			if value, err = mcpEventsRematchValue(matchedThrough+1, record.through); err != nil {
				return err
			}
			n, err = execAffected(ctx, tx, `UPDATE meta SET value=$3 WHERE key=$1 AND value=$2`, MetaKeyMCPEventsRematch, token, value)
		}
		done = n == 1
		return err
	})
	return done, err
}
