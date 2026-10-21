package daemon

import (
	"context"
	"crypto/rand"
	"sync"

	"go.kenn.io/kata/internal/db"
)

// federationReplicaTransitionState is the leave lifecycle of one spoke
// mapping. It replaces a (leaveIntent, suppressed) boolean pair whose four
// combinations encoded only three legal states.
type federationReplicaTransitionState int

const (
	// federationReplicaIdle means no explicit leave is prepared or completed.
	federationReplicaIdle federationReplicaTransitionState = iota
	// federationReplicaLeavePending means PrepareFederationReplicaLeave has
	// durably marked the reservation and the leave has not completed yet.
	federationReplicaLeavePending
	// federationReplicaLeft means an explicit leave completed in this process.
	federationReplicaLeft
)

// federationReplicaTransition holds one mapping's leave state together with
// the drain accounting for hub operations already in flight for that mapping.
type federationReplicaTransition struct {
	state federationReplicaTransitionState
	// preparations counts, for each leave preparation that has neither
	// completed nor been released, the prepare calls that hold it. A client
	// retries prepare with the same preparation ID after losing a response,
	// so one preparation can be held more than once. The mapping stays
	// leave-pending while any remains, so releasing one of two concurrent
	// leaves keeps it blocked.
	preparations map[federationReplicaPreparation]int
	// releasedState is the state restored when the last preparation is
	// released: left when a leave was retried after it had completed, so the
	// mapping stays suppressed, and idle otherwise.
	releasedState federationReplicaTransitionState
	inFlight      int
	drained       chan struct{}
}

// federationReplicaRegistry owns every per-mapping transition record.
//
// Lock discipline: every mutating method must be called by code that already
// holds ensureFederationReplicaMu, so "check leave state, then register a hub
// operation" stays atomic against PrepareFederationReplicaLeave. The
// registry's own RWMutex exists solely so state can serve the config
// reconciler's lock-free FederationReplicaMappingSuppressed read without
// blocking on ensureFederationReplicaMu, which is held across database and
// credential-store I/O.
type federationReplicaRegistry struct {
	mu          sync.RWMutex
	transitions map[string]*federationReplicaTransition
}

var federationReplicaTransitions = &federationReplicaRegistry{
	transitions: make(map[string]*federationReplicaTransition),
}

// state reports the current leave state for key. It is the only method safe to
// call without holding ensureFederationReplicaMu.
func (r *federationReplicaRegistry) state(key string) federationReplicaTransitionState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	transition, ok := r.transitions[key]
	if !ok {
		return federationReplicaIdle
	}
	return transition.state
}

// leaveBlockedError returns the operator-facing error for a mapping whose
// leave state forbids new config-driven hub operations, or nil when idle.
func (r *federationReplicaRegistry) leaveBlockedError(key string) error {
	switch r.state(key) {
	case federationReplicaLeavePending:
		return federationReplicaError(
			ErrFederationReplicaLeavePending,
			"explicit federation leave is pending",
			"",
		)
	case federationReplicaLeft:
		return federationReplicaError(
			ErrFederationReplicaLeavePending,
			"federation mapping was explicitly left",
			"",
		)
	default:
		return nil
	}
}

// registerHubOperationLocked records one in-flight hub operation for key. The
// caller must hold ensureFederationReplicaMu and must have already rejected a
// blocking leave state with its own message. The returned finish function must
// also be called with that mutex held, and is safe to call more than once.
func (r *federationReplicaRegistry) registerHubOperationLocked(key string) func() {
	r.mu.Lock()
	transition, ok := r.transitions[key]
	if !ok {
		transition = &federationReplicaTransition{}
		r.transitions[key] = transition
	}
	transition.inFlight++
	if transition.drained == nil {
		transition.drained = make(chan struct{})
	}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { r.finishHubOperationLocked(key) })
	}
}

// registerHubOperation is registerHubOperationLocked for callers that release
// ensureFederationReplicaMu during hub I/O: the returned finish function takes
// the mutex itself.
func (r *federationReplicaRegistry) registerHubOperation(key string) func() {
	finish := r.registerHubOperationLocked(key)
	return func() {
		ensureFederationReplicaMu.Lock()
		defer ensureFederationReplicaMu.Unlock()
		finish()
	}
}

func (r *federationReplicaRegistry) finishHubOperationLocked(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	transition, ok := r.transitions[key]
	if !ok || transition.inFlight == 0 {
		return
	}
	transition.inFlight--
	if transition.inFlight > 0 {
		return
	}
	if transition.drained != nil {
		close(transition.drained)
		transition.drained = nil
	}
	r.pruneLocked(key, transition)
}

// drainSignal reports the channel closed when the last hub operation in flight
// for key finishes; ok is false when nothing is in flight. The caller must hold
// ensureFederationReplicaMu, and must re-check after each drain: a new
// operation can register between the drain and the re-check.
func (r *federationReplicaRegistry) drainSignal(key string) (<-chan struct{}, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	transition, ok := r.transitions[key]
	if !ok || transition.inFlight == 0 || transition.drained == nil {
		return nil, false
	}
	return transition.drained, true
}

// waitFederationReplicaOperations waits for hub requests already registered
// for key. Callers must first mark the mapping leave-pending so no new request
// can register while this loop drains the existing ones.
func waitFederationReplicaOperations(ctx context.Context, key string) error {
	for {
		ensureFederationReplicaMu.Lock()
		drained, waiting := federationReplicaTransitions.drainSignal(key)
		ensureFederationReplicaMu.Unlock()
		if !waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-drained:
		}
	}
}

// federationReplicaPreparation identifies one leave preparation. The client's
// preparation ID is scoped to the daemon store and the project that prepared
// it, because the registry is shared by every daemon service in the process
// and their database-local project IDs can match.
type federationReplicaPreparation struct {
	instanceUID string
	projectID   int64
	id          string
}

// newFederationReplicaPreparation scopes preparationID to projectID in store.
// An empty preparationID gets a fresh random ID.
func newFederationReplicaPreparation(
	store db.Storage, projectID int64, preparationID string,
) federationReplicaPreparation {
	if preparationID == "" {
		preparationID = rand.Text()
	}
	return federationReplicaPreparation{
		instanceUID: store.InstanceUID(), projectID: projectID, id: preparationID,
	}
}

// markLeavePending records that one prepare call for preparation durably
// prepared an explicit leave. A retry with the same preparation adds a hold to
// the same preparation instead of creating another one.
func (r *federationReplicaRegistry) markLeavePending(
	key string, preparation federationReplicaPreparation,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	transition, ok := r.transitions[key]
	if !ok {
		transition = &federationReplicaTransition{}
		r.transitions[key] = transition
	}
	if transition.state != federationReplicaLeavePending {
		transition.releasedState = transition.state
	}
	transition.state = federationReplicaLeavePending
	if transition.preparations == nil {
		transition.preparations = make(map[federationReplicaPreparation]int)
	}
	transition.preparations[preparation]++
}

// preparationKeys returns every key that preparation holds. It searches every
// key because a project can be renamed after it prepared a leave, and its key
// may embed the old name; a preparation retried after the rename holds both.
func (r *federationReplicaRegistry) preparationKeys(
	preparation federationReplicaPreparation,
) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var keys []string
	for key, transition := range r.transitions {
		if transition.preparations[preparation] > 0 {
			keys = append(keys, key)
		}
	}
	return keys
}

// releasePreparedLeave releases a preparation of a leave that stopped before
// local teardown, however many prepare calls hold it. An unknown, already
// released, or completed preparation is a no-op, so a retried release cannot
// consume another leave's preparation. The mapping leaves the pending state
// only when no other preparation remains, because another leave may still
// revoke and tear down.
func (r *federationReplicaRegistry) releasePreparedLeave(
	key string, preparation federationReplicaPreparation,
) {
	r.releasePreparation(key, preparation, true)
}

// releaseFailedPrepare drops the hold of one prepare call that failed before
// it answered its client. A retry of the same preparation that is still
// running keeps its own hold.
func (r *federationReplicaRegistry) releaseFailedPrepare(
	key string, preparation federationReplicaPreparation,
) {
	r.releasePreparation(key, preparation, false)
}

func (r *federationReplicaRegistry) releasePreparation(
	key string, preparation federationReplicaPreparation, everyHold bool,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	transition, ok := r.transitions[key]
	if !ok {
		return
	}
	holds := transition.preparations[preparation]
	switch {
	case holds == 0:
		return
	case holds > 1 && !everyHold:
		transition.preparations[preparation] = holds - 1
		return
	}
	delete(transition.preparations, preparation)
	if len(transition.preparations) > 0 {
		return
	}
	transition.state = transition.releasedState
	transition.releasedState = federationReplicaIdle
	transition.preparations = nil
	r.pruneLocked(key, transition)
}

// markLeft records that an explicit leave completed in this process.
func (r *federationReplicaRegistry) markLeft(key string) {
	r.setState(key, federationReplicaLeft)
}

// clearLeave returns key to idle after a successful ensure or an explicit
// connect, so it clears the left and leave-pending states alike. A leave that
// stopped before local teardown uses releasePreparedLeave instead.
func (r *federationReplicaRegistry) clearLeave(key string) {
	r.setState(key, federationReplicaIdle)
}

func (r *federationReplicaRegistry) setState(
	key string, state federationReplicaTransitionState,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	transition, ok := r.transitions[key]
	if !ok {
		if state == federationReplicaIdle {
			return
		}
		transition = &federationReplicaTransition{}
		r.transitions[key] = transition
	}
	transition.state = state
	transition.preparations = nil
	transition.releasedState = federationReplicaIdle
	r.pruneLocked(key, transition)
}

// pruneLocked drops records that carry no information: an idle mapping with
// nothing in flight is indistinguishable from one never seen.
func (r *federationReplicaRegistry) pruneLocked(
	key string, transition *federationReplicaTransition,
) {
	if transition.state == federationReplicaIdle && transition.inFlight == 0 {
		delete(r.transitions, key)
	}
}
