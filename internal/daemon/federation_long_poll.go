package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

const (
	// federationPollWaitMax keeps a held poll under the 60 s idle timeout that
	// load balancers and reverse proxies commonly use, and under this daemon's
	// own 60 s federation request deadline.
	federationPollWaitMax = 45 * time.Second
	// federationPollWaitMargin leaves time to authorize and answer a held poll
	// before its request deadline.
	federationPollWaitMargin = 10 * time.Second
	// A held poll keeps a connection, a goroutine and a broadcaster
	// subscription. Polls beyond these bounds answer at once.
	federationPollMaxWaiters              = 256
	federationPollMaxWaitersPerCPU        = 32
	federationPollMaxWaitersPerCredential = 32
)

// federationPollWaits bounds the polls a hub holds and wakes them when their
// project commits an event in another process sharing the database.
type federationPollWaits struct {
	mu           sync.Mutex
	total        int
	maxTotal     int
	byCredential map[int64]int
	// signed counts held polls that also hold a federation control admission
	// slot. They may use at most half of those slots, so held polls never
	// starve metadata, lease and ordinary poll requests.
	signed    int
	maxSigned int
	remote    *federationPollWakeBus
	// instance tags this process's notifications so it ignores its own.
	instance string
}

func newFederationPollWaits(controlAdmissionSlots, cpus int) *federationPollWaits {
	return &federationPollWaits{
		byCredential: map[int64]int{},
		maxTotal:     min(federationPollMaxWaiters, federationPollMaxWaitersPerCPU*cpus),
		maxSigned:    controlAdmissionSlots / 2,
		remote:       newFederationPollWakeBus(),
		instance:     rand.Text(),
	}
}

type federationControlAdmissionContextKey struct{}

// withFederationControlAdmission marks a request that holds a federation
// control admission slot for its whole lifetime.
func withFederationControlAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, federationControlAdmissionContextKey{}, true)
}

func holdsFederationControlAdmission(ctx context.Context) bool {
	held, _ := ctx.Value(federationControlAdmissionContextKey{}).(bool)
	return held
}

// federationPollWait validates the requested wait and returns how long this
// request may be held. Zero means answer at once.
func federationPollWait(ctx context.Context, requested api.OptionalInt) (time.Duration, error) {
	if !requested.IsSet {
		return 0, nil
	}
	if requested.Value < 0 {
		return 0, api.NewError(http.StatusBadRequest, "validation", "wait must be a non-negative integer", "", nil)
	}
	// Clamp the seconds before converting, so a huge value cannot overflow.
	wait := time.Duration(min(requested.Value, int(federationPollWaitMax/time.Second))) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)-federationPollWaitMargin)
	}
	return max(wait, 0), nil
}

// federationPollWaiter is one admitted held poll.
type federationPollWaiter struct {
	waits        *federationPollWaits
	broadcaster  *EventBroadcaster
	filter       SubFilter
	local        Subscription
	remote       <-chan struct{}
	unsubRemote  func()
	enrollmentID int64
	signed       bool
}

// admit reserves a held poll for a project, or reports false when a bound is
// reached. The subscriptions start here, before the caller's first read, so
// an event that commits after that read still wakes the poll.
func (w *federationPollWaits) admit(ctx context.Context, broadcaster *EventBroadcaster, enrollmentID int64, project db.Project) (*federationPollWaiter, bool) {
	signed := holdsFederationControlAdmission(ctx)
	w.mu.Lock()
	if w.total >= w.maxTotal ||
		w.byCredential[enrollmentID] >= federationPollMaxWaitersPerCredential ||
		(signed && w.signed >= w.maxSigned) {
		w.mu.Unlock()
		return nil, false
	}
	w.total++
	w.byCredential[enrollmentID]++
	if signed {
		w.signed++
	}
	w.mu.Unlock()
	filter := SubFilter{ProjectID: project.ID, ProjectUID: project.UID}
	remote, unsubRemote := w.remote.subscribe(filter)
	return &federationPollWaiter{
		waits: w, broadcaster: broadcaster, filter: filter,
		local: broadcaster.Subscribe(filter), remote: remote, unsubRemote: unsubRemote,
		enrollmentID: enrollmentID, signed: signed,
	}, true
}

func (p *federationPollWaiter) release() {
	p.local.Unsub()
	p.unsubRemote()
	w := p.waits
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total--
	if w.byCredential[p.enrollmentID]--; w.byCredential[p.enrollmentID] <= 0 {
		delete(w.byCredential, p.enrollmentID)
	}
	if p.signed {
		w.signed--
	}
}

// wait blocks until the project may have a new event, the deadline passes, or
// the client goes away. It reports whether the poll was woken.
func (p *federationPollWaiter) wait(ctx context.Context, deadline time.Time) (bool, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	case _, open := <-p.local.Ch:
		if !open {
			// The broadcaster drops a subscriber whose buffer filled; the
			// overflow itself means events committed.
			p.local = p.broadcaster.Subscribe(p.filter)
		}
	case <-p.remote:
	}
	for {
		select {
		case _, open := <-p.local.Ch:
			if open {
				continue
			}
			p.local = p.broadcaster.Subscribe(p.filter)
		case <-p.remote:
			continue
		default:
		}
		return true, nil
	}
}

// federationPollCaughtUp reports a page that carries nothing for the spoke to
// apply: no events and no reset.
func federationPollCaughtUp(result db.FederationReadResult) bool {
	return len(result.Events) == 0 && result.ResetAfterID == 0
}

// federationPollWakeBus fans wakeups from other hub processes out to held polls.
// Each woken poll reads its own project again under its authorization.
type federationPollWakeBus struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]federationPollWakeSub
	secret []byte
}

type federationPollWakeSub struct {
	filter SubFilter
	ch     chan struct{}
}

func newFederationPollWakeBus() *federationPollWakeBus {
	return &federationPollWakeBus{subs: map[int]federationPollWakeSub{}}
}

// subscribe registers a held poll for remote wakes.
func (b *federationPollWakeBus) subscribe(filter SubFilter) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = federationPollWakeSub{filter: filter, ch: ch}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// wake signals every held poll.
func (b *federationPollWakeBus) wake() {
	b.wakeTags(nil)
}

func wakeTag(secret []byte, uid string) int {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("wake-v1"))
	_, _ = mac.Write([]byte(uid))
	return int(mac.Sum(nil)[0] & 63)
}

func (b *federationPollWakeBus) setSecret(secret []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.secret = secret
}

func (b *federationPollWakeBus) wakeTags(tags []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		if len(tags) > 0 && len(b.secret) > 0 && sub.filter.ProjectUID != "" {
			tag := wakeTag(b.secret, sub.filter.ProjectUID)
			if tag != tags[0] && (len(tags) < 2 || tag != tags[1]) {
				continue
			}
		}
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}

func (w *federationPollWaits) message() string {
	return w.instance
}

func (w *federationPollWaits) messageFor(msg StreamMsg) string {
	w.remote.mu.Lock()
	defer w.remote.mu.Unlock()
	if len(w.remote.secret) == 0 || msg.Event == nil || msg.Event.ProjectUID == "" {
		return w.message()
	}
	payload := w.instance + " " + strconv.Itoa(wakeTag(w.remote.secret, msg.Event.ProjectUID))
	if msg.movedFromProjectUID != "" {
		payload += " " + strconv.Itoa(wakeTag(w.remote.secret, msg.movedFromProjectUID))
	}
	return payload
}

func (w *federationPollWaits) receive(payload string) {
	fields := strings.Fields(payload)
	if len(fields) > 0 && fields[0] == w.instance {
		return
	}
	if len(fields) < 2 || len(fields) > 3 {
		w.remote.wake()
		return
	}
	tags := make([]int, 0, 2)
	for _, field := range fields[1:] {
		tag, err := strconv.Atoi(field)
		if err != nil || tag < 0 || tag >= 64 {
			w.remote.wake()
			return
		}
		tags = append(tags, tag)
	}
	w.remote.wakeTags(tags)
}

type federationPollWakeSecretStore interface {
	FederationPollWakeSecret(context.Context) ([]byte, error)
}

func loadFederationPollWakeSecret(ctx context.Context, bus db.FederationPollWakeBus, waits *federationPollWaits) {
	waits.remote.mu.Lock()
	hasSecret := len(waits.remote.secret) > 0
	waits.remote.mu.Unlock()
	if store, ok := bus.(federationPollWakeSecretStore); ok && !hasSecret {
		if secret, err := store.FederationPollWakeSecret(ctx); err == nil {
			waits.remote.setSecret(secret)
		}
	}
}

// runFederationPollWakeBridge publishes this process's committed events to the
// other hub processes on the database and wakes local held polls for theirs,
// until ctx ends.
func runFederationPollWakeBridge(ctx context.Context, bus db.FederationPollWakeBus, broadcaster *EventBroadcaster, waits *federationPollWaits, logger *slog.Logger) {
	// Both halves stop once the store closes; a server that is never closed
	// must not retry against it.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	loadFederationPollWakeSecret(ctx, bus, waits)
	var wg sync.WaitGroup
	wg.Go(func() { defer stop(); publishFederationPollWakes(ctx, bus, broadcaster, waits, logger) })
	wg.Go(func() { defer stop(); listenFederationPollWakes(ctx, bus, waits, logger) })
	wg.Wait()
}

// publishFederationPollWakes signals other processes after each committed event.
func publishFederationPollWakes(ctx context.Context, bus db.FederationPollWakeBus, broadcaster *EventBroadcaster, waits *federationPollWaits, logger *slog.Logger) {
	sub := broadcaster.Subscribe(SubFilter{})
	defer func() { sub.Unsub() }()
	for {
		var message string
		select {
		case <-ctx.Done():
			return
		case msg, open := <-sub.Ch:
			message = waits.messageFor(msg)
			if !open {
				sub = broadcaster.Subscribe(SubFilter{})
			}
		}
		err := bus.PublishFederationPollWake(ctx, message)
		if errors.Is(err, db.ErrFederationPollWakeClosed) {
			return
		}
		if err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "federation poll wake publish failed; remote held polls answer when their wait expires", "error", err)
		}
	}
}

func listenFederationPollWakes(ctx context.Context, bus db.FederationPollWakeBus, waits *federationPollWaits, logger *slog.Logger) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		connected := false
		err := bus.ListenFederationPollWake(ctx, func() {
			connected = true
			loadFederationPollWakeSecret(ctx, bus, waits)
			// Notifications sent while no listener was connected are lost.
			waits.remote.wake()
		}, waits.receive)
		if ctx.Err() != nil || errors.Is(err, db.ErrFederationPollWakeClosed) {
			return
		}
		if connected {
			backoff = time.Second
		}
		logger.WarnContext(ctx, "federation poll wake listener stopped; retrying", "error", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// startFederationPollBridge runs the cross-process wake bridge for a store
// shared by several hub processes, for the lifetime of the server. The
// returned function stops it and is safe to call more than once.
func startFederationPollBridge(cfg ServerConfig) func() {
	bus, ok := cfg.DB.(db.FederationPollWakeBus)
	if !ok {
		return func() {}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// The server lifetime owns this context; Close cancels it.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFederationPollWakeBridge(ctx, bus, cfg.Broadcaster, cfg.federationPollWaits, logger)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// holdFederationPoll waits on a caught-up page until the project may have new
// events or the wait ends. After a wake it authorizes the request again from
// scratch and reads again, so revocation during the wait still applies. When
// the wait ends it reports timedOut and returns the page already read; the
// spoke's next poll authorizes and reads from the same cursor. The hold ends at
// deadline, fixed before the poll's first read, so a slow first read shortens
// the hold.
func holdFederationPoll(
	ctx context.Context,
	waiter *federationPollWaiter,
	deadline time.Time,
	result db.FederationReadResult,
	reauthorize func() (context.Context, error),
	read func(context.Context) (db.FederationReadResult, error),
) (db.FederationReadResult, bool, bool, error) {
	waited := false
	for federationPollCaughtUp(result) && time.Now().Before(deadline) {
		waited = true
		woke, err := waiter.wait(ctx, deadline)
		if err != nil {
			return result, waited, false, fmt.Errorf("held federation poll ended: %w", err)
		}
		if !woke {
			return result, waited, true, nil
		}
		readCtx, err := reauthorize()
		if err != nil {
			return result, waited, false, err
		}
		if result, err = read(readCtx); err != nil {
			return result, waited, false, err
		}
	}
	return result, waited, false, nil
}

// heldFederationPollAllowed reports whether the hub can authorize a held poll
// again by itself. A request admitted by a host access decision made outside
// the federation credential check answers at once instead.
func heldFederationPollAllowed(ctx context.Context) bool {
	_, hostDecided := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState)
	return !hostDecided
}

// reauthorizeFederationPoll repeats the poll's federation authorization
// without the authorization cached for the request: the signing key,
// credential, project membership and the host federation decision are all
// checked again.
func reauthorizeFederationPoll(ctx context.Context, cfg ServerConfig, authHeader string, projectID int64, operation HostFederationOperation) (context.Context, error) {
	if err := recheckFederationSigningKey(ctx); err != nil {
		return ctx, err
	}
	authorization, err := evaluateFederationRequestForCapabilities(ctx, cfg, authHeader, projectID, []string{"pull"}, operation)
	if err != nil {
		return ctx, err
	}
	if err := rejectRelayCredentialOnLegacyEvents(authorization.principal); err != nil {
		return ctx, err
	}
	ctx = withFederationProjectAuthority(ctx, authorization)
	ctx = db.WithAdditionalTransactionFence(ctx, authorization.transactionFence)
	if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
		return ctx, err
	}
	return db.WithFederationEventStream(ctx), nil
}
