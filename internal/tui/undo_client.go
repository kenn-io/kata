package tui

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// undoAttempt travels on a mutation response, even when the write failed.
// The top-level model consumes it before view generation guards apply.
type undoAttempt struct {
	client   *undoClient
	ready    chan struct{}
	entry    *undoEntry
	boundary string
	unknown  bool
}

func (a *undoAttempt) complete() {
	if a == nil || a.client == nil {
		return
	}
	a.client.mu.Lock()
	defer a.client.mu.Unlock()
	i := slices.Index(a.client.pending, a)
	if i < 0 {
		return
	}
	a.client.pending = slices.Delete(a.client.pending, i, i+1)
	if i == 0 && len(a.client.pending) > 0 {
		close(a.client.pending[0].ready)
	}
}

// undoClient wraps only the TUI's write methods. Embedding KataAPI forwards
// all reads and unrelated operations to the connected daemon client.
type undoClient struct {
	KataAPI
	mu       sync.Mutex
	pending  []*undoAttempt
	instance InstanceInfo
	epoch    uint64
}

func (c *undoClient) snapshotEpoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

func (m *Model) advanceMutationEpoch() {
	m.mutationEpoch++
	if c, ok := m.api.(*undoClient); ok {
		c.mu.Lock()
		c.epoch = m.mutationEpoch
		c.mu.Unlock()
	}
}

func newUndoClient(base KataAPI) *undoClient { return &undoClient{KataAPI: base} }

func (c *undoClient) ListTokens(ctx context.Context) ([]TokenInfo, time.Time, error) {
	audit, ok := c.KataAPI.(credentialAuditAPI)
	if !ok {
		return nil, time.Time{}, errors.New("credential audit client unavailable")
	}
	return audit.ListTokens(ctx)
}

// Writes wait until the model has recorded the preceding response. Undo must
// refuse a busy client because its history entry was selected before waiting.
func (c *undoClient) begin(ctx context.Context, wait bool) (*undoAttempt, error) {
	c.mu.Lock()
	if !wait && len(c.pending) > 0 {
		c.mu.Unlock()
		return nil, errors.New("another issue action is in progress")
	}
	attempt := &undoAttempt{client: c, ready: make(chan struct{})}
	c.pending = append(c.pending, attempt)
	if len(c.pending) == 1 {
		close(attempt.ready)
	}
	c.mu.Unlock()
	select {
	case <-attempt.ready:
		return attempt, nil
	case <-ctx.Done():
		attempt.complete()
		return nil, ctx.Err()
	}
}

func (c *undoClient) edit(ctx context.Context, projectID int64, ref, actor, kind string,
	write func(string) (*MutationResp, error),
) (*MutationResp, error) {
	attempt, err := c.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	instance := c.instance
	c.mu.Unlock()
	if instance.InstanceUID == "" {
		return &MutationResp{undo: attempt}, errors.New("daemon instance identity is unavailable")
	}
	before, err := c.GetIssueDetail(ctx, projectID, ref)
	if err != nil || before == nil || before.Issue == nil {
		if err == nil {
			err = errors.New("issue preflight returned no issue")
		}
		return &MutationResp{undo: attempt}, err
	}
	issue := *before.Issue
	if issue.UID == "" {
		return &MutationResp{undo: attempt}, errors.New("issue preflight returned no UID")
	}
	resp, err := write(issue.UID)
	if resp == nil {
		resp = &MutationResp{}
	}
	resp.undo = attempt
	if err != nil {
		// A write may have committed before transport or server failure.
		attempt.unknown = ambiguousWriteError(err)
		return resp, err
	}
	if !resp.Changed {
		return resp, nil
	}
	if resp.Issue == nil {
		attempt.boundary = "mutation response did not identify the changed issue"
		return resp, nil
	}
	if resp.Issue.UID != issue.UID || resp.Issue.ProjectID != issue.ProjectID {
		attempt.boundary = "mutation response identified a different issue"
		return resp, nil
	}
	entry := &undoEntry{kind: kind, uid: issue.UID, projectID: projectID,
		projectUID: issue.ProjectUID, actor: actor, before: issue,
		after: *resp.Issue, revision: resp.Issue.Revision, instanceUID: instance.InstanceUID, auth: instance.Auth}
	if kind == "owner.assign" && (issue.AssignmentExpiresOn != nil || resp.Issue.AssignmentExpiresOn != nil) {
		attempt.boundary = "owner edit involving an expiry cannot be undone"
		return resp, nil
	}
	if kind == "close" && (issue.RecurrenceID != nil || before.Lease != nil) {
		if issue.RecurrenceID != nil {
			attempt.boundary = "closing a recurring issue cannot be undone"
		} else {
			attempt.boundary = "closing a leased issue cannot be undone"
		}
		return resp, nil
	}
	if kind == "reopen" {
		if issue.RecurrenceID != nil {
			attempt.boundary = "reopening a recurring issue cannot be undone"
			return resp, nil
		}
		if issue.ClosedReason == nil || *issue.ClosedReason != "done" {
			attempt.boundary = "reopening an issue not closed as done cannot be undone"
			return resp, nil
		}
	}
	if kind == "link.add" {
		if resp.Link == nil || resp.Link.ID == 0 || resp.Link.From.UID == "" || resp.Link.To.UID == "" {
			attempt.boundary = "link response did not identify the created link"
			return resp, nil
		}
		link := *resp.Link
		entry.link = &link
	}
	attempt.entry = entry
	return resp, nil
}

func (c *undoClient) Close(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error) {
	return c.edit(ctx, projectID, ref, actor, "close", func(uid string) (*MutationResp, error) {
		return c.KataAPI.Close(ctx, projectID, uid, actor)
	})
}

func (c *undoClient) AddLink(ctx context.Context, projectID int64, ref string, body LinkBody, actor string) (*MutationResp, error) {
	resp, err := c.edit(ctx, projectID, ref, actor, "link.add", func(uid string) (*MutationResp, error) {
		return c.KataAPI.AddLink(ctx, projectID, uid, body, actor)
	})
	if resp != nil && resp.undo != nil && resp.undo.entry != nil {
		entry := resp.undo.entry
		link := entry.link
		validEndpoint := link != nil && (link.From.UID == entry.uid ||
			(body.Type == "related" && link.To.UID == entry.uid))
		if link == nil || link.Type != body.Type || !validEndpoint {
			resp.undo.entry = nil
			resp.undo.boundary = "link response did not match the created link"
		}
	}
	return resp, err
}

func (c *undoClient) Reopen(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error) {
	return c.edit(ctx, projectID, ref, actor, "reopen", func(uid string) (*MutationResp, error) {
		return c.KataAPI.Reopen(ctx, projectID, uid, actor)
	})
}

func (c *undoClient) Assign(ctx context.Context, projectID int64, ref, owner, actor string) (*MutationResp, error) {
	return c.edit(ctx, projectID, ref, actor, "owner.assign", func(uid string) (*MutationResp, error) {
		return c.KataAPI.Assign(ctx, projectID, uid, owner, actor)
	})
}

func (c *undoClient) SetPriority(ctx context.Context, projectID int64, ref string, priority *int64, actor string) (*MutationResp, error) {
	return c.edit(ctx, projectID, ref, actor, "priority.set", func(uid string) (*MutationResp, error) {
		return c.KataAPI.SetPriority(ctx, projectID, uid, priority, actor)
	})
}

func (c *undoClient) AddLabel(ctx context.Context, projectID int64, ref, label, actor string) (*MutationResp, error) {
	resp, err := c.edit(ctx, projectID, ref, actor, "label.add", func(uid string) (*MutationResp, error) {
		return c.KataAPI.AddLabel(ctx, projectID, uid, label, actor)
	})
	if resp != nil && resp.undo != nil && resp.undo.entry != nil {
		resp.undo.entry.label = label
	}
	return resp, err
}

func (c *undoClient) RemoveLabel(ctx context.Context, projectID int64, ref, label, actor string) (*MutationResp, error) {
	resp, err := c.edit(ctx, projectID, ref, actor, "label.remove", func(uid string) (*MutationResp, error) {
		return c.KataAPI.RemoveLabel(ctx, projectID, uid, label, actor)
	})
	if resp != nil && resp.undo != nil && resp.undo.entry != nil {
		resp.undo.entry.label = label
	}
	return resp, err
}

func (c *undoClient) EditBody(ctx context.Context, projectID int64, ref, body, actor string) (*MutationResp, error) {
	return c.edit(ctx, projectID, ref, actor, "body.edit", func(uid string) (*MutationResp, error) {
		return c.KataAPI.EditBody(ctx, projectID, uid, body, actor)
	})
}

func (c *undoClient) CloseWithEvidence(ctx context.Context, projectID int64, ref string, in CloseInput) (*MutationResp, error) {
	closer, ok := c.KataAPI.(evidenceCloseAPI)
	if !ok {
		return nil, errors.New("connected client cannot close with evidence")
	}
	return c.edit(ctx, projectID, ref, in.Actor, "close", func(uid string) (*MutationResp, error) {
		return closer.CloseWithEvidence(ctx, projectID, uid, in)
	})
}

func (c *undoClient) boundaryWrite(ctx context.Context, reason string, write func() (*MutationResp, error)) (*MutationResp, error) {
	attempt, err := c.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	resp, err := write()
	if resp == nil {
		resp = &MutationResp{}
	}
	resp.undo = attempt
	if err != nil {
		attempt.unknown = ambiguousWriteError(err)
	} else if resp.Changed {
		attempt.boundary = reason
	}
	return resp, err
}

func (c *undoClient) CreateIssue(ctx context.Context, projectID int64, body CreateIssueBody) (*MutationResp, error) {
	return c.boundaryWrite(ctx, "issue creation cannot be undone", func() (*MutationResp, error) {
		return c.KataAPI.CreateIssue(ctx, projectID, body)
	})
}

func (c *undoClient) AddComment(ctx context.Context, projectID int64, ref, body, actor string) (*MutationResp, error) {
	return c.boundaryWrite(ctx, "comment addition cannot be undone", func() (*MutationResp, error) {
		return c.KataAPI.AddComment(ctx, projectID, ref, body, actor)
	})
}

func (c *undoClient) ClaimTimedAssignment(ctx context.Context, projectID int64, ref, actor string, ttl time.Duration) (*MutationResp, error) {
	return c.boundaryWrite(ctx, "timed assignment cannot be undone", func() (*MutationResp, error) {
		return c.KataAPI.ClaimTimedAssignment(ctx, projectID, ref, actor, ttl)
	})
}
