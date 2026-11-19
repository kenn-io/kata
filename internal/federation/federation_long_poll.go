package federation

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
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

// longPollProjectEvents asks the hub to hold a caught-up poll for up to wait.
// It reports the hub's advertised maximum wait, zero when the hub does not
// support held polls.
func (c *Client) longPollProjectEvents(ctx context.Context, hubProjectID, afterID int64, wait time.Duration) (api.PollEventsBody, time.Duration, error) {
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return api.PollEventsBody{}, 0, err
	}
	seconds := int64(wait / time.Second)
	limit := int64(1)
	response, callErr := apiClient.PollFederationProjectEventsWithResponse(ctx, &generated.PollFederationProjectEventsRequestOptions{
		PathParams: &generated.PollFederationProjectEventsPath{ProjectID: hubProjectID},
		Query:      &generated.PollFederationProjectEventsQuery{AfterID: &afterID, Limit: &limit, Wait: &seconds},
	})
	var body api.PollEventsBody
	if response == nil {
		return body, 0, callErr
	}
	if err := decodeReplicationResponse(response.HTTPResponse, response.Body, &body); err != nil {
		return body, 0, err
	}
	advertised, err := strconv.Atoi(response.HTTPResponse.Header.Get(federationPollWaitMaxHeader))
	if err != nil || advertised <= 0 {
		return body, 0, nil
	}
	return body, time.Duration(advertised) * time.Second, nil
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
	// admission gates the store reads a watcher makes between held polls. A
	// held poll itself takes no lease, so it never keeps the daemon awake.
	admission activity.WaitableAdmission
	// ctx bounds the watchers; stopAll cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// changed carries a wakeup to the runner when a held poll saw work.
	changed chan struct{}

	mu    sync.Mutex
	polls map[int64]*hubLongPoll
	// passDone is closed after each runner pass and then replaced.
	passDone chan struct{}
}

type hubLongPoll struct {
	cancel context.CancelFunc
	done   chan struct{}
	// held is the cursor of an in-flight poll the hub is known to hold, or -1.
	held int64
}

func newHubLongPolls(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	opts clientpkg.Opts,
	interval time.Duration,
	admission activity.WaitableAdmission,
) *hubLongPolls {
	ctx, cancel := context.WithCancel(ctx)
	return &hubLongPolls{
		store: store, credentials: credentials, opts: opts, interval: interval, admission: admission,
		ctx: ctx, cancel: cancel,
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
// cursor, so an ordinary poll now would return an empty page.
func hubHoldsPull(ctx context.Context, projectID, cursor int64) bool {
	polls, _ := ctx.Value(hubLongPollsContextKey{}).(*hubLongPolls)
	if polls == nil {
		return false
	}
	polls.mu.Lock()
	defer polls.mu.Unlock()
	poll := polls.polls[projectID]
	return poll != nil && poll.held >= 0 && poll.held == cursor
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

func (p *hubLongPolls) setHeld(projectID, cursor int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if poll := p.polls[projectID]; poll != nil {
		poll.held = cursor
	}
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
// spokes the runner's pass just scanned.
func (p *hubLongPolls) reconcile(spokes []activeSpokeBinding) {
	if p == nil {
		return
	}
	wanted := map[int64]bool{}
	for _, spoke := range spokes {
		if spoke.binding.RelayConfig == nil {
			wanted[spoke.binding.ProjectID] = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for projectID, poll := range p.polls {
		if !wanted[projectID] {
			poll.cancel()
			delete(p.polls, projectID)
		}
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
	go func() {
		defer close(poll.done)
		p.watch(pollCtx, projectID)
	}()
}

func (p *hubLongPolls) stopAll() {
	p.cancel()
	p.mu.Lock()
	polls := p.polls
	p.polls = map[int64]*hubLongPoll{}
	p.mu.Unlock()
	for _, poll := range polls {
		poll.cancel()
		<-poll.done
	}
}

// watch holds polls for one binding until ctx ends. Errors and refusals back
// off; the runner's interval pass keeps pulling meanwhile.
func (p *hubLongPolls) watch(ctx context.Context, projectID int64) {
	var (
		client    *Client
		clientFor config.FederationCredential
		supported time.Duration
	)
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
		binding, credential, ok := p.pollTarget(ctx, projectID)
		release()
		if !ok {
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 16*p.interval)
			continue
		}
		opts := clientOptsForCredential(p.opts, credential)
		if client == nil || !sameLongPollCredential(clientFor, credential) {
			var err error
			if client, err = NewClient(ctx, credential.HubURL, credential.Token, opts); err != nil {
				client = nil
				if !sleepContext(ctx, backoff) {
					return
				}
				continue
			}
			clientFor = credential
		}
		wait := federationLongPollWait
		if supported > 0 {
			wait = min(wait, supported)
		}
		wait = min(wait, opts.Timeout-federationLongPollMargin)
		if wait < time.Second {
			return
		}
		cursor := binding.PullCursorEventID
		// Interval passes skip this binding's pull while the poll is out. A
		// hub that cannot hold it answers at once, which ends the skip.
		p.setHeld(projectID, cursor)
		started := time.Now()
		body, advertised, err := client.longPollProjectEvents(ctx, credential.HubProjectID, cursor, wait)
		p.setHeld(projectID, -1)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
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
		backoff = p.interval
		if pollChanged(body, cursor) {
			supported = advertised
			p.signal()
			// Let the runner apply the page before polling from the new cursor.
			select {
			case <-ctx.Done():
				return
			case <-p.nextPass():
			case <-time.After(p.interval):
			}
			continue
		}
		if time.Since(started) < wait/2 {
			// The hub answered at once: it is at its bound for held polls.
			supported = 0
			if !sleepContext(ctx, p.interval) {
				return
			}
			continue
		}
		supported = advertised
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

// pollTarget resolves the binding and transport credential exactly as the
// runner's pass does.
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
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	if credential.HubURL == "" || credential.HubProjectID == 0 {
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

// pullCompletedAt keeps a pull's success time from preceding its start.
func pullCompletedAt(startedAt time.Time) time.Time {
	completedAt := time.Now().UTC()
	if completedAt.Before(startedAt) {
		return startedAt
	}
	return completedAt
}
