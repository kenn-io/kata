package federation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/pkg/client/generated"
)

// federationWatchJoinDelay debounces joins from one hub's watchers so a pass
// or timeout that finishes across many projects settles into one watch instead
// of cancelling and reissuing partial watches. federationWatchJoinMaxWait caps
// how long steady joins can defer starting a watch.
const (
	federationWatchJoinDelay   = 100 * time.Millisecond
	federationWatchJoinMaxWait = 500 * time.Millisecond
)

// hubWatchSupport is what the spoke has learned about one hub's watches.
type hubWatchSupport struct {
	advertised hubPollAdvertisement
	// refusedUntil is set when the hub advertised watches but refused one,
	// as a proxy in front of it that admits only known routes does.
	refusedUntil time.Time
}

// heldResult is how one held poll or watch ended for one binding.
type heldResult struct {
	// changed reports that the hub has work for the binding after its cursor.
	changed bool
	// advertised is the longest wait the hub said it holds.
	advertised time.Duration
	// elapsed is how long the hub held a request it answered with nothing
	// new; the watcher reads it only then.
	elapsed time.Duration
	err     error
	// unwatched reports that the hub would not watch the binding with its
	// token; the binding holds its own poll from now on.
	unwatched bool
	// again asks the watcher to hold again at once: the hub answered for
	// other bindings, or the spoke stopped sending it watches.
	again bool
	// quiet reports a clean caught-up watch answer whose hold was refused or
	// skipped during hold-backoff; the watcher records pull success, skips
	// interval pulls for sleep, and rejoins the watch after sleep.
	quiet bool
	// sleep overrides the watcher's local delay so every member of one watch
	// wakes together on a shared jittered schedule.
	sleep time.Duration
}

// takesWatches reports whether a watch at the hub can carry several bindings.
func (s *hubWatchSupport) takesWatches(now time.Time) bool {
	return s != nil && s.advertised.wait > 0 && s.advertised.watchMax > 1 && !now.Before(s.refusedUntil)
}

// learnHub records what a hub advertised in a poll or watch response.
func (p *hubLongPolls) learnHub(baseURL string, advertised hubPollAdvertisement) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hubs == nil {
		p.hubs = map[string]*hubWatchSupport{}
	}
	support := p.hubs[baseURL]
	if support == nil {
		support = &hubWatchSupport{}
		p.hubs[baseURL] = support
	}
	support.advertised = advertised
}

// learnHubFromContext records a hub's advertisement seen by a runner pass.
func learnHubFromContext(ctx context.Context, baseURL string, advertised hubPollAdvertisement) {
	polls, _ := ctx.Value(hubLongPollsContextKey{}).(*hubLongPolls)
	polls.learnHub(baseURL, advertised)
}

// refuseWatches stops watches at a hub that refused one, until a reprobe.
func (p *hubLongPolls) refuseWatches(baseURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if support := p.hubs[baseURL]; support != nil {
		support.refusedUntil = p.now().Add(federationLongPollReprobe)
	}
}

// watchGroup returns the group that holds one watch for every binding sharing
// this hub address and transport, or nil when this binding holds its own poll.
// A watch carries other bindings' tokens in its body, and a signature proves
// only the route bearer's key, so signed credentials never join one. A hub
// whose watch names only its route project shares nothing, so its bindings
// hold their own polls.
func (p *hubLongPolls) watchGroup(client *Client, credential config.FederationCredential) *hubWatchGroup {
	if credential.Signing != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	support := p.hubs[client.baseURL]
	if !support.takesWatches(p.now()) {
		return nil
	}
	key := client.baseURL + " " + strconv.FormatBool(credential.AllowInsecure)
	group := p.groups[key]
	if group == nil {
		if p.ctx.Err() != nil {
			return nil
		}
		if p.groups == nil {
			p.groups = map[string]*hubWatchGroup{}
		}
		group = &hubWatchGroup{
			polls: p, baseURL: client.baseURL,
			joins: make(chan *watchMember), leaves: make(chan *watchMember), done: make(chan struct{}),
		}
		p.groups[key] = group
		p.watchers.Go(func() { group.run(p.ctx) })
	}
	return group
}

// watchMember is one binding's place in a watch, for one held answer.
type watchMember struct {
	projectID    int64
	hubProjectID int64
	cursor       int64
	token        string
	client       *Client
	wait         time.Duration
	// finishSync ends the binding's endpoint gate once a request carrying
	// its bearer has been sent.
	finishSync func()
	result     chan heldResult
	// left is closed once no request carrying the bearer is in flight.
	left chan struct{}
}

// hubWatchGroup holds one watch at a hub for its member bindings. Each member
// joins for one answer, so the group sends again whenever its members change.
type hubWatchGroup struct {
	polls   *hubLongPolls
	baseURL string
	joins   chan *watchMember
	leaves  chan *watchMember
	done    chan struct{}

	// Pacing state owned solely by run().
	holdBackoff time.Duration
	holdRetryAt time.Time
	nextPollAt  time.Time
	lastChecked map[int64]time.Time
	errBackoff  time.Duration
}

// hold waits for the hub's answer for member. When ctx ends, as an endpoint
// change does, it returns only after no request carrying the member's bearer
// is in flight.
func (g *hubWatchGroup) hold(ctx context.Context, member *watchMember) heldResult {
	member.result = make(chan heldResult, 1)
	member.left = make(chan struct{})
	select {
	case g.joins <- member:
	case <-ctx.Done():
		return heldResult{err: ctx.Err()}
	case <-g.done:
		return heldResult{err: context.Canceled}
	}
	select {
	case result := <-member.result:
		return result
	case <-ctx.Done():
	case <-g.done:
		return heldResult{err: context.Canceled}
	}
	select {
	case g.leaves <- member:
		select {
		case <-member.left:
		case <-g.done:
		}
	case <-g.done:
	}
	return heldResult{err: ctx.Err()}
}

// watchFlight is one watch request in flight.
type watchFlight struct {
	lead          *watchMember
	members       map[*watchMember]bool
	requestedWait time.Duration
	probe         hubProbe
	releaseProbe  func()
	cancel        context.CancelFunc
	started       time.Time
	done          chan watchAnswer
	reissued      bool
}

type watchAnswer struct {
	// flight is the watch that answered. It is never nil.
	flight     *watchFlight
	ready      map[int64]bool
	unwatched  map[int64]bool
	advertised hubPollAdvertisement
	err        error
}

func (g *hubWatchGroup) run(ctx context.Context) {
	defer close(g.done)
	members := map[*watchMember]bool{}
	var (
		flight       *watchFlight
		leaving      []*watchMember
		sendTimer    *time.Timer
		send         <-chan time.Time
		pendingSince time.Time
	)
	stopTimer := func() {
		if sendTimer != nil {
			sendTimer.Stop()
		}
		send = nil
	}
	defer stopTimer()
	arm := func(now time.Time) {
		if pendingSince.IsZero() {
			pendingSince = now
		}
		remaining := max(federationWatchJoinMaxWait-now.Sub(pendingSince), 0)
		delay := min(federationWatchJoinDelay, remaining)
		stopTimer()
		sendTimer = time.NewTimer(delay)
		send = sendTimer.C
	}
	memberPacedWait := func(member *watchMember, now time.Time) (time.Duration, bool) {
		if g.holdRetryAt.IsZero() || !now.Before(g.holdRetryAt) {
			return 0, false
		}
		until := g.lastChecked[member.projectID]
		if now.Before(until) {
			return until.Sub(now), true
		}
		return 0, false
	}
	drainPaced := func(now time.Time) bool {
		for member := range members {
			if wait, paced := memberPacedWait(member, now); paced {
				delete(members, member)
				member.result <- heldResult{again: true, sleep: wait}
			}
		}
		if len(members) == 0 {
			stopTimer()
			pendingSince = time.Time{}
			return true
		}
		return false
	}
	for {
		var answers <-chan watchAnswer
		if flight != nil {
			answers = flight.done
		}
		select {
		case <-ctx.Done():
			if flight != nil {
				flight.cancel()
				<-flight.done
				if flight.releaseProbe != nil {
					flight.releaseProbe()
				}
			}
			return
		case member := <-g.joins:
			now := g.polls.now()
			if wait, paced := memberPacedWait(member, now); paced {
				// Return pacing waits to an already-checked watcher so it
				// sleeps outside the project's endpoint gate and re-reads
				// bindings/credentials before rejoining. Unchecked members
				// stay eligible so stragglers are never starved.
				member.result <- heldResult{again: true, sleep: wait}
				continue
			}
			members[member] = true
			arm(now)
		case member := <-g.leaves:
			delete(members, member)
			delete(g.lastChecked, member.projectID)
			if flight != nil && flight.members[member] {
				flight.reissued = true
				flight.cancel()
				leaving = append(leaving, member)
				continue
			}
			close(member.left)
		case answer := <-answers:
			for _, member := range leaving {
				close(member.left)
			}
			leaving = nil
			reissued := answer.flight.reissued && errors.Is(answer.err, context.Canceled)
			g.deliver(answer, members)
			if answer.flight.releaseProbe != nil {
				answer.flight.releaseProbe()
			}
			flight = nil
			pendingSince = time.Time{}
			if len(members) > 0 {
				now := g.polls.now()
				if drainPaced(now) {
					continue
				}
				if reissued && send == nil {
					flight = g.start(ctx, members)
				} else if send == nil {
					arm(now)
				}
			}
		case <-send:
			send = nil
			pendingSince = time.Time{}
			if len(members) == 0 {
				continue
			}
			if flight != nil {
				if hasUnwatchedMember(members, flight.members) {
					flight.reissued = true
					flight.cancel()
				}
				continue
			}
			if drainPaced(g.polls.now()) {
				continue
			}
			flight = g.start(ctx, members)
		}
	}
}

func hasUnwatchedMember(members, inFlight map[*watchMember]bool) bool {
	for member := range members {
		if !inFlight[member] {
			return true
		}
	}
	return false
}

// start sends one watch for members. The lowest local project leads with its
// own bearer; the rest go in the body. Members the hub cannot take in this
// watch hold their own polls. When the hub no longer takes watches, it sends
// no watch and returns every member to its watcher, which then polls alone.
func (g *hubWatchGroup) start(ctx context.Context, members map[*watchMember]bool) *watchFlight {
	g.polls.mu.Lock()
	support := g.polls.hubs[g.baseURL]
	takes := support.takesWatches(g.polls.now())
	limit := 0
	if support != nil {
		limit = support.advertised.watchMax
	}
	g.polls.mu.Unlock()
	if !takes {
		for member := range members {
			delete(members, member)
			member.result <- heldResult{again: true}
		}
		return nil
	}
	probeTTL := max(2*g.polls.opts.Timeout, 30*time.Second)
	releaseProbe, allowed, _, probe := g.polls.retries.acquireHubProbe(g.baseURL, g.polls.now(), probeTTL)
	if !allowed {
		for member := range members {
			delete(members, member)
			member.result <- heldResult{again: true}
		}
		return nil
	}
	var lead *watchMember
	for member := range members {
		if lead == nil || member.projectID < lead.projectID {
			lead = member
		}
	}
	if lead == nil {
		releaseProbe()
		return nil
	}
	flight := &watchFlight{
		lead: lead, members: map[*watchMember]bool{lead: true},
		probe: probe, releaseProbe: releaseProbe, done: make(chan watchAnswer, 1),
	}
	hubProjects := map[int64]bool{lead.hubProjectID: true}
	var others []generated.FederationWatchProject
	wait := lead.wait
	for member := range members {
		if member == lead {
			continue
		}
		if len(others)+1 >= limit || hubProjects[member.hubProjectID] {
			delete(members, member)
			member.result <- heldResult{unwatched: true}
			continue
		}
		hubProjects[member.hubProjectID] = true
		flight.members[member] = true
		others = append(others, generated.FederationWatchProject{ProjectID: member.hubProjectID, AfterID: member.cursor, Token: member.token})
		wait = min(wait, member.wait)
	}
	effectiveWait := wait
	if probe.version != 0 || (!g.holdRetryAt.IsZero() && g.polls.now().Before(g.holdRetryAt)) {
		effectiveWait = 0
	}
	flight.requestedWait = effectiveWait
	requestCtx, cancel := context.WithCancel(ctx)
	flight.cancel = cancel
	requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				for member := range flight.members {
					member.finishSync()
				}
			}
		},
	})
	flight.started = time.Now()
	g.polls.watchers.Go(func() {
		defer cancel()
		body, advertised, err := lead.client.watchProjectEvents(requestCtx, lead.hubProjectID, lead.cursor, others, effectiveWait)
		answer := watchAnswer{flight: flight, advertised: advertised, err: err, ready: map[int64]bool{}, unwatched: map[int64]bool{}}
		for _, id := range body.Ready {
			answer.ready[id] = true
		}
		for _, id := range body.Unwatched {
			answer.unwatched[id] = true
		}
		flight.done <- answer
	})
	return flight
}

// deliver hands one answer to the members its watch carried.
func (g *hubWatchGroup) deliver(answer watchAnswer, members map[*watchMember]bool) {
	flight := answer.flight
	if flight.reissued && errors.Is(answer.err, context.Canceled) {
		// The group ended this watch because its members changed; the
		// remaining members are in the next one. Completed HTTP responses
		// (including 429/503) still flow through so cooldowns are preserved.
		return
	}
	elapsed := time.Since(flight.started)
	var quietSleep time.Duration
	var errorSleep time.Duration
	var recoveryProbeClean bool
	routeRefused, leadDenied := watchRefusal(answer.err)
	switch {
	case answer.err == nil:
		g.errBackoff = 0
		g.polls.retries.clearHubUnavailable(g.baseURL, flight.probe)
		g.polls.learnHub(g.baseURL, answer.advertised)
		if answer.advertised.wait > 0 && answer.advertised.watchMax > 1 {
			if flight.requestedWait > 0 {
				if len(answer.ready) == 0 && elapsed < min(flight.requestedWait, answer.advertised.wait)/2 {
					// The hub answered at once: its held-poll bound is full.
					// Keep checking all watched projects with one wait=0 watch
					// per interval, and probe wait>0 again with jittered backoff.
					if g.holdBackoff == 0 {
						g.holdBackoff = 2 * g.polls.interval
					} else {
						g.holdBackoff = min(g.holdBackoff*2, 16*g.polls.interval)
					}
					now := g.polls.now()
					g.holdRetryAt = now.Add(g.polls.jitteredDelay(g.holdBackoff))
					quietSleep = g.polls.jitteredDelay(g.polls.interval)
					g.nextPollAt = now.Add(quietSleep)
				} else if len(answer.ready) == 0 {
					g.holdBackoff = 0
					g.holdRetryAt = time.Time{}
					g.nextPollAt = time.Time{}
					clear(g.lastChecked)
				}
			} else if flight.probe.version != 0 && (g.holdRetryAt.IsZero() || !g.polls.now().Before(g.holdRetryAt)) {
				// A wait=0 recovery probe succeeded after a hub cooldown;
				// immediately resume holding watches.
				recoveryProbeClean = true
			} else {
				now := g.polls.now()
				quietSleep = g.polls.jitteredDelay(g.polls.interval)
				g.nextPollAt = now.Add(quietSleep)
			}
			if quietSleep > 0 {
				if g.lastChecked == nil {
					g.lastChecked = map[int64]time.Time{}
				}
				for member := range flight.members {
					if answer.ready[member.hubProjectID] || answer.unwatched[member.hubProjectID] {
						delete(g.lastChecked, member.projectID)
					} else {
						g.lastChecked[member.projectID] = g.nextPollAt
					}
				}
			}
		}
	case leadDenied:
		// The hub refused the lead's own credential or project. That is the
		// lead's failure alone; the others watch again without it.
	case routeRefused:
		g.polls.refuseWatches(g.baseURL)
		g.polls.report(fmt.Errorf("federation hub %s refused the watch request, so the spoke holds one poll per project for the next hour; check that a proxy in front of the hub allows POST /api/v1/projects/{project_id}/federation/events:watch: %w",
			g.baseURL, answer.err))
	default:
		if g.errBackoff == 0 {
			g.errBackoff = g.polls.interval
		} else {
			g.errBackoff = min(g.errBackoff*2, 16*g.polls.interval)
		}
		errorSleep = g.polls.jitteredDelay(g.errBackoff)
		if hubAskedToWait(answer.err) {
			now := g.polls.now()
			_, _, retryAfter := classifySyncRetry(answer.err)
			hubWait := g.polls.retries.recordHubUnavailable(g.baseURL, flight.probe.version, retryAfter, g.polls.interval, now)
			errorSleep = max(errorSleep, hubWait)
			if flight.requestedWait > 0 {
				// Back off requesting held waits after a 429/503 on a held watch
				// so a wait=0 recovery probe against a separate request pool
				// stays multiplexed on wait=0 instead of immediately tripping
				// the held-poll pool again.
				if g.holdBackoff == 0 {
					g.holdBackoff = 2 * g.polls.interval
				} else {
					g.holdBackoff = min(g.holdBackoff*2, 16*g.polls.interval)
				}
				g.holdRetryAt = now.Add(max(g.polls.jitteredDelay(g.holdBackoff), errorSleep))
			}
		}
	}
	result := func(member *watchMember) heldResult {
		switch {
		case leadDenied:
			if member == flight.lead {
				return heldResult{err: answer.err}
			}
			return heldResult{again: true}
		case routeRefused:
			return heldResult{again: true}
		case answer.err != nil:
			return heldResult{err: answer.err, sleep: errorSleep}
		case answer.ready[member.hubProjectID]:
			return heldResult{changed: true, advertised: answer.advertised.wait}
		case answer.unwatched[member.hubProjectID]:
			return heldResult{unwatched: true}
		case quietSleep > 0:
			return heldResult{advertised: answer.advertised.wait, elapsed: elapsed, sleep: quietSleep, quiet: true}
		case recoveryProbeClean:
			return heldResult{again: true}
		case len(answer.ready) > 0:
			return heldResult{again: true}
		default:
			return heldResult{advertised: answer.advertised.wait, elapsed: elapsed}
		}
	}
	for member := range flight.members {
		if !members[member] {
			continue
		}
		delete(members, member)
		member.result <- result(member)
	}
}

// watchRefusal reads a failed watch. routeRefused reports a refusal of the
// watch route itself, as from a proxy that admits only the routes it knows:
// any 405, or a 401, 403, or 404 without a Kata error code. leadDenied reports
// the hub's own refusal of the lead's credential or project, which carries an
// error code, as the lead's own poll would be refused.
func watchRefusal(err error) (routeRefused, leadDenied bool) {
	hubErr, ok := errors.AsType[*HubStatusError](err)
	if !ok {
		return false, false
	}
	switch hubErr.StatusCode {
	case http.StatusMethodNotAllowed:
		return true, false
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		coded := federationHubErrorCode(hubErr.Body) != ""
		return !coded, coded
	default:
		return false, false
	}
}

// heldPollResult holds one binding's own poll, as a spoke does at a hub that
// takes no watches.
func (p *hubLongPolls) heldPollResult(ctx context.Context, client *Client, hubProjectID, cursor int64, wait time.Duration) heldResult {
	started := time.Now()
	body, advertised, err := client.pollProjectEvents(ctx, hubProjectID, "", cursor, 1, wait)
	if err != nil {
		return heldResult{err: err}
	}
	p.learnHub(client.baseURL, advertised)
	if pollChanged(body, cursor) {
		return heldResult{changed: true, advertised: advertised.wait}
	}
	return heldResult{advertised: advertised.wait, elapsed: time.Since(started)}
}
