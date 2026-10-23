package db

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"time"
)

// ImportKindMCPEventSubscription is the JSONL kind of a stored MCP Events
// subscription. Only full backups and cutover carry it.
const ImportKindMCPEventSubscription = "mcp_event_subscription"

// MCPEventSubscriptionExport is the portable full-backup form of one row of
// mcp_event_subscriptions. It carries every column, including sealed secrets,
// pending bodies and the incarnation, so a restored row resumes where the
// original stopped. Callback URLs and secrets are sensitive; never log it.
type MCPEventSubscriptionExport struct {
	ID                   string     `json:"id"`
	PrincipalAuthority   string     `json:"principal_authority"`
	PrincipalSubject     string     `json:"principal_subject"`
	PrincipalKey         string     `json:"principal_key"`
	Actor                string     `json:"actor"`
	EventName            string     `json:"event_name"`
	Arguments            string     `json:"arguments"`
	CallbackURL          string     `json:"callback_url"`
	SecretEnc            []byte     `json:"secret_enc"`
	PreviousSecretEnc    []byte     `json:"previous_secret_enc,omitzero"`
	PreviousSecretUntil  *time.Time `json:"previous_secret_until,omitzero"`
	VerifiedAt           *time.Time `json:"verified_at,omitzero"`
	Incarnation          string     `json:"incarnation"`
	Generation           int64      `json:"generation"`
	ContinuityGeneration int64      `json:"continuity_generation"`
	ReplayCutoff         *time.Time `json:"replay_cutoff,omitzero"`
	ReplayHead           *int64     `json:"replay_head,omitzero"`
	State                string     `json:"state"`
	EndReason            string     `json:"end_reason"`
	ExpiresAt            *time.Time `json:"expires_at,omitzero"`
	CursorEventID        int64      `json:"cursor_event_id"`
	GapUnacked           bool       `json:"gap_unacked"`
	PendingKind          string     `json:"pending_kind,omitzero"`
	PendingMessageID     string     `json:"pending_message_id,omitzero"`
	PendingBody          []byte     `json:"pending_body,omitzero"`
	PendingNextCursor    *int64     `json:"pending_next_cursor,omitzero"`
	AttemptCount         int        `json:"attempt_count"`
	NextAttemptAt        *time.Time `json:"next_attempt_at,omitzero"`
	ClaimToken           string     `json:"claim_token,omitzero"`
	ClaimUntil           *time.Time `json:"claim_until,omitzero"`
	FailedSince          *time.Time `json:"failed_since,omitzero"`
	FailedAttempts       int        `json:"failed_attempts"`
	LastDeliveryAt       *time.Time `json:"last_delivery_at,omitzero"`
	LastError            string     `json:"last_error"`
	DeadLetterCount      int        `json:"dead_letter_count"`
	EndedAt              *time.Time `json:"ended_at,omitzero"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// ImportKind identifies a stored MCP Events subscription record.
func (*MCPEventSubscriptionExport) ImportKind() string { return ImportKindMCPEventSubscription }

// MCPEventSubscriptionExportOf converts a stored row to its export form.
func MCPEventSubscriptionExportOf(s MCPEventSubscription) MCPEventSubscriptionExport {
	return MCPEventSubscriptionExport{
		ID: s.ID, PrincipalAuthority: s.PrincipalAuthority, PrincipalSubject: s.PrincipalSubject, PrincipalKey: s.PrincipalKey,
		Actor: s.Actor, EventName: s.EventName, Arguments: string(s.Arguments), CallbackURL: s.CallbackURL,
		SecretEnc: s.SecretEnc, PreviousSecretEnc: s.PreviousSecretEnc, PreviousSecretUntil: s.PreviousSecretUntil,
		VerifiedAt: s.VerifiedAt, Incarnation: s.Incarnation, Generation: s.Generation,
		ContinuityGeneration: s.ContinuityGeneration, ReplayCutoff: s.ReplayCutoff, ReplayHead: s.ReplayHead,
		State: string(s.State), EndReason: s.EndReason, ExpiresAt: s.ExpiresAt, CursorEventID: s.CursorEventID,
		GapUnacked: s.GapUnacked, PendingKind: string(s.PendingKind), PendingMessageID: s.PendingMessageID,
		PendingBody: s.PendingBody, PendingNextCursor: s.PendingNextCursor, AttemptCount: s.AttemptCount,
		NextAttemptAt: s.NextAttemptAt, ClaimToken: s.ClaimToken, ClaimUntil: s.ClaimUntil, FailedSince: s.FailedSince,
		FailedAttempts: s.FailedAttempts, LastDeliveryAt: s.LastDeliveryAt, LastError: s.LastError,
		DeadLetterCount: s.DeadLetterCount, EndedAt: s.EndedAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

// Subscription converts the export form back to a stored row.
func (e MCPEventSubscriptionExport) Subscription() MCPEventSubscription {
	return MCPEventSubscription{
		ID: e.ID, PrincipalAuthority: e.PrincipalAuthority, PrincipalSubject: e.PrincipalSubject, PrincipalKey: e.PrincipalKey,
		Actor: e.Actor, EventName: e.EventName, Arguments: []byte(e.Arguments), CallbackURL: e.CallbackURL,
		SecretEnc: e.SecretEnc, PreviousSecretEnc: e.PreviousSecretEnc, PreviousSecretUntil: e.PreviousSecretUntil,
		VerifiedAt: e.VerifiedAt, Incarnation: e.Incarnation, Generation: e.Generation,
		ContinuityGeneration: e.ContinuityGeneration, ReplayCutoff: e.ReplayCutoff, ReplayHead: e.ReplayHead,
		State: MCPEventState(e.State), EndReason: e.EndReason, ExpiresAt: e.ExpiresAt, CursorEventID: e.CursorEventID,
		GapUnacked: e.GapUnacked, PendingKind: MCPEventPendingKind(e.PendingKind), PendingMessageID: e.PendingMessageID,
		PendingBody: e.PendingBody, PendingNextCursor: e.PendingNextCursor, AttemptCount: e.AttemptCount,
		NextAttemptAt: e.NextAttemptAt, ClaimToken: e.ClaimToken, ClaimUntil: e.ClaimUntil, FailedSince: e.FailedSince,
		FailedAttempts: e.FailedAttempts, LastDeliveryAt: e.LastDeliveryAt, LastError: e.LastError,
		DeadLetterCount: e.DeadLetterCount, EndedAt: e.EndedAt, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

// ExportMCPEventSubscriptionsSQL streams every subscription for a full
// backup. A project-scoped filter yields nothing: subscriptions belong to
// principals, not projects, and callbacks must not follow partial data.
func ExportMCPEventSubscriptionsSQL(ctx context.Context, query CronQuery, filter ExportFilter) iter.Seq2[MCPEventSubscriptionExport, error] {
	return func(yield func(MCPEventSubscriptionExport, error) bool) {
		if filter.ProjectID != nil {
			return
		}
		rows, err := query(ctx, "SELECT "+mcpEventColumns+" FROM mcp_event_subscriptions ORDER BY id")
		if err != nil {
			yield(MCPEventSubscriptionExport{}, err)
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			s, err := scanMCPEvent(rows)
			if !yield(MCPEventSubscriptionExportOf(s), err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(MCPEventSubscriptionExport{}, err)
		}
	}
}

// ReplayMCPEventSubscription validates and writes one backup row inside the
// backend's restore transaction. Full restore and cutover only.
func ReplayMCPEventSubscription(ctx context.Context, tx *sql.Tx, record *MCPEventSubscriptionExport) error {
	if record == nil {
		return fmt.Errorf("nil mcp event subscription")
	}
	s := record.Subscription()
	if err := ValidateMCPEventSubscription(s); err != nil {
		return err
	}
	return mcpEventUpsertTx(ctx, tx, s)
}
