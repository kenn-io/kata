package agentsview

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/transcript"
	"golang.org/x/sync/singleflight"
)

var ErrTotalOverflow = errors.New("usage total cannot be represented")
var ErrDisabled = errors.New("usage source disabled")

type Total struct {
	State            string `json:"state"`
	CostMicrodollars *int64 `json:"cost_microdollars,omitempty"`
	Estimated        bool   `json:"estimated"`
}

type RunSession struct {
	Agent     string  `json:"agent"`
	SessionID string  `json:"session_id"`
	Step      *string `json:"step,omitempty"`
	URL       string  `json:"url,omitempty"`
	SessionUsage
}

type RunUsage struct {
	RunUID string `json:"run_uid"`
	Total
	SessionsIncomplete bool         `json:"sessions_incomplete"`
	FetchedAt          time.Time    `json:"fetched_at"`
	Sessions           []RunSession `json:"sessions"`
}

func aggregate(sessions []SessionUsage, incomplete bool) (Total, error) {
	result := Total{State: "no_sessions"}
	if len(sessions) == 0 {
		return result, nil
	}
	result.State = "unknown"
	var sum int64
	seen := make(map[string]bool)
	priced, partial := false, incomplete
	for _, item := range sessions {
		if item.State != "priced" {
			partial = true
			continue
		}
		priced = true
		if seen[item.CanonicalSessionID] {
			continue
		}
		seen[item.CanonicalSessionID] = true
		if item.CostMicrodollars == nil || *item.CostMicrodollars > math.MaxInt64-sum {
			return Total{}, ErrTotalOverflow
		}
		sum += *item.CostMicrodollars
		if item.CostSource == "computed" || item.CostSource == "mixed" {
			result.Estimated = true
		}
	}
	if priced {
		result.State = "complete"
		if partial {
			result.State = "partial"
		}
		result.CostMicrodollars = &sum
	}
	return result, nil
}

type cacheEntry struct {
	fact   SessionUsage
	at     time.Time
	ttl    time.Duration
	misses int
}
type generation struct {
	client  *Client
	cache   *lru.Cache[string, cacheEntry]
	flights singleflight.Group
	now     func() time.Time
}

func newGeneration(client *Client) *generation {
	cache, _ := lru.New[string, cacheEntry](2048)
	return &generation{client: client, cache: cache, now: time.Now}
}

// Service owns a single global four-slot limit even across credential reloads.
type Service struct {
	mu      sync.RWMutex
	cfg     config.ResolvedAgentsView
	current *generation
	slots   chan struct{}
}

func NewService(cfg config.ResolvedAgentsView, trustPrivate bool) (*Service, error) {
	s := &Service{cfg: cfg, slots: make(chan struct{}, 4)}
	if cfg.Enabled() {
		client, err := NewClient(cfg, trustPrivate)
		if err != nil {
			return nil, err
		}
		s.current = newGeneration(client)
	}
	return s, nil
}

func (s *Service) Enabled() bool { s.mu.RLock(); defer s.mu.RUnlock(); return s.current != nil }

// Reload swaps credentials and clears cached facts only for the same targets.
// It returns false when a restart is required to change targets or enablement.
func (s *Service) Reload(cfg config.ResolvedAgentsView, trustPrivate bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.URL != cfg.URL || s.cfg.BrowserURL != cfg.BrowserURL || s.cfg.Enabled() != cfg.Enabled() {
		return false, nil
	}
	if !cfg.Enabled() {
		return true, nil
	}
	client, err := NewClient(cfg, trustPrivate)
	if err != nil {
		return false, err
	}
	s.cfg = cfg
	s.current = newGeneration(client)
	return true, nil
}

func (s *Service) Usage(ctx context.Context, uid string, summary cron.Summary, refresh bool) (RunUsage, error) {
	s.mu.RLock()
	gen, cfg := s.current, s.cfg
	s.mu.RUnlock()
	if gen == nil {
		return RunUsage{}, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result := RunUsage{RunUID: uid, FetchedAt: time.Now().UTC(), Sessions: []RunSession{}}
	result.SessionsIncomplete = summary.SessionsIncomplete != nil && *summary.SessionsIncomplete
	if summary.Sessions == nil {
		result.Total.State = "no_sessions"
		return result, nil
	}
	entries := *summary.Sessions
	result.Sessions = make([]RunSession, len(entries))
	facts := make([]SessionUsage, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(i int, entry cron.Session) {
			defer wg.Done()
			link, _ := (transcript.Transcript{Agent: entry.Agent, SessionID: entry.SessionID}).Link(cfg.BrowserURL)
			fact := gen.lookup(ctx, s.slots, entry.Agent, entry.SessionID, refresh)
			facts[i] = fact
			result.Sessions[i] = RunSession{Agent: entry.Agent, SessionID: entry.SessionID, Step: entry.Step, URL: link, SessionUsage: fact}
		}(i, entry)
	}
	wg.Wait()
	total, err := aggregate(facts, result.SessionsIncomplete)
	if err != nil {
		return RunUsage{}, err
	}
	result.Total = total
	result.FetchedAt = time.Now().UTC()
	return result, nil
}

func (g *generation) lookup(ctx context.Context, slots chan struct{}, agent, id string, refresh bool) SessionUsage {
	unavailable := SessionUsage{State: "unavailable"}
	key := agent + "\x00" + id
	if entry, ok := g.cache.Get(key); ok && g.now().Sub(entry.at) < entry.ttl && (!refresh || g.now().Sub(entry.at) <= 10*time.Second) {
		return entry.fact
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	ch := g.flights.DoChan(key, func() (any, error) {
		// Shared work can outlive one canceled caller for other waiters, but the
		// originating request deadline bounds queueing and the lookup together.
		shared, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer cancel()
		select {
		case slots <- struct{}{}:
		case <-shared.Done():
			return unavailable, nil
		}
		defer func() { <-slots }()
		lookup, stop := context.WithTimeout(shared, 2*time.Second)
		defer stop()
		fact := g.client.Lookup(lookup, id)
		if shared.Err() != nil {
			return unavailable, nil
		}
		ttl, misses := 5*time.Minute, 0
		switch fact.State {
		case "not_found":
			if old, found := g.cache.Peek(key); found && old.fact.State == "not_found" {
				misses = old.misses + 1
			}
			ttl = time.Minute << min(misses, 4)
			if ttl > 15*time.Minute {
				ttl = 15 * time.Minute
			}
		case "unauthorized", "unavailable":
			ttl = 15 * time.Second
		}
		g.cache.Add(key, cacheEntry{fact: fact, at: g.now(), ttl: ttl, misses: misses})
		return fact, nil
	})
	select {
	case <-ctx.Done():
		return unavailable
	case value := <-ch:
		return value.Val.(SessionUsage)
	}
}

// Probe performs the operator's explicit on-demand diagnostic, not a startup probe.
func (s *Service) Probe(ctx context.Context) string {
	s.mu.RLock()
	gen := s.current
	s.mu.RUnlock()
	if gen == nil {
		return "off"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return gen.client.Probe(ctx)
}
