package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/db"
)

func validMCPEventSubscription() db.MCPEventSubscription {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	head := int64(40)
	next := int64(42)
	return db.MCPEventSubscription{
		ID:                   "sub_0123456789abcdef",
		PrincipalAuthority:   "daemon.example",
		PrincipalSubject:     "worker",
		PrincipalKey:         "daemon.example:worker",
		Actor:                "worker",
		EventName:            "kata.inbox.changed",
		Arguments:            []byte(`{"for":"worker"}`),
		CallbackURL:          "https://receiver.example/hook",
		SecretEnc:            []byte("ciphertext"),
		PreviousSecretEnc:    []byte("previous"),
		PreviousSecretUntil:  &at,
		VerifiedAt:           &at,
		Incarnation:          "0123456789abcdef0123456789abcdef",
		Generation:           3,
		ContinuityGeneration: 1,
		ReplayCutoff:         &at,
		ReplayHead:           &head,
		State:                db.MCPEventStateActive,
		ExpiresAt:            &at,
		CursorEventID:        41,
		GapUnacked:           true,
		PendingKind:          db.MCPEventPendingEvent,
		PendingMessageID:     "42",
		PendingBody:          []byte(`{"id":"42"}`),
		PendingNextCursor:    &next,
		AttemptCount:         2,
		NextAttemptAt:        &at,
		ClaimToken:           "token",
		ClaimUntil:           &at,
		FailedSince:          &at,
		FailedAttempts:       2,
		LastDeliveryAt:       &at,
		LastError:            "http_5xx",
		DeadLetterCount:      1,
		CreatedAt:            at,
		UpdatedAt:            at,
	}
}

func TestValidateMCPEventSubscription(t *testing.T) {
	t.Parallel()
	ended := func(s *db.MCPEventSubscription) {
		at := s.CreatedAt
		s.State = db.MCPEventStateEnded
		s.EndReason = "unsubscribed"
		s.EndedAt = &at
		s.PendingKind = db.MCPEventPendingTerminated
		s.PendingMessageID = "msg_terminated_0123"
		s.PendingBody = []byte(`{"type":"terminated"}`)
	}
	tests := []struct {
		name    string
		mutate  func(*db.MCPEventSubscription)
		wantErr string
	}{
		{name: "fully populated active row"},
		{name: "ended row with pending terminated", mutate: ended},
		{name: "no pending delivery", mutate: func(s *db.MCPEventSubscription) {
			s.PendingKind, s.PendingMessageID, s.PendingBody, s.PendingNextCursor = db.MCPEventPendingNone, "", nil, nil
		}},
		{name: "empty id", mutate: func(s *db.MCPEventSubscription) { s.ID = "" }, wantErr: "id"},
		{name: "missing sub_ prefix", mutate: func(s *db.MCPEventSubscription) { s.ID = "abc123" }, wantErr: "id"},
		{name: "unknown state", mutate: func(s *db.MCPEventSubscription) { s.State = "paused" }, wantErr: "state"},
		{name: "empty state", mutate: func(s *db.MCPEventSubscription) { s.State = "" }, wantErr: "state"},
		{name: "unknown pending kind", mutate: func(s *db.MCPEventSubscription) { s.PendingKind = "retry" }, wantErr: "pending kind"},
		{name: "pending without message id", mutate: func(s *db.MCPEventSubscription) { s.PendingMessageID = "" }, wantErr: "pending"},
		{name: "pending without body", mutate: func(s *db.MCPEventSubscription) { s.PendingBody = nil }, wantErr: "pending"},
		{name: "active with end reason", mutate: func(s *db.MCPEventSubscription) { s.EndReason = "expired" }, wantErr: "end reason"},
		{name: "negative generation", mutate: func(s *db.MCPEventSubscription) { s.Generation = -1 }, wantErr: "generation"},
		{name: "negative continuity generation", mutate: func(s *db.MCPEventSubscription) { s.ContinuityGeneration = -1 }, wantErr: "continuity"},
		{name: "negative cursor", mutate: func(s *db.MCPEventSubscription) { s.CursorEventID = -1 }, wantErr: "cursor"},
		{name: "negative attempt count", mutate: func(s *db.MCPEventSubscription) { s.AttemptCount = -1 }, wantErr: "attempt"},
		{name: "negative failed attempts", mutate: func(s *db.MCPEventSubscription) { s.FailedAttempts = -1 }, wantErr: "failed"},
		{name: "negative dead letter count", mutate: func(s *db.MCPEventSubscription) { s.DeadLetterCount = -1 }, wantErr: "dead letter"},
		{name: "principal key mismatch", mutate: func(s *db.MCPEventSubscription) { s.PrincipalKey = "daemon.example:other" }, wantErr: "principal key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := validMCPEventSubscription()
			if tt.mutate != nil {
				tt.mutate(&s)
			}
			err := db.ValidateMCPEventSubscription(s)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
