package daemon

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/kata/internal/tracker"
)

type trackerOwnerSession struct {
	ctx                    context.Context // daemon-owned provider session lifetime, canceled on close/expiry/shutdown
	cancel                 context.CancelFunc
	ticket                 tracker.WorkerTicket
	provider               trackerOwnerRun
	busy                   bool
	lastUsed, hardDeadline time.Time
	closing                bool
}
type trackerOwnerCall struct {
	flushing bool
	command  trackerWorkerCommand
	reply    *trackerWorkerReply
	chunks   []tracker.WorkerResultChunk
	complete bool
	sent     bool
	expires  time.Time
	held     int
}
type trackerOwnerWorker struct {
	ctx        context.Context // the explicit RunTrackerWorkers daemon lifetime
	mu         sync.Mutex
	sessions   map[string]*trackerOwnerSession
	calls      map[string]*trackerOwnerCall
	retained   int
	replyReady chan struct{}
}

func newTrackerOwnerWorker(ctx context.Context) *trackerOwnerWorker {
	return &trackerOwnerWorker{ctx: ctx, sessions: map[string]*trackerOwnerSession{}, calls: map[string]*trackerOwnerCall{}, replyReady: make(chan struct{}, 1)}
}
func (w *trackerOwnerWorker) pruneLocked(now time.Time) {
	for key, s := range w.sessions {
		if !now.Before(s.hardDeadline) {
			s.closing = true
			s.cancel()
		}
		if !s.busy && (s.closing || s.ctx.Err() != nil || !now.Before(s.lastUsed.Add(30*time.Second))) {
			s.cancel()
			delete(w.sessions, key)
		}
	}
	for id, call := range w.calls {
		if call.complete && !call.flushing && !now.Before(call.expires) {
			w.retained -= call.held
			delete(w.calls, id)
		}
	}
}
func (w *trackerOwnerWorker) prune(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now)
}
func (w *trackerOwnerWorker) acquire(t tracker.WorkerTicket, now time.Time) (*trackerOwnerSession, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now)
	if err := w.ctx.Err(); err != nil {
		return nil, nil, err
	}
	key := t.Selection.ConnectionUID
	session, exists := w.sessions[key]
	if exists {
		old := session.ticket
		if session.busy || session.closing || old.RootInstanceUID != t.RootInstanceUID || old.ProjectUID != t.ProjectUID || old.RunUID != t.RunUID || old.SessionUID != t.SessionUID || old.Selection != t.Selection || old.Principal != t.Principal || old.RootEpoch != t.RootEpoch || old.OwnerEpoch != t.OwnerEpoch {
			return nil, nil, tracker.ErrControlCapacity
		}
	} else {
		if len(w.sessions) >= 4 {
			return nil, nil, tracker.ErrControlCapacity
		}
		deadline := now.Add(25 * time.Minute)
		ctx, cancel := context.WithDeadline(w.ctx, deadline)
		session = &trackerOwnerSession{ctx: ctx, cancel: cancel, ticket: t, lastUsed: now, hardDeadline: deadline}
		w.sessions[key] = session
	}
	session.busy = true
	var once sync.Once
	release := func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			session.busy = false
			session.lastUsed = time.Now()
			if session.closing || session.ctx.Err() != nil {
				session.cancel()
				if w.sessions[key] == session {
					delete(w.sessions, key)
				}
			}
		})
	}
	return session, release, nil
}
func (w *trackerOwnerWorker) closeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, session := range w.sessions {
		session.closing = true
		session.cancel()
	}
	w.pruneLocked(time.Now())
}

func (w *trackerOwnerWorker) closeRun(t tracker.WorkerTicket) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	session, found := w.sessions[t.Selection.ConnectionUID]
	if !found {
		return nil
	}
	old := session.ticket
	if old.RootInstanceUID != t.RootInstanceUID || old.ProjectUID != t.ProjectUID || old.RunUID != t.RunUID || old.SessionUID != t.SessionUID || old.Selection != t.Selection || old.Principal != t.Principal || old.RootEpoch != t.RootEpoch || old.OwnerEpoch != t.OwnerEpoch {
		return tracker.ErrDenied
	}
	session.closing = true
	session.cancel()
	if !session.busy {
		delete(w.sessions, t.Selection.ConnectionUID)
	}
	return nil
}

func (w *trackerOwnerWorker) wakeReplies() {
	select {
	case w.replyReady <- struct{}{}:
	default:
	}
}
