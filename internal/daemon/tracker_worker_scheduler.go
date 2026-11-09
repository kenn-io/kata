package daemon

import (
	"context"
	"sync"
	"time"
)

// runTrackerPollScheduler rotates one entry per existing enrolled project.
// Completion frees a slot immediately: a slow poll never creates a whole-pass
// barrier. The fixed network pool bounds I/O without inventing a project cap.
// Fairness does not guarantee a deadline under overload; tickets never renew.
func runTrackerPollScheduler(ctx context.Context, interval time.Duration, parallel int, projects func(context.Context) ([]int64, error), poll func(context.Context, int64)) {
	type state struct {
		enabled, inFlight bool
		due               time.Time
	}
	states := map[int64]*state{}
	queue := []int64{}
	jobs := make(chan int64, parallel)
	complete := make(chan int64, parallel)
	var workers sync.WaitGroup
	for i := 0; i < parallel; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				request, cancel := context.WithTimeout(ctx, 10*time.Second)
				if request.Err() == nil {
					poll(request, id)
				}
				cancel()
				select {
				case complete <- id:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	defer func() { close(jobs); workers.Wait() }()
	nextRefresh := time.Time{}
	active := 0
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(nextRefresh) {
			if ids, err := projects(ctx); err == nil {
				for _, s := range states {
					s.enabled = false
				}
				for _, id := range ids {
					s, exists := states[id]
					if !exists {
						s = &state{}
						states[id] = s
						queue = append(queue, id)
					}
					s.enabled = true
				}
				for id, s := range states {
					if !s.enabled && !s.inFlight {
						delete(states, id)
					}
				}
				kept := queue[:0]
				seen := map[int64]bool{}
				for _, id := range queue {
					if s, ok := states[id]; ok && s.enabled && !s.inFlight && !seen[id] {
						kept = append(kept, id)
						seen[id] = true
					}
				}
				queue = kept
			}
			nextRefresh = now.Add(interval)
		}
		turns := len(queue)
		for turns > 0 && active < parallel {
			turns--
			id := queue[0]
			queue = queue[1:]
			s, exists := states[id]
			if !exists || !s.enabled {
				continue
			}
			if s.inFlight {
				continue
			}
			if now.Before(s.due) {
				queue = append(queue, id)
				continue
			}
			s.inFlight = true
			active++
			jobs <- id
		}
		wake := nextRefresh
		if active < parallel {
			for _, id := range queue {
				if s, ok := states[id]; ok && s.enabled && s.due.Before(wake) {
					wake = s.due
				}
			}
		}
		timer := time.NewTimer(max(time.Until(wake), time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case id := <-complete:
			timer.Stop()
			active--
			if s, exists := states[id]; exists {
				s.inFlight = false
				if s.enabled {
					s.due = time.Now().Add(interval)
					queue = append(queue, id)
				} else {
					delete(states, id)
				}
			}
		case <-timer.C:
		}
	}
}
