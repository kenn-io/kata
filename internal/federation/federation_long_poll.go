package federation

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	held       int64
	heldTarget string
	cancelHeld context.CancelFunc
	probedAt   string
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
		changed: make(chan struct{}, 1), polls: map[int64]*hubLongPoll{}, passDone: make(chan struct{}),
	}
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
	if poll == nil || poll.held < 0 {
		return false
	}
	if poll.heldTarget != target {
		poll.cancelHeld()
		return false
	}
	return poll.held == cursor
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
	poll := &hubLongPoll{cancel: cancel, done: make(chan struct{}), held: -1}
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
	var (
		client    *Client
		clientFor config.FederationCredential
		supported time.Duration
		answered  *heldAnswer
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
			if !sleepContext(ctx, backoff) {
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
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		}
		identity, err := heldRetryIdentity(binding, credential)
		if err != nil || p.retries.blockedFor(ctx, projectID, binding, credential, p.now(), version) {
			finishSync()
			release()
			if !sleepContext(ctx, p.interval) {
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
				case <-time.After(backoff):
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
				if !sleepContext(ctx, backoff) {
					return
				}
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
		// the skip.
		requestCtx, cancelRequest := context.WithCancel(ctx)
		doneHeld := federationcoord.BeginHeld(federationcoord.Key(p.store.InstanceUID(), projectID), cancelRequest)
		p.setHeld(poll, cursor, target, cancelRequest)
		started := time.Now()
		requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					finishSync()
				}
			},
		})
		body, advertised, err := client.pollProjectEvents(requestCtx, credential.HubProjectID, "", cursor, 1, wait)
		finishSync()
		doneHeld()
		p.clearHeld(poll)
		superseded := errors.Is(requestCtx.Err(), context.Canceled)
		cancelRequest()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && superseded:
			// The runner ended a poll held for a target the binding no longer
			// uses. Poll the current target at once.
			continue
		case err != nil:
			if hubAskedToWait(err) {
				p.retries.recordSchedule(projectID, identity, identity, true, err, p.interval, p.now(), &version)
			}
			supported = 0
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		case advertised == 0:
			// The hub cannot hold polls, so it answered at once. Hand any page
			// it returned to the runner instead of waiting for the next tick.
			supported = 0
			if pollChanged(body, cursor) {
				p.signal()
			}
			if !sleepContext(ctx, federationLongPollReprobe) {
				return
			}
			continue
		}
		if pollChanged(body, cursor) {
			supported = advertised
			// Take the pass signal before waking the runner, so a pass that
			// ends before this goroutine looks again is not missed.
			next := p.nextPass()
			p.signal()
			select {
			case <-ctx.Done():
				return
			case <-next:
			case <-time.After(p.interval):
			}
			unread = cursor
			continue
		}
		if time.Since(started) < min(wait, advertised)/2 {
			// The hub answered at once: it is at its bound for held polls.
			// The interval pass pulls meanwhile, so retry less often each time.
			supported = 0
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		}
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

func (p *hubLongPolls) report(err error) {
	if p.notice != nil && p.ctx.Err() == nil {
		p.notice(err)
	}
}

// pollChanged reports whether a page has anything for the runner to apply.
func pollChanged(body api.PollEventsBody, cursor int64) bool {
	return len(body.Events) > 0 || body.ResetRequired || body.NextAfterID != cursor
}

func (p *hubLongPolls) signal() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
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
