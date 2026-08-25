package daemon

import "sync"

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
	state    federationReplicaTransitionState
	inFlight int
	drained  chan struct{}
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

// markLeavePending records that an explicit leave was durably prepared.
func (r *federationReplicaRegistry) markLeavePending(key string) {
	r.setState(key, federationReplicaLeavePending)
}

// markLeft records that an explicit leave completed in this process.
func (r *federationReplicaRegistry) markLeft(key string) {
	r.setState(key, federationReplicaLeft)
}

// clearLeave returns key to idle. It serves both the successful-ensure clear
// and PrepareFederationReplicaLeave's rollback, so it clears the left and
// leave-pending states alike.
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
