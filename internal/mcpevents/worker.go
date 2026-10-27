package mcpevents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"log/slog"
	mathrand "math/rand/v2"
	"strings"
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
	// idleRecheck bounds how long an idle subscription goes without a
	// revalidation of its principal, expiry and scope.
	idleRecheck = time.Minute
	// failureBackoff keeps a row that could not be processed (authority or
	// store outage) from being retried in a tight loop.
	failureBackoff = 5 * time.Second
	listBatch      = 256
	listCeiling    = 16384
	// quietWindow bounds the events a quiet check reads before it gives up
	// and lets the subscription take a full pass.
	quietWindow = 1000
	// maxStepsPerClaim bounds the work one claim does before releasing, so a
	// large backlog does not monopolize a worker slot.
	maxStepsPerClaim = 64
	maxDrainPasses   = 1000
)

// idleMark records that a subscription had nothing to do when the archive
// stood at head. It is process memory only: after a restart every row is
// checked once.
type idleMark struct {
	incarnation string
	generation  int64
	head        int64
	at          time.Time
	// projects are the visible selected projects of the last full pass.
	projects map[string]bool
}

// deliveryState is the worker's process-local bookkeeping. Nothing in it is
// persisted; durable delivery state lives in the subscription rows.
type deliveryState struct {
	broadcaster Subscriber
	pollEvery   time.Duration
	renewEvery  time.Duration
	jitter      func() float64
	hook        func()
	archiveHead func(context.Context) (int64, error)
	sem         chan struct{}
	kick        chan struct{}
	tasks       sync.WaitGroup

	mu        sync.Mutex
	running   map[string]bool
	backoff   map[string]time.Time
	marks     map[string]idleMark
	lastSweep time.Time
}

func newDeliveryState(cfg ServiceConfig) *deliveryState {
	d := &deliveryState{
		broadcaster: cfg.Broadcaster, pollEvery: cfg.PollInterval, renewEvery: cfg.RenewInterval,
		jitter: cfg.Jitter, hook: cfg.DrainHook, archiveHead: cfg.ArchiveHead,
		kick:    make(chan struct{}, 1),
		running: map[string]bool{}, backoff: map[string]time.Time{}, marks: map[string]idleMark{},
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
	if _, err := s.dispatch(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("MCP Events drain failed", "error", err)
	}
	if d.hook != nil {
		d.hook()
	}
}

// Drain delivers everything that is due now and waits for those deliveries to
// finish. Run calls the same logic on every wake; Drain is for callers that
// want a synchronous pass. Drain must not run concurrently with Run: they
// share one WaitGroup, so Drain would also wait for Run's deliveries.
func (s *Service) Drain(ctx context.Context) error {
	if !s.avail.Enabled {
		return nil
	}
	for range maxDrainPasses {
		started, err := s.dispatch(ctx)
		s.delivery.tasks.Wait()
		if err != nil {
			return err
		}
		if started == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// Sweep runs the once-a-minute maintenance: it ends expired active grants,
// deletes ended rows past retention or over the retained-row caps, and forgets
// idle marks so the next drain revalidates principal and scope. Expiry does not
// wait for pending retries or delivery claims; per-attempt checks also enforce it.
func (s *Service) Sweep(ctx context.Context) error {
	if !s.avail.Enabled {
		return nil
	}
	now := s.now().UTC()
	d := s.delivery
	d.mu.Lock()
	d.lastSweep = now
	clear(d.marks)
	d.mu.Unlock()
	if _, err := s.store.ExpireMCPEventSubscriptions(ctx, now); err != nil {
		return err
	}
	_, err := s.store.PruneEndedMCPEventSubscriptions(ctx, now, db.MCPEventRetention{
		MaxAge: db.MCPEventEndedRetention, PerPrincipal: db.MCPEventEndedPrincipalMax, Daemon: db.MCPEventEndedDaemonMax,
	})
	return err
}

// Recheck makes the next drain revalidate every subscription, as after an
// authority change the worker cannot see.
func (s *Service) Recheck() {
	d := s.delivery
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.marks)
	clear(d.backoff)
}

// storeHead is the archive position including reserved purge-reset
// positions. An idle subscription cannot have work while its mark is at it.
func (s *Service) storeHead(ctx context.Context) (int64, error) {
	if s.delivery.archiveHead != nil {
		return s.delivery.archiveHead(ctx)
	}
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
// running, idle, or backing off, up to the concurrency limit. It reports how
// many it started.
func (s *Service) dispatch(ctx context.Context) (int, error) {
	now := s.now().UTC()
	head, err := s.storeHead(ctx)
	if err != nil {
		return 0, err
	}
	limit := listBatch
	windows := map[int64][]db.Event{}
	for {
		rows, err := s.store.ListDueMCPEventSubscriptions(ctx, now, limit)
		if err != nil {
			return 0, err
		}
		started := 0
		for _, row := range rows {
			if s.skipRow(row, head, now) || s.quiet(ctx, row, head, now, windows) {
				continue
			}
			if !s.startWorker(ctx, row, head) {
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

func (s *Service) skipRow(row db.MCPEventSubscription, head int64, now time.Time) bool {
	d := s.delivery
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[row.ID] {
		return true
	}
	if until, ok := d.backoff[row.ID]; ok {
		if now.Before(until) {
			return true
		}
		delete(d.backoff, row.ID)
	}
	if row.State == db.MCPEventStateActive && row.PendingKind == db.MCPEventPendingNone {
		if m, ok := d.marks[row.ID]; ok && m.incarnation == row.Incarnation && m.generation == row.Generation && m.head >= head && now.Sub(m.at) < idleRecheck {
			return true
		}
	}
	return false
}

// quiet reports that every event since an idle subscription's last full pass
// is one that cannot change its inbox, and moves its mark to head. It reads
// the archive once per mark position per dispatch and writes nothing. Any
// doubt, including a read error or a window it cannot see to head, returns
// false so the subscription takes a full pass. The mark keeps its time, so
// the periodic recheck still revalidates authority and settles the cursor.
func (s *Service) quiet(ctx context.Context, row db.MCPEventSubscription, head int64, now time.Time, windows map[int64][]db.Event) bool {
	d := s.delivery
	d.mu.Lock()
	m, ok := d.marks[row.ID]
	d.mu.Unlock()
	if !ok || m.projects == nil || m.incarnation != row.Incarnation || m.generation != row.Generation ||
		row.State != db.MCPEventStateActive || row.PendingKind != db.MCPEventPendingNone || now.Sub(m.at) >= idleRecheck {
		return false
	}
	events, ok := windows[m.head]
	if !ok {
		events = s.quietWindow(ctx, m.head, head)
		windows[m.head] = events
	}
	if events == nil {
		return false
	}
	for _, event := range events {
		if eventMayConcern(event, m.projects) {
			return false
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if current, ok := d.marks[row.ID]; ok && current.head == m.head {
		current.head = head
		d.marks[row.ID] = current
	}
	return true
}

// quietWindow reads the events in (after, head], or returns nil when the read
// cannot prove the window: an error, a purge reset above after (purges reserve
// positions without rows), too many rows, or rows that stop short of head.
func (s *Service) quietWindow(ctx context.Context, after, head int64) []db.Event {
	reset, err := s.store.PurgeResetCheck(ctx, after, 0)
	if err != nil || reset != 0 {
		return nil
	}
	events, err := s.store.EventsAfter(ctx, db.EventsAfterParams{AfterID: after, ThroughID: head, Limit: quietWindow})
	if err != nil || len(events) == quietWindow || len(events) == 0 || events[len(events)-1].ID < head {
		return nil
	}
	return events
}

// eventMayConcern is the conservative pre-claim filter: catalog and reset
// events, and inbox-relevant types in or moving out of a selected project.
func eventMayConcern(event db.Event, projects map[string]bool) bool {
	switch {
	case event.Type == db.ProjectScopeResetEventType, strings.HasPrefix(event.Type, "project."):
		return true
	case !inboxVisibilityTypes[event.Type] && !inboxRequestTypes[event.Type]:
		return false
	case projects[event.ProjectUID]:
		return true
	}
	return event.Type == "issue.moved" && movedFrom(event.Payload, projects)
}

func movedFrom(payload string, projects map[string]bool) bool {
	var move struct {
		From string `json:"from_project_uid"`
	}
	return json.Unmarshal([]byte(payload), &move) == nil && projects[move.From]
}

// markIdle records a full pass that found nothing to do at head.
func (s *Service) markIdle(row db.MCPEventSubscription, head int64, projects []Project) {
	selected := make(map[string]bool, len(projects))
	for _, p := range projects {
		selected[p.UID] = true
	}
	d := s.delivery
	d.mu.Lock()
	defer d.mu.Unlock()
	d.marks[row.ID] = idleMark{incarnation: row.Incarnation, generation: row.Generation, head: head, at: s.now().UTC(), projects: selected}
}

// startWorker begins one claim-and-deliver task. It reports false when the
// process is already at its concurrency limit.
func (s *Service) startWorker(ctx context.Context, row db.MCPEventSubscription, head int64) bool {
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
		s.finish(row, s.process(ctx, row, head))
	}()
	return true
}

// finish backs off a row whose step could not run. An idle pass records its
// own mark, with the scope it read.
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
	// head is the archive position the dispatch saw; an idle pass marks it.
	head   int64
	cancel context.CancelFunc

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
func (s *Service) process(ctx context.Context, listed db.MCPEventSubscription, head int64) stepResult {
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
	c := &claim{id: listed.ID, incarnation: listed.Incarnation, token: token, head: head, gen: listed.Generation, until: now.Add(claimTTL), cancel: cancel}
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
