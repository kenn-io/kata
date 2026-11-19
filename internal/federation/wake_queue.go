package federation

import "sync"

// WakeQueue collects the spokes that wakes ask the runner to sync before its
// next interval. Wakes for different projects accumulate until the runner
// takes them, so a wake for one project never replaces a pending wake for
// another. A wake for every spoke absorbs project wakes until it is taken.
// The zero value is not usable; call NewWakeQueue.
type WakeQueue struct {
	mu       sync.Mutex
	all      bool
	projects map[int64]bool
	ready    chan struct{}
}

// NewWakeQueue returns an empty queue.
func NewWakeQueue() *WakeQueue {
	return &WakeQueue{ready: make(chan struct{}, 1)}
}

// WakeAll asks for a pass over every spoke binding.
func (q *WakeQueue) WakeAll() {
	q.mu.Lock()
	q.all = true
	q.projects = nil
	q.mu.Unlock()
	q.signal()
}

// WakeProject asks for a pass over the spoke binding of one local project.
func (q *WakeQueue) WakeProject(projectID int64) {
	q.mu.Lock()
	if !q.all {
		if q.projects == nil {
			q.projects = map[int64]bool{}
		}
		q.projects[projectID] = true
	}
	q.mu.Unlock()
	q.signal()
}

func (q *WakeQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// readyC returns the channel that fires after a wake. A nil queue never fires.
func (q *WakeQueue) readyC() <-chan struct{} {
	if q == nil {
		return nil
	}
	return q.ready
}

// take removes the pending wakes. It reports whether any were pending and,
// for project wakes, which projects to sync; a nil set means every spoke.
func (q *WakeQueue) take() (woken bool, only map[int64]bool) {
	if q == nil {
		return false, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// A signal sent before this take carries nothing more; drop it so the
	// runner does not debounce an empty wake.
	select {
	case <-q.ready:
	default:
	}
	woken, only = q.all || len(q.projects) > 0, q.projects
	q.all, q.projects = false, nil
	return woken, only
}
