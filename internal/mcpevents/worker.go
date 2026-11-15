package mcpevents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	mathrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/kata/internal/db"
)

// Subscriber is the wake source: one subscription to archive changes. The
// returned channel receives a value when something may have changed and is
// closed when the subscriber fell behind (overflow); the worker then
// resubscribes and drains from the database. cancel releases the
// subscription and is safe to call more than once.
type Subscriber interface {
	Subscribe() (wake <-chan struct{}, cancel func())
}

// Delivery values (design spec §8 and §9).
const (
	defaultPollInterval  = 5 * time.Second
	defaultRenewInterval = 20 * time.Second
	defaultConcurrency   = 8
	claimTTL             = 60 * time.Second
	sweepInterval        = time.Minute
	// authorityRecheck bounds how long a subscription whose owner was revoked,
	// or whose explicit selection lost every project, stays active while no
	// change is delivered to it.
	authorityRecheck = 5 * time.Second
	// failureBackoff keeps a row that could not be processed (authority or
	// store outage) from being retried in a tight loop.
	failureBackoff = 5 * time.Second
	listBatch      = 256
	listCeiling    = 16384
	// maxStepsPerClaim bounds the work one claim does before releasing, so a
	// large backlog does not monopolize a worker slot.
	maxStepsPerClaim = 64
	maxDrainPasses   = 1000
)

// deliveryState is the worker's process-local bookkeeping. Nothing in it is
// persisted; durable delivery state lives in the subscription rows.
type deliveryState struct {
	broadcaster Subscriber
	pollEvery   time.Duration
	renewEvery  time.Duration
	jitter      func() float64
	hook        func()
	sem         chan struct{}
	kick        chan struct{}
	tasks       sync.WaitGroup

	mu            sync.Mutex
	running       map[string]bool
	backoff       map[string]time.Time
	lastSweep     time.Time
	lastAuthority time.Time
}

func newDeliveryState(cfg ServiceConfig) *deliveryState {
	d := &deliveryState{
		broadcaster: cfg.Broadcaster, pollEvery: cfg.PollInterval, renewEvery: cfg.RenewInterval,
		jitter: cfg.Jitter, hook: cfg.DrainHook,
		kick:    make(chan struct{}, 1),
		running: map[string]bool{}, backoff: map[string]time.Time{},
	}
	if d.pollEvery <= 0 {
		d.pollEvery = defaultPollInterval
	}
	if d.renewEvery <= 0 {
		d.renewEvery = defaultRenewInterval
	}
	// A claimant must renew before its claim enters claimMargin, or it would
	// cancel its own delivery. Clamp a slower interval to the default.
	if d.renewEvery >= claimTTL-claimMargin {
		d.renewEvery = defaultRenewInterval
	}
	if d.jitter == nil {
		d.jitter = mathrand.Float64
	}
	workers := cfg.Concurrency
	if workers <= 0 {
		workers = defaultConcurrency
	}
	d.sem = make(chan struct{}, workers)
	return d
}

// Run delivers until ctx ends. It holds one broadcaster subscription,
// resubscribes and drains when that subscription closes, and also drains on a
// fallback ticker. It returns nil at once when Events is disabled, and joins
// every in-flight delivery before returning.
func (s *Service) Run(ctx context.Context) error {
	if !s.avail.Enabled {
		return nil
	}
	d := s.delivery
	defer d.tasks.Wait()
	var wake <-chan struct{}
	cancelWake := func() {}
	if d.broadcaster != nil {
		wake, cancelWake = d.broadcaster.Subscribe()
	}
	defer func() { cancelWake() }()
	ticker := time.NewTicker(d.pollEvery)
	defer ticker.Stop()

	s.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, open := <-wake:
			if !open {
				// Overflow: events may have been missed. Resubscribe first so
				// nothing is missed from here on, then drain from the database.
				cancelWake()
				wake, cancelWake = d.broadcaster.Subscribe()
			}
			coalesce(wake)
			s.cycle(ctx)
		case <-ticker.C:
			s.cycle(ctx)
		case <-d.kick:
			s.cycle(ctx)
		}
	}
}

// coalesce discards wake signals that queued while a cycle ran.
func coalesce(wake <-chan struct{}) {
	for {
		select {
		case _, open := <-wake:
			if !open {
				return
			}
		default:
			return
		}
	}
}

func (s *Service) cycle(ctx context.Context) {
	d := s.delivery
	d.mu.Lock()
	due := s.now().Sub(d.lastSweep) >= sweepInterval
	d.mu.Unlock()
	if due {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("MCP Events sweep failed", "error", err)
		}
	}
	if err := s.recheckAuthority(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("MCP Events authority check failed", "error", err)
	}
	if _, err := s.scan(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("MCP Events scan failed", "error", err)
	}
	if err := s.rematch(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("MCP Events rematch failed", "error", err)
	}
	if _, err := s.dispatch(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("MCP Events drain failed", "error", err)
	}
	if d.hook != nil {
		d.hook()
	}
}

// Drain scans the archive and delivers everything that is due now, waiting
// for those deliveries to finish. Run calls the same logic on every wake;
// Drain is for callers that want a synchronous pass. Drain must not run
// concurrently with Run: they share one WaitGroup, so Drain would also wait
// for Run's deliveries.
func (s *Service) Drain(ctx context.Context) error {
	if !s.avail.Enabled {
		return nil
	}
	if err := s.recheckAuthority(ctx); err != nil {
		return err
	}
	for range maxDrainPasses {
		moved, err := s.scan(ctx)
		if err != nil {
			return err
		}
		if err := s.rematch(ctx); err != nil {
			return err
		}
		started, err := s.dispatch(ctx)
		s.delivery.tasks.Wait()
		if err != nil {
			return err
		}
		if started == 0 && !moved {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// Sweep runs the once-a-minute maintenance: it ends expired active grants and
// deletes ended rows past retention or over the retained-row caps. Expiry does
// not wait for pending retries or delivery claims; per-attempt checks also
// enforce it.
func (s *Service) Sweep(ctx context.Context) error {
	if !s.avail.Enabled {
		return nil
	}
	now := s.now().UTC()
	d := s.delivery
	d.mu.Lock()
	d.lastSweep = now
	d.mu.Unlock()
	if _, err := s.store.ExpireMCPEventSubscriptions(ctx, now); err != nil {
		return err
	}
	_, err := s.store.PruneEndedMCPEventSubscriptions(ctx, now, db.MCPEventRetention{
		MaxAge: db.MCPEventEndedRetention, PerPrincipal: db.MCPEventEndedPrincipalMax, Daemon: db.MCPEventEndedDaemonMax,
	})
	return err
}

// Recheck makes the next drain revalidate every owner's authority and retry
// rows that are backing off, as after an authority change the worker cannot
// see.
func (s *Service) Recheck() {
	d := s.delivery
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastAuthority = time.Time{}
	clear(d.backoff)
}

// recheckAuthority ends the subscriptions whose owner lost authority or whose
// explicit selection has no visible project left. Delivery checks this before
// every ping, but a subscription with nothing to deliver is otherwise never
// looked at. It runs at most once per authorityRecheck and reacquires each
// owner once per stored actor.
func (s *Service) recheckAuthority(ctx context.Context) error {
	now := s.now().UTC()
	d := s.delivery
	d.mu.Lock()
	due := now.Sub(d.lastAuthority) >= authorityRecheck
	if due {
		d.lastAuthority = now
	}
	d.mu.Unlock()
	if !due {
		return nil
	}
	subs, _, err := s.activeSubscriptions(ctx)
	if err != nil {
		return err
	}
	byAuthority := map[string][]scanSubscription{}
	for _, sub := range subs {
		byAuthority[authorityKey(sub.row)] = append(byAuthority[authorityKey(sub.row)], sub)
	}
	for _, owned := range byAuthority {
		auth, err := s.authority.Reacquire(ctx, principalOf(owned[0].row))
		if errors.Is(err, ErrPrincipalRevoked) {
			for _, sub := range owned {
				s.endLive(ctx, sub.row.ID, sub.row.Actor, "principal_revoked")
			}
			continue
		}
		if err != nil {
			// No decision now; the next pass tries again.
			continue
		}
		for _, sub := range owned {
			if _, err := ResolveProjects(ctx, auth, sub.args); errors.Is(err, ErrScopeRemoved) {
				s.endLive(ctx, sub.row.ID, sub.row.Actor, "scope_removed")
			}
		}
	}
	return nil
}

// storeHead is the archive position including reserved purge-reset
// positions.
func (s *Service) storeHead(ctx context.Context) (int64, error) {
	head, err := s.store.MaxEventID(ctx)
	if err != nil {
		return 0, err
	}
	reset, err := s.store.PurgeResetCheck(ctx, 0, 0)
	if err != nil {
		return 0, err
	}
	return max(head, reset), nil
}

// dispatch lists due rows and starts a worker for each that is not already
// running or backing off, up to the concurrency limit. It reports how many it
// started.
func (s *Service) dispatch(ctx context.Context) (int, error) {
	now := s.now().UTC()
	limit := listBatch
	for {
		rows, err := s.store.ListDueMCPEventSubscriptions(ctx, now, limit)
		if err != nil {
			return 0, err
		}
		started := 0
		for _, row := range rows {
			if s.skipRow(row.ID, now) {
				continue
			}
			if !s.startWorker(ctx, row) {
				return started, nil
			}
			started++
		}
		// Skipped rows keep their place at the front of the list, so widen the
		// window rather than starve rows behind them.
		if started > 0 || len(rows) < limit || limit >= listCeiling {
			return started, nil
		}
		limit = min(limit*4, listCeiling)
	}
}

// skipRow reports a row this process is already delivering or backing off.
func (s *Service) skipRow(id string, now time.Time) bool {
	d := s.delivery
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[id] {
		return true
	}
	if until, ok := d.backoff[id]; ok {
		if now.Before(until) {
			return true
		}
		delete(d.backoff, id)
	}
	return false
}

// startWorker begins one claim-and-deliver task. It reports false when the
// process is already at its concurrency limit.
func (s *Service) startWorker(ctx context.Context, row db.MCPEventSubscription) bool {
	d := s.delivery
	select {
	case d.sem <- struct{}{}:
	default:
		return false
	}
	d.mu.Lock()
	d.running[row.ID] = true
	d.mu.Unlock()
	d.tasks.Add(1)
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.running, row.ID)
			d.mu.Unlock()
			<-d.sem
			d.tasks.Done()
			select {
			case d.kick <- struct{}{}:
			default:
			}
		}()
		s.finish(row, s.process(ctx, row))
	}()
	return true
}

// finish backs off a row whose step could not run.
func (s *Service) finish(row db.MCPEventSubscription, result stepResult) {
	if result != stepError {
		return
	}
	d := s.delivery
	d.mu.Lock()
	defer d.mu.Unlock()
	d.backoff[row.ID] = s.now().UTC().Add(failureBackoff)
}

// claim is a worker's hold on one subscription. The generation moves when
// the worker itself fences the row (a gap or an end), so every conditional
// write and every renewal goes through mu.
type claim struct {
	id          string
	incarnation string
	token       string
	cancel      context.CancelFunc

	mu       sync.Mutex
	gen      int64
	until    time.Time
	lost     atomic.Bool
	released bool
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// process claims a row and delivers until it has nothing left to do, a retry
// is scheduled, or the claim is lost.
func (s *Service) process(ctx context.Context, listed db.MCPEventSubscription) stepResult {
	token, err := randomHex(16)
	if err != nil {
		return stepError
	}
	now := s.now().UTC()
	won, err := s.store.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{
		ID: listed.ID, Incarnation: listed.Incarnation, Generation: listed.Generation, Token: token, Now: now, Until: now.Add(claimTTL),
	})
	if err != nil {
		if ctx.Err() != nil {
			return stepLost
		}
		return stepError
	}
	if !won {
		return stepLost
	}
	runCtx, cancel := context.WithCancel(ctx)
	c := &claim{id: listed.ID, incarnation: listed.Incarnation, token: token, gen: listed.Generation, until: now.Add(claimTTL), cancel: cancel}
	renewed := make(chan struct{})
	go s.renewLoop(runCtx, c, renewed)
	defer func() {
		cancel()
		<-renewed
		s.releaseClaim(ctx, c)
	}()

	for range maxStepsPerClaim {
		row, err := s.store.GetMCPEventSubscription(runCtx, c.id)
		if err != nil {
			if runCtx.Err() != nil {
				return stepLost
			}
			return stepError
		}
		c.mu.Lock()
		gen := c.gen
		c.mu.Unlock()
		if row.Incarnation != c.incarnation || row.Generation != gen || row.ClaimToken != c.token {
			return stepLost
		}
		if result := s.step(runCtx, c, row); result != stepAgain {
			return result
		}
	}
	return stepBusy
}

// claimMargin is how long before claim_until a claimant that has not renewed
// must consider the claim lost. It exceeds the 10 s webhook timeout, so a
// request started under a live claim cannot outlive it unnoticed.
const claimMargin = 15 * time.Second

// renewLoop extends the claim while the worker runs. A claimant that fails to
// renew cancels its context, which aborts the in-flight request, and stops. It
// does so as soon as the claim is within claimMargin of lapsing, not only on a
// renewal tick.
func (s *Service) renewLoop(ctx context.Context, c *claim, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.delivery.renewEvery)
	defer ticker.Stop()
	lapse := time.NewTimer(s.untilLapse(c))
	defer lapse.Stop()
	lose := func() {
		c.lost.Store(true)
		c.cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-lapse.C:
			lose()
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		now := s.now().UTC()
		ok, err := s.store.RenewMCPEventClaim(ctx, c.id, c.incarnation, c.gen, c.token, now.Add(claimTTL))
		switch {
		case ctx.Err() != nil:
			c.mu.Unlock()
			return
		case err == nil && ok:
			c.until = now.Add(claimTTL)
			c.mu.Unlock()
			lapse.Reset(s.untilLapse(c))
			continue
		case err == nil || !now.Before(c.until.Add(-claimMargin)):
			// The claim was taken over, or could not be renewed in time.
			c.mu.Unlock()
			lose()
			return
		}
		c.mu.Unlock()
	}
}

// untilLapse is how long until the claim must be treated as lost.
func (s *Service) untilLapse(c *claim) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(c.until.Add(-claimMargin).Sub(s.now()), 0)
}

// releaseClaim frees the claim so the next worker need not wait for it to
// expire. It is best effort and uses a context that survives shutdown.
func (s *Service) releaseClaim(ctx context.Context, c *claim) {
	c.mu.Lock()
	skip := c.released || c.lost.Load()
	c.mu.Unlock()
	if skip {
		return
	}
	rel, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = s.write(rel, c, db.MCPEventDeliveryUpdate{ReleaseClaim: true})
}

// write applies one claim-fenced delivery update. The predicate is the row id,
// incarnation, current generation and claim token; a write that bumps the
// generation moves the claim's generation with it.
func (s *Service) write(ctx context.Context, c *claim, in db.MCPEventDeliveryUpdate) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in.ID, in.Incarnation, in.Generation, in.ClaimToken = c.id, c.incarnation, c.gen, c.token
	if in.Now.IsZero() {
		in.Now = s.now().UTC()
	}
	ok, err := s.store.UpdateMCPEventDelivery(ctx, in)
	if ok && in.BumpGeneration {
		c.gen++
	}
	if ok && in.ReleaseClaim {
		c.released = true
	}
	return ok, err
}
