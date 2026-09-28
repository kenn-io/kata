package embedding

import (
	"fmt"
	"time"

	"go.kenn.io/kata/internal/config"
)

// CredentialError is an operator-actionable embedding availability error.
// Provider response bodies are deliberately excluded from its diagnostics.
type CredentialError struct {
	Reason   string
	Provider *APIError
}

func (e *CredentialError) Error() string { return "semantic search unavailable: " + e.Reason }
func (e *CredentialError) Unwrap() error {
	if e.Provider == nil {
		return nil
	}
	return e.Provider
}

// CredentialHealth reports only source metadata, never a credential value.
type CredentialHealth struct {
	Credential       string
	CredentialSource string
	CredentialReason string
	LastError        string
	LastErrorAt      *time.Time
	LastErrorStatus  int
	LastSuccessAt    *time.Time
}

// SetCredential replaces the running credential. Unchanged reloads preserve
// rejection state. A revision fences responses issued with an older key.
func (c *Client) SetCredential(credential config.EmbeddingCredential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credential == credential {
		return
	}
	c.credential = credential
	c.credentialRevision++
	c.credentialHealth.CredentialSource = credential.Source
	c.credentialHealth.CredentialReason = credential.Reason
	c.credentialHealth.LastError = ""
	c.credentialHealth.LastErrorAt = nil
	c.credentialHealth.LastErrorStatus = 0
	if credential.Key == "" {
		c.credentialHealth.Credential = "missing"
		if credential.Reason == "" {
			c.credential.Reason = "no embedding API key"
			c.credentialHealth.CredentialReason = c.credential.Reason
		}
	} else {
		c.credentialHealth.Credential = "ok"
	}
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// CredentialHealth returns a detached snapshot shared by query and backfill.
func (c *Client) CredentialHealth() CredentialHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.credentialHealth
	h.LastErrorAt = cloneTime(h.LastErrorAt)
	h.LastSuccessAt = cloneTime(h.LastSuccessAt)
	return h
}

// MissingCredentialError blocks missing credentials only. Rejected credentials
// remain retryable: a successful call is the evidence needed to clear them.
func (c *Client) MissingCredentialError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credential.Key == "" {
		return &CredentialError{Reason: c.credential.Reason}
	}
	return nil
}

func (c *Client) requestCredential() (config.EmbeddingCredential, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credential.Key == "" {
		return c.credential, c.credentialRevision, &CredentialError{Reason: c.credential.Reason}
	}
	return c.credential, c.credentialRevision, nil
}

func (c *Client) rejectCredential(credential config.EmbeddingCredential, revision uint64, provider *APIError) error {
	// Do not retain echoed secrets in the wrapped error either.
	provider.Body = ""
	e := &CredentialError{Reason: fmt.Sprintf("embedding provider rejected the API key (%d) from %s", provider.StatusCode, credential.Source), Provider: provider}
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision == c.credentialRevision {
		now := time.Now().UTC()
		c.credentialHealth.Credential = "rejected"
		c.credentialHealth.CredentialReason = e.Reason
		c.credentialHealth.LastError = e.Error()
		c.credentialHealth.LastErrorAt = &now
		c.credentialHealth.LastErrorStatus = provider.StatusCode
	}
	return e
}

func (c *Client) credentialSucceeded(revision uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision != c.credentialRevision {
		return
	}
	now := time.Now().UTC()
	c.credentialHealth.Credential = "ok"
	c.credentialHealth.CredentialReason = ""
	c.credentialHealth.LastSuccessAt = &now
	c.credentialHealth.LastError = ""
	c.credentialHealth.LastErrorAt = nil
	c.credentialHealth.LastErrorStatus = 0
}
