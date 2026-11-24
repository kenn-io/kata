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

// federationWatchJoinDelay lets the watchers of one hub, which start together
// after a runner pass, join one watch instead of reissuing it once per join.
const federationWatchJoinDelay = 100 * time.Millisecond

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
	// elapsed is how long the hub held the request that answered.
	elapsed time.Duration
	err     error
	// unwatched reports that the hub would not watch the binding with its
	// token; the binding holds its own poll from now on.
	unwatched bool
	// again asks the watcher to hold again at once: the hub answered for
	// other bindings, or the spoke stopped sending it watches.
	again bool
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
	lead     *watchMember
	members  map[*watchMember]bool
	cancel   context.CancelFunc
	started  time.Time
	done     chan watchAnswer
	reissued bool
}

type watchAnswer struct {
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
		flight  *watchFlight
		leaving []*watchMember
		timer   *time.Timer
		send    <-chan time.Time
		answers <-chan watchAnswer
	)
	arm := func() {
		if send == nil {
			timer = time.NewTimer(federationWatchJoinDelay)
			send = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			if flight != nil {
				flight.cancel()
				<-flight.done
			}
			return
		case member := <-g.joins:
			members[member] = true
			if flight != nil {
				flight.reissued = true
				flight.cancel()
			}
			arm()
		case member := <-g.leaves:
			delete(members, member)
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
			g.deliver(answer, members)
			flight, answers = nil, nil
			if len(members) > 0 {
				arm()
			}
		case <-send:
			send, timer = nil, nil
			if flight != nil || len(members) == 0 {
				continue
			}
			if flight = g.start(ctx, members); flight != nil {
				answers = flight.done
			}
		}
	}
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
	var lead *watchMember
	for member := range members {
		if lead == nil || member.projectID < lead.projectID {
			lead = member
		}
	}
	if lead == nil {
		return nil
	}
	flight := &watchFlight{lead: lead, members: map[*watchMember]bool{lead: true}, done: make(chan watchAnswer, 1)}
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
		body, advertised, err := lead.client.watchProjectEvents(requestCtx, lead.hubProjectID, lead.cursor, others, wait)
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
	if answer.err != nil && flight.reissued {
		// The group ended this watch because its members changed; the
		// remaining members are in the next one.
		return
	}
	elapsed := time.Since(flight.started)
	result := func(member *watchMember) heldResult {
		switch {
		case answer.err != nil:
			return heldResult{err: answer.err, elapsed: elapsed}
		case answer.ready[member.hubProjectID]:
			return heldResult{changed: true, advertised: answer.advertised.wait, elapsed: elapsed}
		case answer.unwatched[member.hubProjectID]:
			return heldResult{unwatched: true}
		case len(answer.ready) > 0:
			return heldResult{again: true}
		default:
			return heldResult{advertised: answer.advertised.wait, elapsed: elapsed}
		}
	}
	routeRefused, leadDenied := watchRefusal(answer.err)
	switch {
	case answer.err == nil:
		g.polls.learnHub(g.baseURL, answer.advertised)
	case leadDenied:
		// The hub refused the lead's own credential or project. That is the
		// lead's failure alone; the others watch again without it.
		result = func(member *watchMember) heldResult {
			if member == flight.lead {
				return heldResult{err: answer.err, elapsed: elapsed}
			}
			return heldResult{again: true}
		}
	case routeRefused:
		g.polls.refuseWatches(g.baseURL)
		g.polls.report(fmt.Errorf("federation hub %s refused the watch request, so the spoke holds one poll per project for the next hour; check that a proxy in front of the hub allows POST /api/v1/projects/{project_id}/federation/events:watch: %w",
			g.baseURL, answer.err))
		result = func(*watchMember) heldResult { return heldResult{again: true} }
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
		return heldResult{err: err, elapsed: time.Since(started)}
	}
	p.learnHub(client.baseURL, advertised)
	return heldResult{changed: pollChanged(body, cursor), advertised: advertised.wait, elapsed: time.Since(started)}
}
