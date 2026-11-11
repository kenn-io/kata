package daemon

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type uiAttachmentRouteKey struct {
	url, projectUID, authorityUID string
	projectID                     int64
}

type uiAttachmentCapability struct {
	key         uiAttachmentRouteKey
	available   bool
	unavailable bool
	expires     time.Time
}

type uiAttachmentCapabilityCache struct {
	mu      sync.Mutex
	entries map[int64]uiAttachmentCapability
	now     func() time.Time
}

func newUIAttachmentCapabilityCache() *uiAttachmentCapabilityCache {
	return &uiAttachmentCapabilityCache{entries: make(map[int64]uiAttachmentCapability), now: time.Now}
}

func attachmentRouteKey(binding db.FederationBinding) uiAttachmentRouteKey {
	key := uiAttachmentRouteKey{url: binding.HubURL, projectID: binding.HubProjectID, projectUID: binding.HubProjectUID}
	if binding.RelayConfig != nil {
		key.authorityUID = binding.RelayConfig.AuthorityUID
	}
	return key
}

func (c *uiAttachmentCapabilityCache) available(ctx, parent context.Context, cfg ServerConfig, binding db.FederationBinding) (bool, bool) {
	key := attachmentRouteKey(binding)
	c.mu.Lock()
	for id, cached := range c.entries {
		if !c.now().Before(cached.expires) {
			delete(c.entries, id)
		}
	}
	entry, ok := c.entries[binding.ProjectID]
	if ok && entry.key == key && c.now().Before(entry.expires) {
		c.mu.Unlock()
		return entry.available, entry.unavailable
	}
	c.mu.Unlock()
	project, err := cfg.DB.ProjectByID(ctx, binding.ProjectID)
	if err == nil {
		_, err = docbankOriginCapabilities(ctx, cfg, project, &binding)
	}
	var apiErr *api.APIError
	unavailable := errors.As(err, &apiErr) && apiErr != nil && (apiErr.Code == "docbank_not_configured" || apiErr.Code == "docbank_forward_unsupported")
	if parent.Err() == nil && (!errors.As(err, &apiErr) || apiErr == nil || (apiErr.Status != http.StatusUnauthorized && apiErr.Status != http.StatusForbidden)) {
		c.mu.Lock()
		c.entries[binding.ProjectID] = uiAttachmentCapability{key: key, available: err == nil, unavailable: unavailable, expires: c.now().Add(5 * time.Second)}
		c.mu.Unlock()
	}
	return err == nil, unavailable
}
