package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MCPEventSQL shares the MCP Events subscription statements across backends.
// Each backend supplies its transaction runner and, where the database needs
// it, a quota lock. SQL uses numbered placeholders supported by SQLite and
// PostgreSQL, numbered in order of first appearance.
type MCPEventSQL struct {
	Query interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
	Transact func(context.Context, func(*sql.Tx) error) error
	// LockQuota serializes quota checks and prunes. Nil means the backend's
	// writer already serializes them.
	LockQuota func(context.Context, *sql.Tx) error
}

const mcpEventColumns = `id,principal_authority,principal_subject,principal_key,actor,event_name,arguments,callback_url,
secret_enc,previous_secret_enc,previous_secret_until,verified_at,incarnation,generation,
state,end_reason,expires_at,dirty,catalog_version,pending_kind,pending_message_id,
pending_body,attempt_count,next_attempt_at,claim_token,claim_until,failed_since,failed_attempts,
last_delivery_at,last_error,dead_letter_count,ended_at,created_at,updated_at`

// mcpEventTimeLayout is fixed width so stored times compare as text.
const mcpEventTimeLayout = "2006-01-02T15:04:05.000000000Z"

func mcpEventTime(t time.Time) string { return t.UTC().Format(mcpEventTimeLayout) }

func mcpEventTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return mcpEventTime(*t)
}

func mcpEventNullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mcpEventNullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func mcpEventBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mcpEventParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse mcp event time: %w", err)
	}
	return t.UTC(), nil
}

func mcpEventParseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := mcpEventParseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func mcpEventInsertArgs(s MCPEventSubscription) []any {
	return []any{
		s.ID, s.PrincipalAuthority, s.PrincipalSubject, s.PrincipalKey, s.Actor, s.EventName, string(s.Arguments), s.CallbackURL,
		s.SecretEnc, mcpEventNullBytes(s.PreviousSecretEnc), mcpEventTimePtr(s.PreviousSecretUntil), mcpEventTimePtr(s.VerifiedAt),
		s.Incarnation, s.Generation,
		string(s.State), s.EndReason, mcpEventTimePtr(s.ExpiresAt),
		mcpEventBool(s.Dirty), s.CatalogVersion, mcpEventNullString(string(s.PendingKind)), mcpEventNullString(s.PendingMessageID),
		mcpEventNullBytes(s.PendingBody), s.AttemptCount, mcpEventTimePtr(s.NextAttemptAt),
		mcpEventNullString(s.ClaimToken), mcpEventTimePtr(s.ClaimUntil), mcpEventTimePtr(s.FailedSince), s.FailedAttempts,
		mcpEventTimePtr(s.LastDeliveryAt), s.LastError, s.DeadLetterCount, mcpEventTimePtr(s.EndedAt),
		mcpEventTime(s.CreatedAt), mcpEventTime(s.UpdatedAt),
	}
}

func mcpEventPlaceholders(n int) string { return mcpEventPlaceholdersFrom(1, n) }

// mcpEventPlaceholdersFrom numbers n placeholders from $start.
func mcpEventPlaceholdersFrom(start, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(start+i)
	}
	return strings.Join(parts, ",")
}

func scanMCPEvent(row interface{ Scan(...any) error }) (MCPEventSubscription, error) {
	var s MCPEventSubscription
	var arguments, state, pendingKind, pendingMessage, claimToken sql.NullString
	var prevUntil, verified, expires, nextAttempt, claimUntil, failedSince, lastDelivery, ended sql.NullString
	var dirty, attempts, failedAttempts, deadLetters int64
	var created, updated string
	err := row.Scan(
		&s.ID, &s.PrincipalAuthority, &s.PrincipalSubject, &s.PrincipalKey, &s.Actor, &s.EventName, &arguments, &s.CallbackURL,
		&s.SecretEnc, &s.PreviousSecretEnc, &prevUntil, &verified, &s.Incarnation, &s.Generation,
		&state, &s.EndReason, &expires, &dirty, &s.CatalogVersion, &pendingKind, &pendingMessage,
		&s.PendingBody, &attempts, &nextAttempt, &claimToken, &claimUntil, &failedSince, &failedAttempts,
		&lastDelivery, &s.LastError, &deadLetters, &ended, &created, &updated,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s, ErrNotFound
		}
		return s, err
	}
	s.Arguments = []byte(arguments.String)
	s.State = MCPEventState(state.String)
	s.PendingKind = MCPEventPendingKind(pendingKind.String)
	s.PendingMessageID = pendingMessage.String
	s.ClaimToken = claimToken.String
	s.Dirty = dirty != 0
	s.AttemptCount, s.FailedAttempts, s.DeadLetterCount = int(attempts), int(failedAttempts), int(deadLetters)
	if len(s.PreviousSecretEnc) == 0 {
		s.PreviousSecretEnc = nil
	}
	if len(s.PendingBody) == 0 {
		s.PendingBody = nil
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{
		{prevUntil, &s.PreviousSecretUntil}, {verified, &s.VerifiedAt}, {expires, &s.ExpiresAt},
		{nextAttempt, &s.NextAttemptAt}, {claimUntil, &s.ClaimUntil}, {failedSince, &s.FailedSince},
		{lastDelivery, &s.LastDeliveryAt}, {ended, &s.EndedAt},
	} {
		if *f.dst, err = mcpEventParseTimePtr(f.src); err != nil {
			return s, err
		}
	}
	if s.CreatedAt, err = mcpEventParseTime(created); err != nil {
		return s, err
	}
	s.UpdatedAt, err = mcpEventParseTime(updated)
	return s, err
}

func (a MCPEventSQL) lockQuota(ctx context.Context, tx *sql.Tx) error {
	if a.LockQuota == nil {
		return nil
	}
	return a.LockQuota(ctx, tx)
}

func mcpEventGetTx(ctx context.Context, tx *sql.Tx, id string) (MCPEventSubscription, error) {
	return scanMCPEvent(tx.QueryRowContext(ctx, "SELECT "+mcpEventColumns+" FROM mcp_event_subscriptions WHERE id=$1", id))
}

// Get reads one subscription by id.
func (a MCPEventSQL) Get(ctx context.Context, id string) (MCPEventSubscription, error) {
	return scanMCPEvent(a.Query.QueryRowContext(ctx, "SELECT "+mcpEventColumns+" FROM mcp_event_subscriptions WHERE id=$1", id))
}

func (a MCPEventSQL) list(ctx context.Context, query string, args ...any) ([]MCPEventSubscription, error) {
	rows, err := a.Query.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MCPEventSubscription
	for rows.Next() {
		s, err := scanMCPEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// List returns one principal's subscriptions in creation order.
func (a MCPEventSQL) List(ctx context.Context, principalKey string) ([]MCPEventSubscription, error) {
	return a.list(ctx, "SELECT "+mcpEventColumns+" FROM mcp_event_subscriptions WHERE principal_key=$1 ORDER BY created_at, id", principalKey)
}

// ConsentExists reports whether a verified row exists for the principal and URL.
func (a MCPEventSQL) ConsentExists(ctx context.Context, principalKey, callbackURL string) (bool, error) {
	var one int
	err := a.Query.QueryRowContext(ctx, `SELECT 1 FROM mcp_event_subscriptions WHERE principal_key=$1 AND callback_url=$2 AND verified_at IS NOT NULL LIMIT 1`, principalKey, callbackURL).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CountActive counts active rows daemon-wide.
func (a MCPEventSQL) CountActive(ctx context.Context) (int, error) {
	var n int
	err := a.Query.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_event_subscriptions WHERE state='active'`).Scan(&n)
	return n, err
}

// ListDue returns unclaimed rows with work due: active rows that are dirty
// or hold a pending ping, and ended rows holding a pending notice. A row
// waiting for its next retry is not due until next_attempt_at arrives.
func (a MCPEventSQL) ListDue(ctx context.Context, now time.Time, limit int) ([]MCPEventSubscription, error) {
	if limit <= 0 {
		return nil, nil
	}
	at := mcpEventTime(now)
	return a.list(ctx, "SELECT "+mcpEventColumns+` FROM mcp_event_subscriptions
WHERE (claim_until IS NULL OR claim_until < $1)
AND (next_attempt_at IS NULL OR next_attempt_at <= $2)
AND ((state='active' AND (dirty=1 OR pending_kind IS NOT NULL)) OR (state='ended' AND pending_kind IS NOT NULL))
ORDER BY COALESCE(next_attempt_at,''), updated_at, id LIMIT $3`, at, at, limit)
}

// MarkDirty records that new changes concern the given active subscriptions.
// It leaves updated_at alone, so it never conflicts with an activation that
// fences on it, and it is idempotent: marking a dirty row changes nothing.
func (a MCPEventSQL) MarkDirty(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	const chunk = 500
	return a.Transact(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(ids); start += chunk {
			part := ids[start:min(start+chunk, len(ids))]
			args := make([]any, len(part))
			for i, id := range part {
				args[i] = id
			}
			if _, err := tx.ExecContext(ctx, "UPDATE mcp_event_subscriptions SET dirty=1 WHERE state='active' AND dirty=0 AND id IN ("+mcpEventPlaceholders(len(part))+")", args...); err != nil { //nolint:gosec // Placeholders are generated; all values are bound.
				return err
			}
		}
		return nil
	})
}

// MCPEventCatalogUnknown is the fingerprint of a subscription whose visible
// projects may have changed unseen. It matches no computed fingerprint.
const MCPEventCatalogUnknown = "unknown"

// ForgetCatalogs stores MCPEventCatalogUnknown for active subscriptions. It
// is never fenced: an unknown fingerprint can cost a redundant ping but never
// hides a change. Like MarkDirty it leaves updated_at alone.
func (a MCPEventSQL) ForgetCatalogs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	const chunk = 500
	return a.Transact(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(ids); start += chunk {
			part := ids[start:min(start+chunk, len(ids))]
			args := []any{MCPEventCatalogUnknown}
			for _, id := range part {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE mcp_event_subscriptions SET catalog_version=$1 WHERE state='active' AND id IN ("+mcpEventPlaceholdersFrom(2, len(part))+")", args...); err != nil { //nolint:gosec // Placeholders are generated; all values are bound.
				return err
			}
		}
		return nil
	})
}

// MCPEventsCatalogEventTypes change which projects exist, so they can change
// any owner's visible projects.
var MCPEventsCatalogEventTypes = []string{"project.created", "project.removed", "project.restored", "project.merged"}

// errMCPEventCatalogsStale rolls back fingerprints a newer change outdated.
var errMCPEventCatalogsStale = errors.New("mcp event catalogs are stale")

// UpdateCatalogs records each active subscription's fingerprint of the
// projects its owner can see. A row whose stored fingerprint differs is
// marked dirty, since a project it could see came or went; a row with none
// yet only records it. Like MarkDirty it leaves updated_at alone.
//
// The fingerprints were computed after the archive head observedThrough. If
// an access change, catalog event or purge follows it, they may predate that
// change, and writing them could replace newer ones another worker stored.
// Nothing is stored then: the change lies ahead of the scan position, so a
// later scan computes the fingerprints again. The check runs after the
// updates lock their rows, so a writer that commits later has seen this one.
func (a MCPEventSQL) UpdateCatalogs(ctx context.Context, versions map[string]string, observedThrough int64) (bool, error) {
	if len(versions) == 0 {
		return true, nil
	}
	ids := slices.Sorted(maps.Keys(versions))
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE mcp_event_subscriptions
				SET dirty = CASE WHEN catalog_version <> '' AND catalog_version <> $1 THEN 1 ELSE dirty END, catalog_version = $1
				WHERE id = $2 AND state = 'active'`, versions[id], id); err != nil {
				return err
			}
		}
		args := []any{SystemProjectUID, observedThrough}
		for _, kind := range MCPEventsCatalogEventTypes {
			args = append(args, kind)
		}
		var stale bool
		//nolint:gosec // Placeholders are generated; all values are bound.
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events e JOIN projects p ON p.id = e.project_id
				WHERE p.uid = $1 AND e.id > $2
				AND e.type IN ('project.access_changed', 'team.membership_changed', 'team.actor_migrated', 'team.deleted'))
			OR EXISTS (SELECT 1 FROM events WHERE id > $2 AND type IN (`+mcpEventPlaceholdersFrom(3, len(MCPEventsCatalogEventTypes))+`))
			OR EXISTS (SELECT 1 FROM purge_log WHERE purge_reset_after_event_id > $2)
			OR EXISTS (SELECT 1 FROM project_purge_log WHERE purge_reset_after_event_id > $2)`, args...).Scan(&stale); err != nil {
			return err
		}
		if stale {
			return errMCPEventCatalogsStale
		}
		return nil
	})
	if errors.Is(err, errMCPEventCatalogsStale) {
		return false, nil
	}
	return err == nil, err
}

// ScanPosition reads the delivery worker's daemon-wide scan position.
func (a MCPEventSQL) ScanPosition(ctx context.Context) (int64, bool, error) {
	var value string
	err := a.Query.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, MetaKeyMCPEventsScanPosition).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	position, err := strconv.ParseInt(value, 10, 64)
	if err != nil || position < 0 {
		return 0, false, fmt.Errorf("mcp events scan position %q is not a non-negative integer", value)
	}
	return position, true, nil
}

// InitializeScanPosition stores head as the scan position when none exists
// and, in the same transaction, marks every active subscription dirty. That
// covers a fresh database and a restored one, whose subscribers may have
// missed changes the backup rolled back. It reports whether it initialized.
func (a MCPEventSQL) InitializeScanPosition(ctx context.Context, head int64) (bool, error) {
	if head < 0 {
		return false, errors.New("mcp events scan position must not be negative")
	}
	var initialized bool
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		initialized = false
		res, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, MetaKeyMCPEventsScanPosition, strconv.FormatInt(head, 10))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		initialized = true
		// Every subscriber reloads, so a pending rematch is moot.
		if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, MetaKeyMCPEventsRematch); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE mcp_event_subscriptions SET dirty=1 WHERE state='active'`)
		return err
	})
	return initialized, err
}

// AdvanceScanPosition moves the scan position from from to to. It reports
// false when another worker moved it first, as when daemons share a database.
func (a MCPEventSQL) AdvanceScanPosition(ctx context.Context, from, to int64) (bool, error) {
	if to < from {
		return false, errors.New("mcp events scan position cannot move backwards")
	}
	return a.execOne(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`,
		strconv.FormatInt(to, 10), MetaKeyMCPEventsScanPosition, strconv.FormatInt(from, 10))
}

// AccessChangesAfter reads the access audit events that can change what a
// principal sees. The ordinary event stream excludes the system project that
// records them.
func (a MCPEventSQL) AccessChangesAfter(ctx context.Context, after, through int64, limit int) ([]Event, error) {
	rows, err := a.Query.QueryContext(ctx, `SELECT e.id, e.uid, e.type, e.payload FROM events e JOIN projects p ON p.id = e.project_id
		WHERE p.uid = $1 AND e.id > $2 AND e.id <= $3
		AND e.type IN ('project.access_changed', 'team.membership_changed', 'team.actor_migrated', 'team.deleted')
		ORDER BY e.id LIMIT $4`, SystemProjectUID, after, through, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		e := Event{ProjectUID: SystemProjectUID}
		if err := rows.Scan(&e.ID, &e.UID, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Import validates and inserts or replaces one subscription.
func (a MCPEventSQL) Import(ctx context.Context, s MCPEventSubscription) error {
	if err := ValidateMCPEventSubscription(s); err != nil {
		return err
	}
	return a.Transact(ctx, func(tx *sql.Tx) error {
		return mcpEventUpsertTx(ctx, tx, s)
	})
}

func mcpEventUpsertTx(ctx context.Context, tx *sql.Tx, s MCPEventSubscription) error {
	cols := strings.Split(strings.ReplaceAll(mcpEventColumns, "\n", ""), ",")
	sets := make([]string, 0, len(cols)-1)
	for _, c := range cols[1:] {
		sets = append(sets, c+"=excluded."+c)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO mcp_event_subscriptions("+mcpEventColumns+") VALUES("+mcpEventPlaceholders(len(cols))+") ON CONFLICT(id) DO UPDATE SET "+strings.Join(sets, ","), mcpEventInsertArgs(s)...) //nolint:gosec // Column names and placeholders are generated from constants; all values are bound.
	return err
}

// Activate creates the row or replaces the one Expect names, enforcing the
// active-row quotas in the same transaction.
func (a MCPEventSQL) Activate(ctx context.Context, in MCPEventActivation) (MCPEventSubscription, error) {
	s := in.Subscription
	if err := ValidateMCPEventSubscription(s); err != nil {
		return MCPEventSubscription{}, err
	}
	if s.State != MCPEventStateActive {
		return MCPEventSubscription{}, errors.New("mcp event activation needs an active subscription")
	}
	if s.Incarnation == "" {
		return MCPEventSubscription{}, errors.New("mcp event activation needs an incarnation")
	}
	principalMax, daemonMax := in.PrincipalMax, in.DaemonMax
	if principalMax <= 0 {
		principalMax = MCPEventPrincipalActiveMax
	}
	if daemonMax <= 0 {
		daemonMax = MCPEventDaemonActiveMax
	}
	var out MCPEventSubscription
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.lockQuota(ctx, tx); err != nil {
			return err
		}
		current, err := mcpEventGetTx(ctx, tx, s.ID)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		switch {
		case !exists && in.Expect != nil, exists && in.Expect == nil:
			return ErrMCPEventConflict
		case exists && (current.Incarnation != in.Expect.Incarnation || current.Generation != in.Expect.Generation || !current.UpdatedAt.Equal(in.Expect.UpdatedAt)):
			return ErrMCPEventConflict
		}
		if exists && s.Incarnation != current.Incarnation {
			return errors.New("mcp event activation keeps the incarnation it replaces")
		}
		if !exists || current.State != MCPEventStateActive {
			var principalCount, daemonCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_event_subscriptions WHERE state='active' AND principal_key=$1`, s.PrincipalKey).Scan(&principalCount); err != nil {
				return err
			}
			if principalCount >= principalMax {
				return ErrMCPEventQuota{Limit: MCPEventQuotaPrincipal, Max: principalMax}
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_event_subscriptions WHERE state='active'`).Scan(&daemonCount); err != nil {
				return err
			}
			if daemonCount >= daemonMax {
				return ErrMCPEventQuota{Limit: MCPEventQuotaDaemon, Max: daemonMax}
			}
		}
		if exists {
			s.CreatedAt = current.CreatedAt
			s.Generation = in.Expect.Generation + 1
			s.ClaimToken, s.ClaimUntil = "", nil
		}
		if s.UpdatedAt.IsZero() {
			s.UpdatedAt = in.Now
		}
		if err := mcpEventWriteActivationTx(ctx, tx, s, in.Expect); err != nil {
			return err
		}
		out, err = mcpEventGetTx(ctx, tx, s.ID)
		return err
	})
	if err != nil {
		return MCPEventSubscription{}, err
	}
	return out, nil
}

// mcpEventWriteActivationTx inserts a new row, or replaces the row expect
// names only while its incarnation, generation and update time still match.
// A write committed after the caller's read is never overwritten.
func mcpEventWriteActivationTx(ctx context.Context, tx *sql.Tx, s MCPEventSubscription, expect *MCPEventExpect) error {
	cols := strings.Split(strings.ReplaceAll(mcpEventColumns, "\n", ""), ",")
	args := mcpEventInsertArgs(s)
	var res sql.Result
	var err error
	if expect == nil {
		res, err = tx.ExecContext(ctx, "INSERT INTO mcp_event_subscriptions("+mcpEventColumns+") VALUES("+mcpEventPlaceholders(len(cols))+") ON CONFLICT(id) DO NOTHING", args...) //nolint:gosec // Column names and placeholders are generated from constants; all values are bound.
	} else {
		var b mcpEventSetter
		for i := 1; i < len(cols); i++ {
			b.set(cols[i], args[i])
		}
		where := " WHERE id=" + b.arg(s.ID) + " AND incarnation=" + b.arg(expect.Incarnation) + " AND generation=" + b.arg(expect.Generation) + " AND updated_at=" + b.arg(mcpEventTime(expect.UpdatedAt))
		res, err = tx.ExecContext(ctx, "UPDATE mcp_event_subscriptions SET "+strings.Join(b.parts, ",")+where, b.args...) //nolint:gosec // Setter emits fixed column names and placeholders; all values are bound.
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrMCPEventConflict
	}
	return nil
}

// mcpEventSetter accumulates a numbered SET or WHERE clause.
type mcpEventSetter struct {
	parts []string
	args  []any
}

func (b *mcpEventSetter) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *mcpEventSetter) set(column string, v any) {
	b.parts = append(b.parts, column+"="+b.arg(v))
}

func (b *mcpEventSetter) raw(expr string) { b.parts = append(b.parts, expr) }

func (a MCPEventSQL) missingOrConflict(ctx context.Context, id string) error {
	var one int
	err := a.Query.QueryRowContext(ctx, `SELECT 1 FROM mcp_event_subscriptions WHERE id=$1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return ErrMCPEventConflict
}

// Refresh updates TTL, actor, secret columns and gap flag of an active row.
func (a MCPEventSQL) Refresh(ctx context.Context, in MCPEventRefresh) (MCPEventSubscription, error) {
	var b mcpEventSetter
	b.set("expires_at", mcpEventTimePtr(in.ExpiresAt))
	if in.Actor != "" {
		b.set("actor", in.Actor)
	}
	if in.ReplaceSecret {
		if len(in.SecretEnc) == 0 {
			return MCPEventSubscription{}, errors.New("mcp event refresh needs a secret to replace")
		}
		b.set("secret_enc", in.SecretEnc)
		b.set("previous_secret_enc", mcpEventNullBytes(in.PreviousSecretEnc))
		b.set("previous_secret_until", mcpEventTimePtr(in.PreviousSecretUntil))
	}
	if in.ClearFailures {
		b.raw("failed_since=NULL")
		b.raw("failed_attempts=0")
	}
	b.set("updated_at", mcpEventTime(in.Now))
	where := " WHERE id=" + b.arg(in.ID) + " AND incarnation=" + b.arg(in.Incarnation) + " AND state='active' AND (expires_at IS NULL OR expires_at > " + b.arg(mcpEventTime(in.Now)) + ")"
	if in.ExpectedSecretEnc != nil {
		where += " AND secret_enc=" + b.arg(in.ExpectedSecretEnc)
	}
	var out MCPEventSubscription
	var missing error
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE mcp_event_subscriptions SET "+strings.Join(b.parts, ",")+where, b.args...) //nolint:gosec // Setter emits fixed column names and placeholders; all values are bound.
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			missing = ErrMCPEventConflict
			return nil
		}
		out, err = mcpEventGetTx(ctx, tx, in.ID)
		return err
	})
	if err != nil {
		return MCPEventSubscription{}, err
	}
	if missing != nil {
		return MCPEventSubscription{}, a.missingOrConflict(ctx, in.ID)
	}
	return out, nil
}

func (a MCPEventSQL) execOne(ctx context.Context, query string, args ...any) (bool, error) {
	var matched bool
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		matched = n == 1
		return err
	})
	return matched, err
}

// Claim acquires the delivery claim with a conditional update.
func (a MCPEventSQL) Claim(ctx context.Context, in MCPEventClaim) (bool, error) {
	if in.Token == "" {
		return false, errors.New("mcp event claim needs a token")
	}
	return a.execOne(ctx, `UPDATE mcp_event_subscriptions SET claim_token=$1, claim_until=$2, updated_at=$3
WHERE id=$4 AND incarnation=$5 AND generation=$6
AND (claim_until IS NULL OR claim_token IS NULL OR claim_until < $7)
AND (claim_token IS NULL OR claim_token <> $8)
AND (next_attempt_at IS NULL OR next_attempt_at <= $7)`,
		in.Token, mcpEventTime(in.Until), mcpEventTime(in.Now), in.ID, in.Incarnation, in.Generation, mcpEventTime(in.Now), in.Token)
}

// Renew extends a claim the caller still holds.
func (a MCPEventSQL) Renew(ctx context.Context, id, incarnation string, generation int64, token string, until time.Time) (bool, error) {
	if token == "" {
		return false, errors.New("mcp event claim renewal needs a token")
	}
	return a.execOne(ctx, `UPDATE mcp_event_subscriptions SET claim_until=$1 WHERE id=$2 AND incarnation=$3 AND generation=$4 AND claim_token=$5`,
		mcpEventTime(until), id, incarnation, generation, token)
}

// UpdateDelivery applies one delivery-state transition fenced by the claim.
func (a MCPEventSQL) UpdateDelivery(ctx context.Context, in MCPEventDeliveryUpdate) (bool, error) {
	if in.ID == "" || in.Incarnation == "" || in.ClaimToken == "" {
		return false, errors.New("mcp event delivery update needs an id, incarnation and claim token")
	}
	if in.Pending != nil && in.ClearPending {
		return false, errors.New("mcp event delivery update cannot set and clear the pending slot")
	}
	if in.AttemptCount != nil && in.IncrementAttempt {
		return false, errors.New("mcp event delivery update cannot set and increment the attempt count")
	}
	if in.NextAttemptAt != nil && in.ClearNextAttempt {
		return false, errors.New("mcp event delivery update cannot set and clear the next attempt")
	}
	if in.RecordFailure && in.ClearFailure {
		return false, errors.New("mcp event delivery update cannot record and clear failure")
	}
	if in.Pending != nil {
		p := in.Pending
		switch p.Kind {
		case MCPEventPendingEvent, MCPEventPendingTerminated:
		default:
			return false, fmt.Errorf("mcp event pending kind %q is not recognized", p.Kind)
		}
		if p.MessageID == "" || len(p.Body) == 0 {
			return false, errors.New("mcp event pending delivery needs a message id and body")
		}
	}
	var b mcpEventSetter
	if in.State != nil {
		b.set("state", string(*in.State))
	}
	if in.EndReason != nil {
		b.set("end_reason", *in.EndReason)
	}
	if in.EndedAt != nil {
		b.set("ended_at", mcpEventTime(*in.EndedAt))
	}
	if in.BumpGeneration {
		b.raw("generation=generation+1")
	}
	if in.ClearDirty {
		b.raw("dirty=0")
	}
	switch {
	case in.Pending != nil:
		b.set("pending_kind", string(in.Pending.Kind))
		b.set("pending_message_id", in.Pending.MessageID)
		b.set("pending_body", in.Pending.Body)
	case in.ClearPending:
		b.raw("pending_kind=NULL")
		b.raw("pending_message_id=NULL")
		b.raw("pending_body=NULL")
	}
	switch {
	case in.AttemptCount != nil:
		b.set("attempt_count", *in.AttemptCount)
	case in.IncrementAttempt:
		b.raw("attempt_count=attempt_count+1")
	}
	switch {
	case in.NextAttemptAt != nil:
		b.set("next_attempt_at", mcpEventTime(*in.NextAttemptAt))
	case in.ClearNextAttempt:
		b.raw("next_attempt_at=NULL")
	}
	switch {
	case in.RecordFailure:
		b.parts = append(b.parts, "failed_since=COALESCE(failed_since,"+b.arg(mcpEventTime(in.Now))+")", "failed_attempts=failed_attempts+1")
	case in.ClearFailure:
		b.raw("failed_since=NULL")
		b.raw("failed_attempts=0")
	}
	if in.LastDeliveryAt != nil {
		b.set("last_delivery_at", mcpEventTime(*in.LastDeliveryAt))
	}
	if in.LastError != nil {
		b.set("last_error", *in.LastError)
	}
	if in.IncrementDeadLetter {
		b.raw("dead_letter_count=dead_letter_count+1")
	}
	if in.ReleaseClaim {
		b.raw("claim_token=NULL")
		b.raw("claim_until=NULL")
	}
	b.set("updated_at", mcpEventTime(in.Now))
	where := " WHERE id=" + b.arg(in.ID) + " AND incarnation=" + b.arg(in.Incarnation) +
		" AND generation=" + b.arg(in.Generation) + " AND claim_token=" + b.arg(in.ClaimToken)
	if in.PendingMessageID != "" {
		where += " AND pending_message_id=" + b.arg(in.PendingMessageID)
	}
	if expected := in.ExpectedFailureHistory; expected != nil {
		where += " AND failed_attempts=" + b.arg(expected.Attempts)
		if expected.Since == nil {
			where += " AND failed_since IS NULL"
		} else {
			where += " AND failed_since=" + b.arg(mcpEventTime(*expected.Since))
		}
	}
	if in.ExpectedActor != nil {
		where += " AND actor=" + b.arg(*in.ExpectedActor)
	}
	if in.RequireExpired {
		where += " AND state='active' AND expires_at IS NOT NULL AND expires_at <= " + b.arg(mcpEventTime(in.Now))
	}
	if in.RequireDeliverable {
		// The claim must also still be live: a claimant whose claim lapsed must
		// not persist an attempt another claimant may now be making.
		where += " AND claim_until IS NOT NULL AND claim_until > " + b.arg(mcpEventTime(in.Now)) +
			" AND (pending_kind='terminated' OR (state='active' AND (expires_at IS NULL OR expires_at > " + b.arg(mcpEventTime(in.Now)) + ")))"
	}
	return a.execOne(ctx, "UPDATE mcp_event_subscriptions SET "+strings.Join(b.parts, ",")+where, b.args...)
}

// End ends an active row outside a claim. Ending an ended row changes nothing.
func (a MCPEventSQL) End(ctx context.Context, in MCPEventEnd) (MCPEventSubscription, error) {
	if in.ID == "" || in.Reason == "" {
		return MCPEventSubscription{}, errors.New("mcp event end needs an id and reason")
	}
	if p := in.Terminated; p != nil {
		if p.Kind != MCPEventPendingTerminated || p.MessageID == "" || len(p.Body) == 0 {
			return MCPEventSubscription{}, errors.New("mcp event end control must be a terminated delivery with a message id and body")
		}
	}
	var out MCPEventSubscription
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		current, err := mcpEventGetTx(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if in.Incarnation != "" && in.Incarnation != current.Incarnation {
			return ErrMCPEventConflict
		}
		if current.State != MCPEventStateActive {
			out = current
			return nil
		}
		if in.Actor != nil && current.Actor != *in.Actor {
			return ErrMCPEventConflict
		}
		var b mcpEventSetter
		b.raw("state='ended'")
		b.raw("dirty=0")
		b.set("end_reason", in.Reason)
		b.set("ended_at", mcpEventTime(in.Now))
		b.raw("generation=generation+1")
		b.raw("attempt_count=0")
		b.raw("next_attempt_at=NULL")
		b.raw("claim_token=NULL")
		b.raw("claim_until=NULL")
		if p := in.Terminated; p != nil {
			b.set("pending_kind", string(p.Kind))
			b.set("pending_message_id", p.MessageID)
			b.set("pending_body", p.Body)
		} else {
			b.raw("pending_kind=NULL")
			b.raw("pending_message_id=NULL")
			b.raw("pending_body=NULL")
		}
		b.set("updated_at", mcpEventTime(in.Now))
		where := " WHERE id=" + b.arg(in.ID) + " AND incarnation=" + b.arg(current.Incarnation) + " AND state='active'"
		if in.Actor != nil {
			where += " AND actor=" + b.arg(*in.Actor)
		}
		// The generation is deliberately not fenced: ending is valid at any
		// generation, and the increment above uses the committed value, so a
		// worker's concurrent bump cannot turn this into a spurious conflict.
		res, err := tx.ExecContext(ctx, "UPDATE mcp_event_subscriptions SET "+strings.Join(b.parts, ",")+where, b.args...) //nolint:gosec // Setter emits fixed column names and placeholders; all values are bound.
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			// A concurrent writer ended or replaced the row after the read.
			latest, err := mcpEventGetTx(ctx, tx, in.ID)
			if err != nil {
				return err
			}
			if latest.Incarnation != current.Incarnation || latest.State == MCPEventStateActive {
				return ErrMCPEventConflict
			}
			out = latest
			return nil
		}
		out, err = mcpEventGetTx(ctx, tx, in.ID)
		return err
	})
	if err != nil {
		return MCPEventSubscription{}, err
	}
	return out, nil
}

// Expire ends active grants whose TTL has elapsed, independent of delivery
// backoff or claims. The expiry predicate is checked by the write itself so a
// concurrent refresh that extends the TTL wins safely. Bumping generation and
// clearing claims fence workers that still hold the expired grant's snapshot.
func (a MCPEventSQL) Expire(ctx context.Context, now time.Time) (int, error) {
	var expired int
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		expired = 0
		if err := a.lockQuota(ctx, tx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE mcp_event_subscriptions
SET state='ended', end_reason='expired', ended_at=$1, updated_at=$1,
 generation=generation+1, attempt_count=0, next_attempt_at=NULL,
 claim_token=NULL, claim_until=NULL, pending_kind=NULL,
 pending_message_id=NULL, pending_body=NULL, dirty=0
WHERE state='active' AND expires_at IS NOT NULL AND expires_at <= $1`, mcpEventTime(now))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		expired = int(n)
		return err
	})
	return expired, err
}

type mcpEventEndedRef struct {
	id, principal string
	deletable     bool
}

// Prune deletes ended rows past retention, then enforces the per-principal and
// per-daemon ended-row caps oldest first. A row with a pending control is
// never deleted.
func (a MCPEventSQL) Prune(ctx context.Context, now time.Time, caps MCPEventRetention) (int, error) {
	if caps.MaxAge <= 0 {
		caps.MaxAge = MCPEventEndedRetention
	}
	if caps.PerPrincipal <= 0 {
		caps.PerPrincipal = MCPEventEndedPrincipalMax
	}
	if caps.Daemon <= 0 {
		caps.Daemon = MCPEventEndedDaemonMax
	}
	removed := 0
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		removed = 0
		if err := a.lockQuota(ctx, tx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM mcp_event_subscriptions WHERE state='ended' AND pending_kind IS NULL AND ended_at < $1`, mcpEventTime(now.Add(-caps.MaxAge)))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed += int(n)

		rows, err := tx.QueryContext(ctx, `SELECT id, principal_key, pending_kind IS NULL FROM mcp_event_subscriptions WHERE state='ended' ORDER BY COALESCE(ended_at, updated_at), id`)
		if err != nil {
			return err
		}
		var ended []mcpEventEndedRef
		for rows.Next() {
			var r mcpEventEndedRef
			if err := rows.Scan(&r.id, &r.principal, &r.deletable); err != nil {
				_ = rows.Close()
				return err
			}
			ended = append(ended, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		drop := map[string]bool{}
		perPrincipal := map[string]int{}
		for _, r := range ended {
			perPrincipal[r.principal]++
		}
		for _, r := range ended {
			if perPrincipal[r.principal] > caps.PerPrincipal && r.deletable {
				drop[r.id] = true
				perPrincipal[r.principal]--
			}
		}
		total := len(ended) - len(drop)
		for _, r := range ended {
			if total > caps.Daemon && r.deletable && !drop[r.id] {
				drop[r.id] = true
				total--
			}
		}
		for id := range drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_event_subscriptions WHERE id=$1 AND state='ended' AND pending_kind IS NULL`, id); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

// KeyCheck reads the events key check value.
func (a MCPEventSQL) KeyCheck(ctx context.Context) (string, bool, error) {
	var value string
	err := a.Query.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, MetaKeyMCPEventsKeyCheck).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// PutKeyCheckIfAbsent stores value unless a value exists, and returns the
// stored value.
func (a MCPEventSQL) PutKeyCheckIfAbsent(ctx context.Context, value string) (string, error) {
	if value == "" {
		return "", errors.New("mcp events key check value is required")
	}
	var stored string
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, MetaKeyMCPEventsKeyCheck, value); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, MetaKeyMCPEventsKeyCheck).Scan(&stored)
	})
	return stored, err
}
