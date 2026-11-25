package federation

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

const (
	// federationLongPollWait is the wait a spoke asks for; the hub caps it.
	federationLongPollWait = 45 * time.Second
	// federationLongPollMargin keeps the wait inside the client's request
	// timeout.
	federationLongPollMargin = 15 * time.Second
	// federationLongPollReprobe is how long a spoke waits before asking a hub
	// that did not advertise long polls again, in case it was upgraded.
	federationLongPollReprobe   = time.Hour
	federationPollWaitMaxHeader = "X-Kata-Federation-Poll-Wait-Max"
	federationWatchMaxHeader    = "X-Kata-Federation-Watch-Max"
)

// heldPollTarget names the hub address, bearer and hub project a held poll
// was sent with.
func (c *Client) heldPollTarget(hubProjectID int64) string {
	return c.baseURL + " " + hex.EncodeToString(c.tokenDigest[:]) + " " + strconv.FormatInt(hubProjectID, 10)
}

// hubLongPolls keeps one held poll open per direct spoke binding whose hub
// supports it. A held poll that answers with work wakes the runner; while a
// poll is held, the runner's interval pass skips that binding's pull, because
// the hub has already answered that nothing new is visible at the cursor.
type hubLongPolls struct {
	store       db.Storage
	credentials config.FederationCredentialStore
	opts        clientpkg.Opts
	interval    time.Duration
	retries     *SyncRetries
	now         func() time.Time
	// jitter returns an additive delay spread in [0, limit).
	jitter func(limit time.Duration) time.Duration
	// admission gates the store reads a watcher makes between held polls. A
	// held poll itself takes no lease, so it never keeps the daemon awake.
	admission activity.WaitableAdmission
	// notice reports held-poll problems to the runner's error log; nil drops
	// them.
	notice func(error)
	// ctx bounds the watchers; stopAll cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// changed carries a wakeup to the runner when a held poll saw work.
	changed chan struct{}

	mu    sync.Mutex
	polls map[int64]*hubLongPoll
	// hubs records what each hub address advertised about held polls and
	// watches; groups holds one watch per hub address and transport.
	hubs     map[string]*hubWatchSupport
	groups   map[string]*hubWatchGroup
	woken    map[int64]bool
	reported map[int64]bool
	// watchers joins every watcher goroutine, including those retired by stop
	// or reconcile, so shutdown returns only after each has exited.
	watchers sync.WaitGroup
	// passDone is closed after each runner pass and then replaced.
	passDone chan struct{}
}

type hubLongPoll struct {
	cancel context.CancelFunc
	done   chan struct{}
	// heldTarget identifies the hub and credential of the in-flight poll.
	held        int64
	heldTarget  string
	cancelHeld  context.CancelFunc
	probedAt    string
	quietCursor int64
	quietTarget string
	quietUntil  time.Time
}

func newHubLongPolls(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	opts clientpkg.Opts,
	interval time.Duration,
	admission activity.WaitableAdmission,
	notice func(error),
	retries *SyncRetries, now func() time.Time,
) *hubLongPolls {
	ctx, cancel := context.WithCancel(ctx)
	return &hubLongPolls{
		store: store, credentials: credentials, opts: opts, interval: interval, admission: admission,
		notice: notice, ctx: ctx, cancel: cancel, retries: retries, now: now,
		jitter: func(limit time.Duration) time.Duration {
			if limit <= 0 {
				return 0
			}
			return rand.N(limit) //nolint:gosec // G404: retry jitter spreads load; it is not a secret.
		},
		changed: make(chan struct{}, 1), polls: map[int64]*hubLongPoll{}, passDone: make(chan struct{}),
	}
}

func (p *hubLongPolls) jitteredDelay(delay time.Duration) time.Duration {
	if delay <= 0 || p == nil || p.jitter == nil {
		return delay
	}
	return delay + p.jitter(delay/4)
}

func (p *hubLongPolls) hubCooldownSleep(until, now time.Time) time.Duration {
	if !until.IsZero() {
		return min(max(until.Sub(now), time.Millisecond), p.interval)
	}
	return min(p.jitteredDelay(max(p.interval/4, 50*time.Millisecond)), p.interval)
}

type hubLongPollsContextKey struct{}

func withHubLongPolls(ctx context.Context, polls *hubLongPolls) context.Context {
	if polls == nil {
		return ctx
	}
	return context.WithValue(ctx, hubLongPollsContextKey{}, polls)
}

// hubHoldsPull reports whether the hub is holding this binding's poll at
// cursor for the same hub address, bearer and hub project, so an ordinary poll
// now would return an empty page. A poll held for another target, such as the
// address before an endpoint change, no longer stands for the binding; it is
// ended so the watcher polls the current target.
func hubHoldsPull(ctx context.Context, projectID, cursor int64, target string) bool {
	polls, _ := ctx.Value(hubLongPollsContextKey{}).(*hubLongPolls)
	if polls == nil {
		return false
	}
	polls.mu.Lock()
	defer polls.mu.Unlock()
	poll := polls.polls[projectID]
	if poll == nil {
		return false
	}
	if poll.held >= 0 {
		if poll.heldTarget != target {
			poll.cancelHeld()
			return false
		}
		return poll.held == cursor
	}
	if poll.quietCursor >= 0 && polls.now().Before(poll.quietUntil) {
		if poll.quietTarget != target {
			poll.quietCursor, poll.quietTarget, poll.quietUntil = -1, "", time.Time{}
			return false
		}
		return poll.quietCursor == cursor
	}
	return false
}

// passCompleted releases polls that are waiting for the runner to apply what
// they saw.
func (p *hubLongPolls) passCompleted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.passDone)
	p.passDone = make(chan struct{})
}

func (p *hubLongPolls) nextPass() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.passDone
}

func (p *hubLongPolls) setHeld(poll *hubLongPoll, cursor int64, target string, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	poll.held, poll.heldTarget, poll.cancelHeld = cursor, target, cancel
	if cursor >= 0 {
		poll.quietCursor, poll.quietTarget, poll.quietUntil = -1, "", time.Time{}
	}
}

func (p *hubLongPolls) setQuiet(poll *hubLongPoll, cursor int64, target string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	poll.quietCursor, poll.quietTarget, poll.quietUntil = cursor, target, until
}

// finishHeldQuiet atomically ends the active held marker and publishes the
// quiet window under one lock acquisition so no interval pass can issue a
// duplicate pull in between.
func (p *hubLongPolls) finishHeldQuiet(poll *hubLongPoll, cursor int64, target string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	poll.held, poll.heldTarget, poll.cancelHeld = -1, "", nil
	poll.quietCursor, poll.quietTarget, poll.quietUntil = cursor, target, until
}

func (p *hubLongPolls) clearQuiet(poll *hubLongPoll) {
	p.setQuiet(poll, -1, "", time.Time{})
}

func (p *hubLongPolls) setProbed(poll *hubLongPoll, hubURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	poll.probedAt = hubURL
}

func (p *hubLongPolls) clearHeld(poll *hubLongPoll) {
	p.setHeld(poll, -1, "", nil)
}

// admit takes the runner's drain admission for one short unit of store work.
// It returns a nil retry channel when the denial is terminal.
func (p *hubLongPolls) admit() (release func(), admitted bool, retry <-chan struct{}) {
	if p.admission == nil {
		return func() {}, true, nil
	}
	lease, admitted, retry := p.admission()
	if !admitted {
		return nil, false, retry
	}
	return lease.Release, true, nil
}

// reconcile stops the watchers whose binding is no longer among the direct
// spokes the runner's pass just scanned, or whose binding moved off the hub
// address the watcher last polled. The next clean pass starts a new watcher.
func (p *hubLongPolls) reconcile(spokes []activeSpokeBinding) {
	if p == nil {
		return
	}
	wanted := map[int64]string{}
	for _, spoke := range spokes {
		if spoke.binding.RelayConfig == nil {
			wanted[spoke.binding.ProjectID] = spoke.binding.HubURL
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for projectID, poll := range p.polls {
		hubURL, ok := wanted[projectID]
		if !ok || (poll.probedAt != "" && poll.probedAt != hubURL) {
			poll.cancel()
			delete(p.polls, projectID)
		}
	}
}

// stop ends the binding's watcher after a pass for it failed. A held poll
// answers at once while the binding has unapplied work, so a watcher kept
// through failing passes would start a new pass after every failure. The
// interval retries the binding, and its next clean pass starts a new watcher.
func (p *hubLongPolls) stop(projectID int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if poll := p.polls[projectID]; poll != nil {
		poll.cancel()
		delete(p.polls, projectID)
	}
}

// start opens a watcher for a direct spoke binding after the runner's pass
// for it succeeded. Relay spokes keep the interval.
func (p *hubLongPolls) start(spoke activeSpokeBinding) {
	if p == nil || spoke.binding.RelayConfig != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	projectID := spoke.binding.ProjectID
	if p.ctx.Err() != nil || p.polls[projectID] != nil {
		return
	}
	pollCtx, cancel := context.WithCancel(p.ctx)
	poll := &hubLongPoll{cancel: cancel, done: make(chan struct{}), held: -1, quietCursor: -1}
	p.polls[projectID] = poll
	p.watchers.Go(func() {
		defer close(poll.done)
		p.watch(pollCtx, projectID, poll)
	})
}

// stopAll cancels every watcher, retired ones included, and waits for them. It
// takes the lock after canceling so a start that passed its context check has
// already registered with the wait group.
func (p *hubLongPolls) stopAll() {
	p.cancel()
	p.mu.Lock()
	p.polls = map[int64]*hubLongPoll{}
	p.mu.Unlock()
	p.watchers.Wait()
}

// heldAnswer is a held poll the hub answered with nothing new: a successful
// pull at cursor that the watcher records once it can check the binding again.
type heldAnswer struct {
	cursor int64
	target string
	at     time.Time
}

// watch holds polls for one binding until ctx ends. Errors and refusals back
// off; the runner's interval pass keeps pulling meanwhile.
func (p *hubLongPolls) watch(ctx context.Context, projectID int64, poll *hubLongPoll) {
	defer p.clearQuiet(poll)
	var (
		client    *Client
		clientFor config.FederationCredential
		supported time.Duration
		answered  *heldAnswer
		// solo keeps the binding on its own held poll after the hub would
		// not watch it with its token.
		solo bool
	)
	// unread is the cursor of a page handed to the runner, until a pass
	// moves the binding past it.
	unread := int64(-1)
	backoff := p.interval
	for ctx.Err() == nil {
		release, admitted, retry := p.admit()
		if !admitted {
			if retry == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-retry:
			}
			continue
		}
		// An endpoint change drains the project's transport work under this
		// gate. Holding it from reading the target until the request is sent
		// keeps the bearer from reaching an address the change has retired.
		finishSync, err := federationcoord.BeginSync(ctx, federationcoord.Key(p.store.InstanceUID(), projectID), p.store, projectID)
		if err != nil {
			release()
			if !sleepContext(ctx, p.jitteredDelay(backoff)) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		}
		version := p.retries.version(projectID)
		binding, credential, ok := p.pollTarget(ctx, projectID)
		if !ok {
			finishSync()
			release()
			if !sleepContext(ctx, p.jitteredDelay(backoff)) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		}
		identity, err := heldRetryIdentity(binding, credential)
		now := p.now()
		if err != nil || p.retries.blockedFor(ctx, projectID, binding, credential, now, version) {
			finishSync()
			release()
			if !sleepContext(ctx, p.interval) {
				return
			}
			continue
		}
		if until, blocked := p.retries.hubBlockedUntil(binding.HubURL, now); blocked {
			finishSync()
			release()
			if !sleepContext(ctx, p.hubCooldownSleep(until, now)) {
				return
			}
			continue
		}
		credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
		cursor := binding.PullCursorEventID
		p.setProbed(poll, binding.HubURL)
		if unread >= 0 {
			if cursor == unread {
				// The pass that took the page left the binding where it was,
				// as one that fails before its pull does. Wait for another pass
				// instead of handing the same page over again at once.
				finishSync()
				release()
				select {
				case <-ctx.Done():
					return
				case <-p.nextPass():
				case <-time.After(p.jitteredDelay(backoff)):
				}
				backoff = min(backoff*2, 16*p.interval)
				continue
			}
			unread, backoff = -1, p.interval
		}
		opts := clientOptsForCredential(p.opts, credential)
		if client == nil || !sameLongPollCredential(clientFor, credential) {
			if client, err = NewClient(ctx, credential.HubURL, credential.Token, opts); err != nil {
				client = nil
				finishSync()
				release()
				if !sleepContext(ctx, p.jitteredDelay(backoff)) {
					return
				}
				backoff = min(backoff*2, 16*p.interval)
				continue
			}
			clientFor = credential
		}
		target := client.heldPollTarget(credential.HubProjectID)
		if answered != nil {
			if answered.cursor == cursor && answered.target == target {
				p.recordHeldAnswer(ctx, projectID, answered.at)
			}
			answered = nil
		}
		wait := federationLongPollWait
		if supported > 0 {
			wait = min(wait, supported)
		}
		wait = min(wait, opts.Timeout-federationLongPollMargin)
		if wait < time.Second {
			finishSync()
			release()
			p.report(fmt.Errorf("held federation polls off for project %d: request timeout %s is under the %s minimum; the spoke keeps its interval pull",
				projectID, opts.Timeout, federationLongPollMargin+time.Second))
			return
		}
		release()
		// Interval passes skip this binding's pull while the poll is out at
		// this target. A hub that cannot hold it answers at once, which ends
		// the skip unless a quiet watch answer covers the next interval.
		requestCtx, cancelRequest := context.WithCancel(ctx)
		doneHeld := federationcoord.BeginHeld(federationcoord.Key(p.store.InstanceUID(), projectID), cancelRequest)
		p.setHeld(poll, cursor, target, cancelRequest)
		requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					finishSync()
				}
			},
		})
		var held heldResult
		var group *hubWatchGroup
		if !solo {
			group = p.watchGroup(client, credential)
		}
		if group != nil {
			held = group.hold(requestCtx, &watchMember{
				projectID: projectID, hubProjectID: credential.HubProjectID, cursor: cursor,
				token: credential.Token, client: client, wait: wait, finishSync: finishSync,
			})
		} else {
			probeTTL := max(2*opts.Timeout, 30*time.Second)
			releaseProbe, allowed, retryAt, hubVersion := p.retries.acquireHubProbe(credential.HubURL, p.now(), probeTTL)
			if !allowed {
				finishSync()
				doneHeld()
				p.clearHeld(poll)
				cancelRequest()
				if !sleepContext(ctx, p.hubCooldownSleep(retryAt, p.now())) {
					return
				}
				continue
			}
			// Send a post-cooldown recovery probe with wait=0 so a healthy
			// idle hub clears the cooldown immediately instead of holding all
			// other callers back for a full wait interval.
			pollWait := wait
			if hubVersion != 0 {
				pollWait = 0
			}
			held = p.heldPollResult(requestCtx, client, credential.HubProjectID, cursor, pollWait)
			if held.err == nil {
				p.retries.clearHubUnavailable(credential.HubURL, hubVersion)
				if hubVersion != 0 && !held.changed && held.advertised > 0 {
					held.again = true
				}
			} else if hubAskedToWait(held.err) && !errors.Is(requestCtx.Err(), context.Canceled) {
				_, _, retryAfter := classifySyncRetry(held.err)
				hubWait := p.retries.recordHubUnavailable(credential.HubURL, hubVersion, retryAfter, p.interval, p.now())
				held.sleep = max(p.jitteredDelay(backoff), hubWait)
			}
			releaseProbe()
		}
		finishSync()
		doneHeld()
		superseded := errors.Is(requestCtx.Err(), context.Canceled)
		cancelRequest()
		err, advertised := held.err, held.advertised
		switch {
		case ctx.Err() != nil:
			p.clearHeld(poll)
			return
		case err != nil && superseded:
			p.clearHeld(poll)
			// The runner ended a poll held for a target the binding no longer
			// uses. Poll the current target at once.
			continue
		case err != nil:
			p.clearHeld(poll)
			var sleep time.Duration
			if hubAskedToWait(err) {
				p.retries.recordSchedule(projectID, identity, identity, true, err, p.interval, p.now(), &version, binding.UpdatedAt)
				if held.sleep > 0 {
					sleep = held.sleep
				} else {
					_, _, retryAfter := classifySyncRetry(err)
					hubWait := p.retries.recordHubUnavailable(credential.HubURL, 0, retryAfter, p.interval, p.now())
					sleep = max(p.jitteredDelay(backoff), hubWait)
				}
			} else {
				p.reportPollFailureOnce(projectID, err)
				if held.sleep > 0 {
					sleep = held.sleep
				} else {
					sleep = p.jitteredDelay(backoff)
				}
			}
			supported = 0
			if !sleepContext(ctx, sleep) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		case held.unwatched:
			p.clearHeld(poll)
			solo = true
			continue
		case held.again:
			if held.sleep > 0 {
				grace := min(max(p.interval/2, 500*time.Millisecond), 5*time.Second)
				p.finishHeldQuiet(poll, cursor, target, p.now().Add(held.sleep+grace))
				if !sleepContext(ctx, held.sleep) {
					return
				}
			} else {
				p.clearHeld(poll)
			}
			continue
		case advertised == 0:
			p.clearHeld(poll)
			// The hub cannot hold polls, so it answered at once. Hand any page
			// it returned to the runner instead of waiting for the next tick.
			supported = 0
			if held.changed {
				p.signal(projectID)
			}
			if !sleepContext(ctx, federationLongPollReprobe) {
				return
			}
			continue
		}
		if held.changed {
			p.clearHeld(poll)
			supported = advertised
			// Take the pass signal before waking the runner, so a pass that
			// ends before this goroutine looks again is not missed.
			next := p.nextPass()
			p.signal(projectID)
			select {
			case <-ctx.Done():
				return
			case <-next:
			case <-time.After(p.interval):
			}
			unread = cursor
			continue
		}
		if held.quiet {
			supported = advertised
			backoff = p.interval
			checkedAt := p.now()
			answered = &heldAnswer{cursor: cursor, target: target, at: checkedAt.UTC()}
			grace := min(max(p.interval/2, 500*time.Millisecond), 5*time.Second)
			p.finishHeldQuiet(poll, cursor, target, checkedAt.Add(held.sleep+grace))
			if !sleepContext(ctx, held.sleep) {
				return
			}
			continue
		}
		if held.elapsed < min(wait, advertised)/2 {
			// The hub answered at once: it is at its bound for held polls.
			// Record the quiet check so the runner does not immediately
			// double-poll the same cursor, and retry holding less often with jitter.
			supported = 0
			checkedAt := p.now()
			answered = &heldAnswer{cursor: cursor, target: target, at: checkedAt.UTC()}
			p.finishHeldQuiet(poll, cursor, target, checkedAt.Add(p.interval))
			if !sleepContext(ctx, p.jitteredDelay(backoff)) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		}
		p.clearHeld(poll)
		supported = advertised
		backoff = p.interval
		answered = &heldAnswer{cursor: cursor, target: target, at: time.Now().UTC()}
	}
}

// recordHeldAnswer records a held poll the hub answered with nothing new as a
// successful pull, unless a pass started or finished a pull since.
func (p *hubLongPolls) recordHeldAnswer(ctx context.Context, projectID int64, at time.Time) {
	if err := p.store.RecordFederationHeldPullSuccess(ctx, projectID, at.Truncate(time.Millisecond)); err != nil {
		p.report(fmt.Errorf("record held federation poll for project %d: %w", projectID, err))
	}
}

func (p *hubLongPolls) reportPollFailureOnce(projectID int64, err error) {
	p.mu.Lock()
	if p.reported == nil {
		p.reported = make(map[int64]bool)
	}
	alreadyReported := p.reported[projectID]
	p.reported[projectID] = true
	p.mu.Unlock()
	if !alreadyReported {
		p.report(fmt.Errorf("held federation poll for project %d: %w", projectID, err))
	}
}

func (p *hubLongPolls) report(err error) {
	if p.notice != nil && p.ctx.Err() == nil {
		p.notice(err)
	}
}

// pollChanged reports whether a page has anything for the runner to apply.
func pollChanged(body api.PollEventsBody, cursor int64) bool {
	return len(body.Events) > 0 || body.ResetRequired || body.NextAfterID != cursor
}

func (p *hubLongPolls) signal(projectID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.woken == nil {
		p.woken = make(map[int64]bool)
	}
	p.woken[projectID] = true
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *hubLongPolls) takeChanged(pending map[int64]bool) map[int64]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.changed:
	default:
	}
	if pending == nil {
		pending = make(map[int64]bool, len(p.woken))
	}
	for id := range p.woken {
		pending[id] = true
	}
	p.woken = nil
	return pending
}

// pollTarget resolves the binding and stored credential the runner's pass
// uses.
func (p *hubLongPolls) pollTarget(ctx context.Context, projectID int64) (db.FederationBinding, config.FederationCredential, bool) {
	binding, err := p.store.FederationBindingByProject(ctx, projectID)
	if err != nil || !binding.Enabled || binding.Role != db.FederationRoleSpoke || binding.RelayConfig != nil {
		return binding, config.FederationCredential{}, false
	}
	project, err := p.store.ProjectByID(ctx, projectID)
	if err != nil || project.DeletedAt != nil {
		return binding, config.FederationCredential{}, false
	}
	credential, _, err := p.credentials.FederationCredential(ctx, project.UID)
	if err != nil || credential.RelayEnrollmentPending || credential.LeavePending {
		return binding, config.FederationCredential{}, false
	}
	if binding.HubURL == "" || binding.HubProjectID == 0 {
		return binding, config.FederationCredential{}, false
	}
	return binding, credential, true
}

func sameLongPollCredential(a, b config.FederationCredential) bool {
	sameSigning := a.Signing == nil && b.Signing == nil ||
		a.Signing != nil && b.Signing != nil && *a.Signing == *b.Signing
	return sameSigning && a.HubURL == b.HubURL && a.Token == b.Token && a.AllowInsecure == b.AllowInsecure
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
