package mcpevents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// Grant and quota values (design spec §7.3).
const (
	defaultTTL          = 24 * time.Hour
	minTTL              = 60 * time.Second
	defaultMaxTTL       = 30 * 24 * time.Hour
	previousSecretGrace = 60 * time.Second
	// maxActivationAttempts bounds retries after a concurrent change.
	maxActivationAttempts = 4
)

// Reasons an otherwise running Service reports for being disabled.
const (
	ReasonKeyUnavailable = "events_key_unavailable"
	ReasonKeyMismatch    = "events_key_mismatch"
	ReasonNotConfigured  = "events_not_configured"
	// ReasonStartFailed reports a host that could not construct the service.
	ReasonStartFailed = "events_start_failed"
)

// Sealed secret roles, bound into the AEAD associated data.
const (
	secretRoleCurrent  = "current"
	secretRolePrevious = "previous"
)

// Limits override the active-subscription quotas. Zero selects the defaults.
type Limits struct {
	PrincipalActive int
	DaemonActive    int
}

// ServiceConfig configures a Service.
type ServiceConfig struct {
	Store db.Storage
	// Keys supplies the Events key. Nil disables Events with a status reason.
	Keys KeyProvider
	// Authority reconstructs a persisted principal's authorization.
	Authority Authority
	// Webhook performs the endpoint verification challenge. Nil selects the
	// production client.
	Webhook *WebhookClient
	// Now is the service clock for every grant, retry and aging decision. Nil
	// selects time.Now.
	Now func() time.Time
	// MaxTTL caps finite grants. Zero selects 30 days.
	MaxTTL time.Duration
	// RefuseNoExpiry gives a no-expiry request a MaxTTL grant instead.
	RefuseNoExpiry bool
	Limits         Limits

	// Broadcaster wakes the delivery worker when the archive changes. Nil
	// leaves only the fallback ticker.
	Broadcaster Subscriber

	// The fields below tune delivery. Zero values select production defaults;
	// they exist so tests and embedders can avoid real waiting.

	// PollInterval is the fallback wake ticker (default 5 s).
	PollInterval time.Duration
	// RenewInterval is how often a claimant renews its claim (default 20 s). A
	// value at or above the claim TTL less its margin (45 s) uses the default.
	RenewInterval time.Duration
	// Concurrency bounds deliveries per process (default 8).
	Concurrency int
	// Jitter returns a value in [0, 1) scaling the 10 % retry jitter. Nil
	// selects a random source.
	Jitter func() float64
	// DrainHook, when set, runs after every wake-driven drain cycle.
	DrainHook func()
}

// Availability reports whether the Service accepts subscriptions.
type Availability struct {
	Enabled bool
	Reason  string
}

// Service owns daemon-side MCP Events subscriptions: list, subscribe,
// unsubscribe and status. All durable state lives in the store.
type Service struct {
	store     db.Storage
	authority Authority
	webhook   *WebhookClient
	now       func() time.Time
	maxTTL    time.Duration
	// refuseNoExpiry clamps no-expiry requests to maxTTL.
	refuseNoExpiry bool
	limits         Limits

	key   []byte
	avail Availability

	challenges *challengeCoordinator
	mu         sync.Mutex
	inflight   map[string]int

	delivery *deliveryState
}

// NewService opens a Service over cfg.Store. A missing, unreadable or
// mismatched key does not fail construction: the Service starts disabled and
// reports why, so ordinary daemon operation continues. An existing key check
// with no key never causes a new key to be created. ctx bounds key loading and
// the startup writes; canceling it leaves the Service disabled.
func NewService(ctx context.Context, cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil {
		return nil, errNoStore
	}
	s := &Service{
		store: cfg.Store, authority: cfg.Authority, webhook: cfg.Webhook, now: cfg.Now, maxTTL: cfg.MaxTTL, refuseNoExpiry: cfg.RefuseNoExpiry, limits: cfg.Limits,
		inflight: map[string]int{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.delivery = newDeliveryState(cfg)
	if s.maxTTL <= 0 {
		s.maxTTL = defaultMaxTTL
	}
	if s.limits.PrincipalActive <= 0 {
		s.limits.PrincipalActive = db.MCPEventPrincipalActiveMax
	}
	if s.limits.DaemonActive <= 0 {
		s.limits.DaemonActive = db.MCPEventDaemonActiveMax
	}
	if s.webhook == nil {
		s.webhook = NewWebhookClient()
	}
	s.challenges = newChallengeCoordinator(s.now)
	if cfg.Keys == nil || cfg.Authority == nil {
		s.avail = Availability{Reason: ReasonNotConfigured}
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := cfg.Keys.Load(ctx)
	if errors.Is(err, ErrKeyMissing) {
		_, existing, checkErr := cfg.Store.MCPEventsKeyCheck(ctx)
		if checkErr != nil {
			return nil, fmt.Errorf("read MCP Events key check: %w", checkErr)
		}
		if existing {
			// The database was written under a key this process does not have,
			// as after restoring a backup. Never replace it.
			s.avail = Availability{Reason: ReasonKeyUnavailable}
			return s, nil
		}
		key, err = cfg.Keys.Create(ctx)
	}
	if err != nil || len(key) != keySize {
		s.avail = Availability{Reason: ReasonKeyUnavailable}
		return s, nil
	}
	check := KeyCheck(key)
	stored, err := cfg.Store.PutMCPEventsKeyCheckIfAbsent(ctx, check)
	if err != nil {
		return nil, fmt.Errorf("record MCP Events key check: %w", err)
	}
	if stored != check {
		s.avail = Availability{Reason: ReasonKeyMismatch}
		return s, nil
	}
	// Start scanning at the current head. On a fresh database this marks
	// nothing; after a restore it marks every restored subscription dirty
	// once, because the restore may have rolled its inbox back.
	head, err := s.storeHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("read MCP Events archive head: %w", err)
	}
	if _, err := cfg.Store.InitializeMCPEventsScanPosition(ctx, head); err != nil {
		return nil, fmt.Errorf("start MCP Events scan position: %w", err)
	}
	s.key = key
	s.avail = Availability{Enabled: true}
	return s, nil
}

// Availability reports whether Events is enabled and, when not, why.
func (s *Service) Availability() Availability { return s.avail }

func (s *Service) ready() error {
	if !s.avail.Enabled {
		return unavailable(s.avail.Reason)
	}
	return nil
}

func validatePrincipal(p Principal) error {
	if !p.Valid() {
		return forbidden("principal")
	}
	return nil
}

// List returns the event definitions.
func (s *Service) List(_ context.Context, p Principal) (ListResult, error) {
	if err := s.ready(); err != nil {
		return ListResult{}, err
	}
	if err := validatePrincipal(p); err != nil {
		return ListResult{}, err
	}
	return ListResult{Events: []Definition{definition()}}, nil
}

// identity is a validated subscription key.
type identity struct {
	args      Arguments
	canonical []byte
	url       string
	parsed    *url.URL
	id        string
}

func canonicalArguments(args Arguments) []byte {
	b, _ := json.Marshal(args, json.Deterministic(true))
	return b
}

// SubscriptionID derives sub_ plus the SHA-256 over the length-prefixed
// principal key, event name, canonical arguments and callback URL.
func SubscriptionID(principalKey, eventName string, canonicalArgs []byte, callbackURL string) string {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(principalKey), []byte(eventName), canonicalArgs, []byte(callbackURL)} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		_, _ = h.Write(n[:])
		_, _ = h.Write(part)
	}
	return db.MCPEventSubscriptionIDPrefix + hex.EncodeToString(h.Sum(nil))
}

func canonicalULID(raw string) bool {
	id, err := ulid.ParseStrict(raw)
	return err == nil && id.String() == raw
}

func normalizeArguments(args Arguments) (Arguments, error) {
	recipient, err := notification.NormalizeRecipient(args.For)
	if err != nil {
		return args, invalidParams("invalid_arguments")
	}
	args.For = recipient
	if args.ProjectUIDs != nil {
		if len(args.ProjectUIDs) == 0 || len(args.ProjectUIDs) > MaxProjects {
			return args, invalidParams("invalid_project_uids")
		}
		for _, uid := range args.ProjectUIDs {
			if !canonicalULID(uid) {
				return args, invalidParams("invalid_project_uids")
			}
		}
		// A list that is not sorted ascending and free of duplicates is
		// rejected, never rewritten: the identity compares canonical JSON.
		if !sortedUnique(args.ProjectUIDs) {
			return args, invalidParams("project_uids_not_sorted_unique")
		}
	}
	if args.IssueUID != "" && !canonicalULID(args.IssueUID) {
		return args, invalidParams("invalid_issue_uid")
	}
	return args, nil
}

func (s *Service) identify(p Principal, name string, args Arguments, callbackURL string) (identity, error) {
	if name != EventName {
		return identity{}, notFound("event")
	}
	args, err := normalizeArguments(args)
	if err != nil {
		return identity{}, err
	}
	parsed, err := parseWebhookCallback(callbackURL)
	if err != nil {
		return identity{}, invalidParams("invalid_callback")
	}
	canonical := canonicalArguments(args)
	return identity{args: args, canonical: canonical, url: callbackURL, parsed: parsed, id: SubscriptionID(p.Key(), name, canonical, callbackURL)}, nil
}

// checkForwarderScope enforces §5.2: every forwarder mode except all-projects
// rejects an absent selection or one outside the projects it serves. It never
// rewrites the selection.
func checkForwarderScope(scope ForwarderScope, args Arguments) error {
	if scope.All {
		return nil
	}
	if args.ProjectUIDs == nil {
		return forbidden("project_scope")
	}
	served := make(map[string]bool, len(scope.Projects))
	for _, uid := range scope.Projects {
		served[uid] = true
	}
	for _, uid := range args.ProjectUIDs {
		if !served[uid] {
			return forbidden("project_scope")
		}
	}
	return nil
}

// grantExpiry applies §7.3: absent is 24 hours, a finite value clamps to
// [60 seconds, max TTL], null is no expiry unless the operator refuses it.
// No value is rejected.
func (s *Service) grantExpiry(now time.Time, ttl TTLMS) *time.Time {
	var d time.Duration
	switch {
	case ttl.IsNull() && s.refuseNoExpiry:
		d = s.maxTTL
	case ttl.IsNull():
		return nil
	case !ttl.Present():
		d = min(defaultTTL, s.maxTTL)
	case ttl.value >= s.maxTTL.Milliseconds():
		d = s.maxTTL
	case ttl.value <= minTTL.Milliseconds():
		d = minTTL
	default:
		d = time.Duration(ttl.value) * time.Millisecond
	}
	d = max(d, minTTL)
	t := now.Add(d).UTC()
	return &t
}

func newIncarnation() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate subscription incarnation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func authorityError(err error) error {
	if errors.Is(err, ErrPrincipalRevoked) {
		return forbidden("principal_revoked")
	}
	if errors.Is(err, ErrScopeRemoved) {
		return forbidden("scope_removed")
	}
	if errors.Is(err, ErrOccurrenceHidden) {
		return forbidden("issue_not_visible")
	}
	return unavailable("authority_unavailable")
}

func live(row db.MCPEventSubscription, now time.Time) bool {
	return row.State == db.MCPEventStateActive && (row.ExpiresAt == nil || row.ExpiresAt.After(now))
}

func resultOf(row db.MCPEventSubscription) SubscribeResult {
	return SubscribeResult{ID: row.ID, RefreshBefore: row.ExpiresAt, DeliveryStatus: deliveryStatusOf(row)}
}

func deliveryStatusOf(row db.MCPEventSubscription) DeliveryStatus {
	return DeliveryStatus{
		Active: row.State == db.MCPEventStateActive, LastDeliveryAt: row.LastDeliveryAt,
		LastError: row.LastError, FailedSince: row.FailedSince,
	}
}

// Subscribe creates, refreshes or reactivates a subscription (§7.3).
func (s *Service) Subscribe(ctx context.Context, p Principal, scope ForwarderScope, req SubscribeRequest) (SubscribeResult, error) {
	if err := s.ready(); err != nil {
		return SubscribeResult{}, err
	}
	if err := validatePrincipal(p); err != nil {
		return SubscribeResult{}, err
	}
	ident, err := s.identify(p, req.Name, req.Arguments, req.Delivery.URL)
	if err != nil {
		return SubscribeResult{}, err
	}
	if req.Delivery.Mode != "webhook" {
		return SubscribeResult{}, unsupported("deliveryMode")
	}
	if err := ValidateWebhookSecret(req.Delivery.Secret); err != nil {
		return SubscribeResult{}, invalidParams("invalid_secret")
	}
	if err := checkForwarderScope(scope, ident.args); err != nil {
		return SubscribeResult{}, err
	}
	// The fingerprint activation stores describes the archive at this head.
	resolvedAfter, err := s.storeHead(db.WithoutIssueRegistries(db.WithAuthorizedProjects(ctx, nil)))
	if err != nil {
		return SubscribeResult{}, unavailable("store_unavailable")
	}
	auth, err := s.authority.Reacquire(ctx, p)
	if err != nil {
		if errors.Is(err, ErrPrincipalRevoked) {
			s.endLive(ctx, ident.id, p.Actor, "principal_revoked")
		}
		return SubscribeResult{}, authorityError(err)
	}
	projects, err := ResolveProjects(ctx, auth, ident.args)
	if err != nil {
		if errors.Is(err, ErrScopeRemoved) {
			s.endLive(ctx, ident.id, p.Actor, "scope_removed")
		}
		return SubscribeResult{}, authorityError(err)
	}
	if ident.args.IssueUID != "" {
		if err := auth.AuthorizeOccurrence(ctx, Occurrence{IssueUID: ident.args.IssueUID}); err != nil {
			return SubscribeResult{}, authorityError(err)
		}
	}
	if err := s.webhook.validateStaticCallback(ctx, ident.parsed); err != nil {
		return SubscribeResult{}, callbackEndpoint(endpointCategory(err))
	}
	call := subscribeCall{p: p, ident: ident, req: req, auth: auth, projects: projects, resolvedAfter: resolvedAfter}
	for range maxActivationAttempts {
		result, retry, err := s.subscribeOnce(ctx, call)
		if !retry {
			return result, err
		}
	}
	return SubscribeResult{}, unavailable("concurrent_update")
}

type subscribeCall struct {
	p        Principal
	ident    identity
	req      SubscribeRequest
	auth     Authorized
	projects []Project
	// resolvedAfter is the archive head read before projects was resolved.
	resolvedAfter int64
}

// subscribeOnce reads the current row and applies one transition. retry
// reports that a concurrent writer moved the row, so the caller starts over.
func (s *Service) subscribeOnce(ctx context.Context, c subscribeCall) (result SubscribeResult, retry bool, err error) {
	now := s.now().UTC()
	row, err := s.store.GetMCPEventSubscription(ctx, c.ident.id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return s.activate(ctx, c, nil, now)
	case err != nil:
		return SubscribeResult{}, false, unavailable("store_unavailable")
	}
	if !live(row, now) {
		return s.activate(ctx, c, &row, now)
	}
	return s.refresh(ctx, c, row, now)
}

// refresh is the live-row path. It touches only the TTL, the actor and the
// secret columns, so it never overwrites concurrent delivery progress.
func (s *Service) refresh(ctx context.Context, c subscribeCall, row db.MCPEventSubscription, now time.Time) (SubscribeResult, bool, error) {
	if err := s.webhook.validateCallback(ctx, c.ident.parsed); err != nil {
		return SubscribeResult{}, false, callbackEndpoint(endpointCategory(err))
	}
	in := db.MCPEventRefresh{
		ID: row.ID, Incarnation: row.Incarnation, Now: now, Actor: c.p.Actor, ExpectedSecretEnc: row.SecretEnc,
		ExpiresAt: s.grantExpiry(now, c.req.TTLMS), ClearFailures: true,
	}
	current, err := Open(s.key, row.ID, secretRoleCurrent, row.SecretEnc)
	if err != nil || subtle.ConstantTimeCompare(current, []byte(c.req.Delivery.Secret)) != 1 {
		sealed, sealErr := Seal(s.key, row.ID, secretRoleCurrent, []byte(c.req.Delivery.Secret))
		if sealErr != nil {
			return SubscribeResult{}, false, unavailable("seal_failed")
		}
		in.ReplaceSecret, in.SecretEnc = true, sealed
		if err == nil {
			previous, sealErr := Seal(s.key, row.ID, secretRolePrevious, current)
			if sealErr != nil {
				return SubscribeResult{}, false, unavailable("seal_failed")
			}
			until := now.Add(previousSecretGrace)
			in.PreviousSecretEnc, in.PreviousSecretUntil = previous, &until
		}
	}
	updated, err := s.store.RefreshMCPEventSubscription(ctx, in)
	switch {
	case errors.Is(err, db.ErrMCPEventConflict), errors.Is(err, db.ErrNotFound):
		return SubscribeResult{}, true, nil
	case err != nil:
		return SubscribeResult{}, false, unavailable("store_unavailable")
	}
	return resultOf(updated), false, nil
}

// reserve snapshots committed rows and reserves a new slot under the same lock
// used by activation commits. A committed activation is never counted again as
// an outstanding verification. The returned release requires s.mu to be held.
func (s *Service) reserve(ctx context.Context, principalKey string, needsSlot bool) ([]db.MCPEventSubscription, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.store.ListMCPEventSubscriptions(ctx, principalKey)
	if err != nil {
		return nil, nil, unavailable("store_unavailable")
	}
	active := 0
	for _, row := range rows {
		if row.State == db.MCPEventStateActive {
			active++
		}
	}
	if needsSlot && active+s.inflight[principalKey]+1 > s.limits.PrincipalActive {
		return nil, nil, resourceExhausted("subscriptions", s.limits.PrincipalActive)
	}
	// Reactivating an expired active row reuses its committed slot.
	reserved := needsSlot
	if reserved {
		s.inflight[principalKey]++
	}
	return rows, func() {
		if !reserved {
			return
		}
		reserved = false
		s.inflight[principalKey]--
		if s.inflight[principalKey] == 0 {
			delete(s.inflight, principalKey)
		}
	}, nil
}

// activate creates a row, or reactivates a non-live one with generation + 1.
func (s *Service) activate(ctx context.Context, c subscribeCall, prior *db.MCPEventSubscription, now time.Time) (SubscribeResult, bool, error) {
	principalKey := c.p.Key()
	rows, release, err := s.reserve(ctx, principalKey, prior == nil || prior.State != db.MCPEventStateActive)
	if err != nil {
		return SubscribeResult{}, false, err
	}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		release()
	}()
	var consentAt *time.Time
	for _, r := range rows {
		if r.CallbackURL == c.ident.url && r.VerifiedAt != nil && (consentAt == nil || r.VerifiedAt.Before(*consentAt)) {
			t := *r.VerifiedAt
			consentAt = &t
		}
	}

	if prior == nil || prior.State != db.MCPEventStateActive {
		// Activate stays authoritative; this only avoids a challenge that the
		// daemon-wide cap would make pointless.
		total, err := s.store.CountActiveMCPEventSubscriptions(ctx)
		if err != nil {
			return SubscribeResult{}, false, unavailable("store_unavailable")
		}
		if total >= s.limits.DaemonActive {
			return SubscribeResult{}, false, resourceExhausted("daemon_subscriptions", s.limits.DaemonActive)
		}
	}

	row := db.MCPEventSubscription{
		ID: c.ident.id, PrincipalAuthority: c.p.Authority, PrincipalSubject: c.p.Subject, PrincipalKey: principalKey, Actor: c.p.Actor,
		EventName: EventName, Arguments: c.ident.canonical, CallbackURL: c.ident.url,
		State: db.MCPEventStateActive, ExpiresAt: s.grantExpiry(now, c.req.TTLMS),
		Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	uids := make([]string, len(c.projects))
	for i, project := range c.projects {
		uids[i] = project.UID
	}
	row.CatalogVersion = catalogVersion(uids)
	var expect *db.MCPEventExpect
	if prior != nil {
		expect = &db.MCPEventExpect{Incarnation: prior.Incarnation, Generation: prior.Generation, UpdatedAt: prior.UpdatedAt}
		row.Incarnation = prior.Incarnation
		row.DeadLetterCount, row.LastDeliveryAt = prior.DeadLetterCount, prior.LastDeliveryAt
	} else if row.Incarnation, err = newIncarnation(); err != nil {
		return SubscribeResult{}, false, unavailable("incarnation_unavailable")
	}

	sealed, err := Seal(s.key, row.ID, secretRoleCurrent, []byte(c.req.Delivery.Secret))
	if err != nil {
		return SubscribeResult{}, false, unavailable("seal_failed")
	}
	row.SecretEnc = sealed
	freshlyVerified := false
	if consentAt != nil {
		row.VerifiedAt = consentAt
	} else {
		if err := s.challenges.verify(ctx, principalKey, c.ident.url, c.ident.parsed, func(ctx context.Context) error {
			if err := s.webhook.Verify(ctx, c.ident.parsed, row.ID, c.req.Delivery.Secret); err != nil {
				if ctx.Err() != nil {
					// The caller gave up; the endpoint did not fail.
					return ctx.Err()
				}
				return callbackEndpoint(endpointCategory(err))
			}
			freshlyVerified = true
			return nil
		}); err != nil {
			if _, ok := errors.AsType[*Error](err); ok {
				return SubscribeResult{}, false, err
			}
			return SubscribeResult{}, false, unavailable("verification_interrupted")
		}
		t := s.now().UTC()
		row.VerifiedAt = &t
	}

	// Durable, recent and coalesced consent can skip the dial. Recheck those
	// destinations before writing, without resolving twice for a fresh challenge.
	if !freshlyVerified {
		if err := s.webhook.validateCallback(ctx, c.ident.parsed); err != nil {
			return SubscribeResult{}, false, callbackEndpoint(endpointCategory(err))
		}
	}

	s.catchUp(ctx)
	s.mu.Lock()
	stored, err := s.store.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{
		Subscription: row, Expect: expect, Now: now,
		PrincipalMax: s.limits.PrincipalActive, DaemonMax: s.limits.DaemonActive,
	})
	release()
	s.mu.Unlock()
	if err != nil {
		var quota db.ErrMCPEventQuota
		switch {
		case errors.Is(err, db.ErrMCPEventConflict):
			return SubscribeResult{}, true, nil
		case errors.As(err, &quota) && quota.Limit == db.MCPEventQuotaPrincipal:
			return SubscribeResult{}, false, resourceExhausted("subscriptions", s.limits.PrincipalActive)
		case errors.As(err, &quota):
			return SubscribeResult{}, false, resourceExhausted("daemon_subscriptions", quota.Max)
		}
		return SubscribeResult{}, false, unavailable("store_unavailable")
	}
	if err := s.settleCatalog(ctx, stored.ID, c.resolvedAfter); err != nil {
		// Without the check, assume a change was missed. The row is already
		// active, so a failure here costs at most a stale fingerprint, as a
		// failed catch-up costs at most a redundant ping.
		_ = s.store.ForgetMCPEventCatalogs(ctx, []string{stored.ID})
	}
	return resultOf(stored), false, nil
}

// Unsubscribe ends the caller's active subscription. It requires only
// ownership, which the key encodes, so a narrowed forwarder can still clean up.
func (s *Service) Unsubscribe(ctx context.Context, p Principal, req UnsubscribeRequest) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := validatePrincipal(p); err != nil {
		return err
	}
	ident, err := s.identify(p, req.Name, req.Arguments, req.Delivery.URL)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	// A conflict means the row was replaced between the read and the write, so
	// read it again. An ended or missing row is already unsubscribed.
	for range 3 {
		row, err := s.store.GetMCPEventSubscription(ctx, ident.id)
		if errors.Is(err, db.ErrNotFound) {
			return notFound("subscription")
		}
		if err != nil {
			return unavailable("store_unavailable")
		}
		if !live(row, now) {
			return notFound("subscription")
		}
		// Ending bumps the generation, which cancels any claim and pending
		// event. Unsubscribe sends no control body.
		_, err = s.store.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: row.ID, Incarnation: row.Incarnation, Reason: "unsubscribed", Now: now})
		switch {
		case err == nil:
			return nil
		case errors.Is(err, db.ErrNotFound):
			return notFound("subscription")
		case errors.Is(err, db.ErrMCPEventConflict):
			continue
		default:
			return unavailable("store_unavailable")
		}
	}
	return unavailable("store_unavailable")
}

// Status lists the caller's subscriptions with delivery diagnostics only.
func (s *Service) Status(ctx context.Context, p Principal) ([]SubscriptionStatus, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := validatePrincipal(p); err != nil {
		return nil, err
	}
	rows, err := s.store.ListMCPEventSubscriptions(ctx, p.Key())
	if err != nil {
		return nil, unavailable("store_unavailable")
	}
	out := make([]SubscriptionStatus, 0, len(rows))
	for _, row := range rows {
		// A row whose arguments cannot be read is still listed, with no
		// recipient or selection, so it cannot hide the caller's other rows.
		// The worker ends such a row; retention later removes it.
		var args Arguments
		if json.Unmarshal(row.Arguments, &args) != nil {
			args = Arguments{}
		}
		out = append(out, SubscriptionStatus{
			ID: row.ID, Name: row.EventName, Recipient: args.For, ProjectUIDs: args.ProjectUIDs, IssueUID: args.IssueUID,
			State: string(row.State), EndReason: row.EndReason, RefreshBefore: row.ExpiresAt, Delivery: deliveryStatusOf(row),
			FailedAttempts: row.FailedAttempts, DeadLetterCount: row.DeadLetterCount, PendingControl: string(row.PendingKind),
		})
	}
	return out, nil
}

// endLive ends the principal's active row for id, if any, with a terminated
// notice (§9.4), when actor's authority was found lost. A row a concurrent
// refresh moved to another actor is left for the next check under that
// actor. It is best effort.
func (s *Service) endLive(ctx context.Context, id, actor, reason string) {
	row, err := s.store.GetMCPEventSubscription(ctx, id)
	if err != nil || row.State != db.MCPEventStateActive {
		return
	}
	body, msgID, err := terminatedBody(reason)
	if err != nil {
		return
	}
	_, _ = s.store.EndMCPEventSubscription(ctx, db.MCPEventEnd{
		ID: row.ID, Incarnation: row.Incarnation, Reason: reason, Now: s.now().UTC(), Actor: &actor,
		Terminated: &db.MCPEventPending{Kind: db.MCPEventPendingTerminated, MessageID: msgID, Body: body},
	})
}

// terminatedBody builds the terminated control (§9.4) and its stable
// webhook-id. The body carries no archive data.
func terminatedBody(reason string) (body []byte, messageID string, err error) {
	code := CodeForbidden
	if reason == "event_removed" {
		code = CodeNotFound
	}
	body, err = json.Marshal(map[string]any{
		"type":  "terminated",
		"error": map[string]any{"code": code, "message": WireName(code), "data": map[string]any{"reason": reason}},
	}, json.Deterministic(true))
	if err != nil {
		return nil, "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", err
	}
	return body, "msg_terminated_" + hex.EncodeToString(random[:]), nil
}
