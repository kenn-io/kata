package daemon

import (
	"context"
	"sync"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

type claimClientEntry struct {
	client     *claimHubClient
	credential config.FederationCredential
	err        error
}

// A cache lives only for one batch request and pins each client to its binding.
type claimClientCache struct {
	mu      sync.Mutex
	entries map[int64]claimClientEntry
}

func (c *claimClientCache) get(ctx context.Context, cfg ServerConfig, binding db.FederationBinding) (*claimHubClient, config.FederationCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int64]claimClientEntry{}
	}
	entry, ok := c.entries[binding.ProjectID]
	if !ok {
		entry.client, entry.credential, entry.err = claimForwardClient(ctx, cfg, binding)
		c.entries[binding.ProjectID] = entry
	}
	return entry.client, entry.credential, entry.err
}
func requireFederatedIssueClaimsConcurrently(ctx context.Context, cfg ServerConfig, issues []db.Issue, actor string, cache *claimClientCache, limit int, refresh bool) map[string]error {
	failures := map[string]error{}
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, limit)
	for _, issue := range issues {
		if seen[issue.UID] {
			continue
		}
		seen[issue.UID] = true
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := requireFederatedIssueClaimWithRefresh(ctx, cfg, issue, actor, refresh, cache); err != nil {
				mu.Lock()
				failures[issue.UID] = err
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return failures
}
