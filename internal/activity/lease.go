// Package activity defines the small lease contract shared by daemon-owned
// background workers without coupling those packages to the idle controller.
package activity

import "sync"

// Admission admits one finite unit of background work.
type Admission func() (*Lease, bool)

// WaitableAdmission admits one finite unit of background work. A reversible
// denial returns a retry channel that closes when admission should be attempted
// again. A nil retry channel means the denial is terminal.
type WaitableAdmission func() (*Lease, bool, <-chan struct{})

// Lease protects one finite unit until Release. An optional fork source can
// transfer that protection to first-generation child work.
type Lease struct {
	mu       sync.Mutex
	release  func()
	fork     Admission
	released bool
}

// NewLease creates finite-work protection with optional first-generation
// child admission.
func NewLease(release func(), fork Admission) *Lease {
	return &Lease{release: release, fork: fork}
}

// Release completes the protected work. Repeated calls are harmless.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	release := l.release
	l.mu.Unlock()
	if release != nil {
		release()
	}
}

// Fork admits one child operation while the parent remains active.
func (l *Lease) Fork() (*Lease, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.fork == nil {
		return nil, false
	}
	return l.fork()
}
