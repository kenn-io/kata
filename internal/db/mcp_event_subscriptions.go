package db

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MCPEventState is the lifecycle state of a stored MCP Events subscription.
type MCPEventState string

const (
	// MCPEventStateActive marks a subscription that can receive deliveries.
	MCPEventStateActive MCPEventState = "active"
	// MCPEventStateEnded marks a subscription kept only to finish pending
	// control deliveries and for retention.
	MCPEventStateEnded MCPEventState = "ended"
)

// MCPEventPendingKind names the kind of delivery held in a row's pending slot.
type MCPEventPendingKind string

const (
	// MCPEventPendingNone means the row holds no pending delivery.
	MCPEventPendingNone MCPEventPendingKind = ""
	// MCPEventPendingEvent is a pending "inbox changed" ping.
	MCPEventPendingEvent MCPEventPendingKind = "event"
	// MCPEventPendingTerminated is a pending termination notice.
	MCPEventPendingTerminated MCPEventPendingKind = "terminated"
)

// MCPEventSubscriptionIDPrefix starts every stored subscription id.
const MCPEventSubscriptionIDPrefix = "sub_"

// MCPEventSubscription is one row of mcp_event_subscriptions. Time fields are
// UTC. Callback URLs, secrets and bodies are sensitive; never log them.
type MCPEventSubscription struct {
	ID                  string
	PrincipalAuthority  string
	PrincipalSubject    string
	PrincipalKey        string
	Actor               string
	EventName           string
	Arguments           []byte // canonical JSON
	CallbackURL         string
	SecretEnc           []byte
	PreviousSecretEnc   []byte
	PreviousSecretUntil *time.Time
	VerifiedAt          *time.Time
	Incarnation         string
	Generation          int64
	State               MCPEventState
	EndReason           string
	ExpiresAt           *time.Time // nil means no expiry
	// Dirty records that a change concerning this subscription has not yet
	// been put in a ping.
	Dirty bool
	// CatalogVersion fingerprints the projects the owner could see when the
	// subscription was last told about them. A change marks it dirty.
	CatalogVersion   string
	PendingKind      MCPEventPendingKind
	PendingMessageID string
	PendingBody      []byte
	AttemptCount     int
	NextAttemptAt    *time.Time
	ClaimToken       string
	ClaimUntil       *time.Time
	FailedSince      *time.Time
	FailedAttempts   int
	LastDeliveryAt   *time.Time
	LastError        string
	DeadLetterCount  int
	EndedAt          *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ValidateMCPEventSubscription checks the invariants the schema does not
// enforce. Stores and JSONL import share it so both reject the same rows.
func ValidateMCPEventSubscription(s MCPEventSubscription) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !strings.HasPrefix(s.ID, MCPEventSubscriptionIDPrefix) || len(s.ID) == len(MCPEventSubscriptionIDPrefix) {
		fail("mcp event subscription id must start with %q and have a suffix", MCPEventSubscriptionIDPrefix)
	}
	switch s.State {
	case MCPEventStateActive, MCPEventStateEnded:
	default:
		fail("mcp event subscription state %q is not active or ended", s.State)
	}
	switch s.PendingKind {
	case MCPEventPendingNone, MCPEventPendingEvent, MCPEventPendingTerminated:
	default:
		fail("mcp event subscription pending kind %q is not recognized", s.PendingKind)
	}
	if s.PendingKind != MCPEventPendingNone && (s.PendingMessageID == "" || len(s.PendingBody) == 0) {
		fail("mcp event subscription pending delivery needs a message id and body")
	}
	if s.State == MCPEventStateActive && s.EndReason != "" {
		fail("mcp event subscription end reason requires state ended")
	}
	for _, c := range []struct {
		name  string
		value int64
	}{
		{"generation", s.Generation},
		{"attempt count", int64(s.AttemptCount)},
		{"failed attempts", int64(s.FailedAttempts)},
		{"dead letter count", int64(s.DeadLetterCount)},
	} {
		if c.value < 0 {
			fail("mcp event subscription %s must not be negative", c.name)
		}
	}
	if s.PrincipalKey != s.PrincipalAuthority+":"+s.PrincipalSubject {
		fail("mcp event subscription principal key must be authority:subject")
	}
	return errors.Join(errs...)
}

// Quota and retention limits (design spec §7.3 and §9.4). Operations accept
// zero-value overrides that select these defaults.
const (
	// MCPEventPrincipalActiveMax caps active rows per principal.
	MCPEventPrincipalActiveMax = 128
	// MCPEventDaemonActiveMax caps active rows per daemon.
	MCPEventDaemonActiveMax = 4096
	// MCPEventEndedPrincipalMax caps retained ended rows per principal.
	MCPEventEndedPrincipalMax = 256
	// MCPEventEndedDaemonMax caps retained ended rows per daemon.
	MCPEventEndedDaemonMax = 8192
	// MCPEventEndedRetention is how long an ended row without a pending
	// control is kept after ended_at.
	MCPEventEndedRetention = 7 * 24 * time.Hour
)

// MCPEventQuotaScope names which active-row limit an activation exceeded.
type MCPEventQuotaScope string

const (
	// MCPEventQuotaPrincipal is the per-principal limit.
	MCPEventQuotaPrincipal MCPEventQuotaScope = "principal"
	// MCPEventQuotaDaemon is the per-daemon limit.
	MCPEventQuotaDaemon MCPEventQuotaScope = "daemon"
)

// ErrMCPEventConflict means a conditional write found a different row than
// its predicate named: a stale incarnation, generation or secret snapshot, an
// existing row, or a row that is not active.
var ErrMCPEventConflict = errors.New("mcp event subscription conflict")

// ErrMCPEventQuota reports an exceeded active-row limit. Use errors.As with a
// value of this type.
type ErrMCPEventQuota struct {
	Limit MCPEventQuotaScope
	Max   int
}

// Error implements error.
func (e ErrMCPEventQuota) Error() string {
	return fmt.Sprintf("mcp event subscription %s quota of %d exceeded", e.Limit, e.Max)
}

// MCPEventExpect names the row an activation replaces.
type MCPEventExpect struct {
	Incarnation string
	Generation  int64
	// UpdatedAt fences activation against refreshes that preserve generation.
	UpdatedAt time.Time
}

// MCPEventActivation creates a subscription row when none exists (Expect nil)
// or replaces the row Expect names. Subscription carries every column to
// store. When the row is new or not yet active, the active-row quotas are
// checked in the same transaction. Zero PrincipalMax or DaemonMax select the
// package defaults.
type MCPEventActivation struct {
	Subscription MCPEventSubscription
	Expect       *MCPEventExpect
	Now          time.Time
	PrincipalMax int
	DaemonMax    int
}

// MCPEventRefresh updates only the TTL, actor and secret columns of an
// active row. ExpiresAt nil means no expiry. SecretEnc and the previous-secret
// columns change only when ReplaceSecret is set. ClearFailures resets the
// failure aging counters, as a successful refresh does (§9.2).
type MCPEventRefresh struct {
	ID          string
	Incarnation string
	Now         time.Time
	// Actor replaces the stored actor snapshot when set.
	Actor     string
	ExpiresAt *time.Time
	// ExpectedSecretEnc, when set, fences refresh against concurrent rotation.
	ExpectedSecretEnc   []byte
	ReplaceSecret       bool
	SecretEnc           []byte
	PreviousSecretEnc   []byte
	PreviousSecretUntil *time.Time
	ClearFailures       bool
}

// MCPEventClaim acquires the delivery claim. Token must be fresh for every
// acquisition; a claim never reuses the token it replaces.
type MCPEventClaim struct {
	ID          string
	Incarnation string
	Generation  int64
	Token       string
	Now         time.Time
	Until       time.Time
}

// MCPEventPending is the delivery held in a row's pending slot.
type MCPEventPending struct {
	Kind      MCPEventPendingKind
	MessageID string
	Body      []byte
}

// MCPEventFailureHistory identifies the endpoint failure run used to decide
// whether a delivery may end a subscription. A refresh clears this history
// without replacing the pending delivery or its claim.
type MCPEventFailureHistory struct {
	Since    *time.Time
	Attempts int
}

// MCPEventDeliveryUpdate is the single conditional writer for delivery state.
// ID, Incarnation, Generation and ClaimToken form the predicate, plus
// PendingMessageID when set. Pointer fields set a column; the bool fields
// increment or clear one. RequireDeliverable adds the attempt-start recheck:
// the claim has not lapsed at Now, and the row is active and unexpired at Now
// or holds a terminated control. A ping requires a live grant.
type MCPEventDeliveryUpdate struct {
	ID               string
	Incarnation      string
	Generation       int64
	ClaimToken       string
	PendingMessageID string
	Now              time.Time

	RequireDeliverable bool
	// RequireExpired fences expiry against a refresh: the row must still be
	// active with a non-null expires_at at or before Now.
	RequireExpired bool
	// ExpectedActor fences an end decided from one actor's authority against
	// a refresh that renamed the row's actor meanwhile.
	ExpectedActor *string
	// ExpectedFailureHistory fences decisions derived from failure aging
	// against a refresh that cleared that history during a delivery attempt.
	ExpectedFailureHistory *MCPEventFailureHistory

	State          *MCPEventState
	EndReason      *string
	EndedAt        *time.Time
	BumpGeneration bool
	// ClearDirty clears the dirty flag, as staging a ping does.
	ClearDirty          bool
	Pending             *MCPEventPending
	ClearPending        bool
	AttemptCount        *int
	IncrementAttempt    bool
	NextAttemptAt       *time.Time
	ClearNextAttempt    bool
	RecordFailure       bool
	ClearFailure        bool
	LastDeliveryAt      *time.Time
	LastError           *string
	IncrementDeadLetter bool
	ReleaseClaim        bool
}

// MCPEventEnd ends an active row outside a claim. Incarnation, when set, must
// match. Terminated queues a pending terminated control.
type MCPEventEnd struct {
	ID          string
	Incarnation string
	Reason      string
	Now         time.Time
	Terminated  *MCPEventPending
	// Actor, when set, ends the row only while its stored actor still equals
	// it. An end decided from one actor's authority must not apply to a row
	// a concurrent refresh moved to a renamed actor.
	Actor *string
}

// MCPEventRetention bounds retained ended rows. Zero fields select the
// package defaults.
type MCPEventRetention struct {
	MaxAge       time.Duration
	PerPrincipal int
	Daemon       int
}

// MetaKeyMCPEventsKeyCheck is the meta key holding the events key check value.
const MetaKeyMCPEventsKeyCheck = "mcp_events_key_check"

// MetaKeyMCPEventsScanPosition is the meta key holding the highest event id
// the delivery worker has matched against subscriptions. It never travels in
// a backup: a database without it marks every active subscription dirty once.
const MetaKeyMCPEventsScanPosition = "mcp_events_scan_position"
