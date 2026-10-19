package issuesync

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// PublishAdapter is optional and leaves existing read-only adapters unchanged.
type PublishAdapter interface {
	OpenPublish(context.Context, db.IssueSyncBinding) (PublishSession, error)
}

// PublishSession freezes a validated title/body request. Create must call admission
// immediately before its single mutation, after pacing, and must never retry it.
// ReadCreated receives the operator ID for adoption, or the saved canonical
// identity and locator for resumption (GitHub needs its repository issue number).
type PublishSession interface {
	PrepareCreate(context.Context, db.Issue) (jsontext.Value, error)
	Create(context.Context, jsontext.Value, func(context.Context) error) (db.IssuePublicationRemote, error)
	ReadCreated(context.Context, db.IssuePublicationRemote) (db.IssuePublicationRemote, error)
}

// PublisherConfig supplies storage, provider access, admission, and event delivery.
type PublisherConfig struct {
	Store          db.Storage
	Adapter        PublishAdapter
	Clock          func() time.Time
	RunTimeout     time.Duration
	DrainAdmission activity.WaitableAdmission
	EventSink      func(context.Context, int64, []db.Event) error
	EventSinkFrom  func(context.Context, int64, []db.Event, activity.Admission) error
}

// Publisher coordinates one foreground create or explicit recovery operation.
type Publisher struct{ config PublisherConfig }

// PublishRequest selects the native issue, actor, and optional recovery action.
type PublishRequest struct {
	ProjectID, IssueID                          int64
	Provider, Actor, Adopt, AttemptUID, Message string
	ResolveNotCreated                           bool
}

// PublishResult returns the verified source mapping and operation outcome.
type PublishResult struct {
	Provider   string `json:"provider"`
	IssueUID   string `json:"issue_uid"`
	IssueRef   string `json:"issue_ref"`
	SourceKey  string `json:"source_key"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`
	Result     string `json:"result"`
}

// PublishError deliberately excludes provider diagnostics and frozen payloads.
type PublishError struct {
	Code    string
	Pending *db.IssuePublicationPendingError
}

func (e *PublishError) Error() string {
	if e.Pending != nil {
		return e.Code + ": " + e.Pending.Error()
	}
	if message, hint, ok := e.ValidationDetails(); ok {
		return e.Code + ": " + message + " " + hint
	}
	return e.Code
}
func (e *PublishError) Unwrap() error {
	if e.Pending == nil {
		return nil
	}
	return e.Pending
}
func publishFailure(code string, p *db.IssuePublication, issueRef string) error {
	e := &PublishError{Code: code}
	if p != nil {
		e.Pending = &db.IssuePublicationPendingError{AttemptUID: p.AttemptUID, Provider: p.Provider, IssueUID: p.IssueUID, IssueRef: issueRef, Phase: p.Phase}
	}
	return e
}

// NewPublisher constructs a coordinator with a bounded operation lifetime.
func NewPublisher(c PublisherConfig) *Publisher {
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.RunTimeout <= 0 || c.RunTimeout > 25*time.Minute {
		c.RunTimeout = 25 * time.Minute
	}
	return &Publisher{config: c}
}

// Publish is an explicit foreground operation. Saved attempts are only inspected
// or attached; no saved phase grants a subsequent create authorization.
func (p *Publisher) Publish(ctx context.Context, r PublishRequest) (PublishResult, error) {
	var zero PublishResult
	if strings.TrimSpace(r.Actor) == "" || r.Provider == "" || r.ProjectID <= 0 || r.IssueID <= 0 ||
		(r.Adopt != "" && r.ResolveNotCreated) ||
		(r.ResolveNotCreated && (r.AttemptUID == "" || strings.TrimSpace(r.Message) == "")) ||
		(!r.ResolveNotCreated && (r.AttemptUID != "" || r.Message != "")) {
		return zero, publishFailure("invalid_publish_request", nil, "")
	}
	store, ok := p.config.Store.(db.IssuePublicationStore)
	if !ok || p.config.Adapter == nil {
		return zero, publishFailure("publishing_unavailable", nil, "")
	}
	var lease *activity.Lease
	if p.config.DrainAdmission != nil {
		var admitted bool
		lease, admitted, _ = p.config.DrainAdmission()
		if !admitted {
			return zero, publishFailure("daemon_draining", nil, "")
		}
	}
	defer lease.Release()
	ctx, cancel := context.WithTimeout(ctx, p.config.RunTimeout)
	defer cancel()
	pending, err := store.PendingIssuePublication(ctx, r.ProjectID)
	if err != nil {
		return zero, err
	}
	now := p.config.Clock().UTC().Truncate(time.Millisecond)
	claim, err := store.ClaimIssuePublication(ctx, r.ProjectID, r.IssueID, r.Provider, now, now.Add(-DefaultStaleLockTTL), pending != nil || r.Adopt != "" || r.ResolveNotCreated)
	if err != nil {
		return zero, err
	}
	result := PublishResult{Provider: r.Provider, IssueUID: claim.Issue.UID, IssueRef: claim.Issue.ShortID, SourceKey: claim.Binding.SourceKey}
	if claim.Mapping != nil {
		result.ExternalID = claim.Mapping.ExternalID
		result.Result = "already_published"
		return result, nil
	}
	retain := false
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), runnerCleanupTimeout)
		defer done()
		_ = store.ReleaseIssuePublication(cleanup, claim.Guard, retain)
	}()
	var issueRef string
	issueRefLoaded := false
	failure := func(code string, pending *db.IssuePublication) error {
		if pending != nil && !issueRefLoaded {
			project, projectErr := p.config.Store.ProjectByID(ctx, claim.Issue.ProjectID)
			if projectErr == nil && project.Name != "" && claim.Issue.ShortID != "" {
				issueRef = project.Name + "#" + claim.Issue.ShortID
			} else {
				issueRef = claim.Issue.UID
			}
			issueRefLoaded = true
		}
		return publishFailure(code, pending, issueRef)
	}
	pending = claim.Pending
	if r.ResolveNotCreated {
		if pending == nil || pending.AttemptUID != r.AttemptUID {
			return zero, failure("publication_attempt_mismatch", pending)
		}
		// This explicit, audited attestation may clear any matching phase,
		// including a retained identity. A later create needs a separate publish;
		// an incorrect attestation can create a duplicate.
		events, e := store.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: r.AttemptUID, Actor: r.Actor, Message: r.Message})
		if e != nil {
			return zero, e
		}
		result.Result = "resolved_not_created"
		return result, p.deliver(r.ProjectID, events, lease)
	}
	if pending != nil && r.Adopt == "" && (pending.Phase != "created" || pending.Remote == nil) {
		return zero, failure("publication_requires_recovery", pending)
	}
	if pending == nil && r.Adopt != "" && claim.Binding.Enabled {
		return zero, failure("restore_adoption_requires_disabled_binding", nil)
	}
	session, err := p.config.Adapter.OpenPublish(ctx, claim.Binding)
	if err != nil || session == nil {
		return zero, failure(publicationValidationCode(err, "publication_source_unavailable"), pending)
	}
	var remote db.IssuePublicationRemote
	if pending != nil || r.Adopt != "" {
		identity := db.IssuePublicationRemote{ExternalID: r.Adopt}
		if r.Adopt == "" {
			identity = *pending.Remote
		}
		remote, err = session.ReadCreated(ctx, identity)
		if err != nil {
			return zero, failure("publication_recovery_read_failed", pending)
		}
		result.Result = "recovered"
	} else {
		payload, e := session.PrepareCreate(ctx, claim.Issue)
		if e != nil {
			return zero, failure(publicationValidationCode(e, "publication_preflight_failed"), nil)
		}
		attemptUID, e := uid.New()
		if e != nil {
			return zero, e
		}
		saved, e := store.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: attemptUID, Actor: r.Actor, Revision: claim.Issue.Revision, Payload: payload}})
		if e != nil {
			return zero, e
		}
		pending = &saved
		dispatched := false
		remote, err = session.Create(ctx, saved.Payload, func(admitCtx context.Context) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if e := admitCtx.Err(); e != nil {
				return e
			}
			if !p.config.Clock().Before(now.Add(25 * time.Minute)) {
				return errors.New("publication claim horizon reached")
			}
			if e := store.DispatchIssuePublication(admitCtx, claim.Guard, saved.AttemptUID); e != nil {
				return e
			}
			dispatched = true
			pending.Phase = "dispatching"
			return nil
		})
		if !dispatched {
			return zero, failure("publication_not_dispatched", pending)
		}
		// A known identity is durable even when the caller or binding changed during
		// the remote request. Live authority is checked separately on attachment.
		if remote.ExternalID != "" {
			cleanup, done := context.WithTimeout(context.Background(), runnerCleanupTimeout)
			saveErr := store.RecordIssuePublicationCreated(cleanup, claim.Guard, saved.AttemptUID, remote)
			done()
			if saveErr != nil {
				retain = true
				return zero, failure("publication_identity_save_failed", pending)
			}
			pending.Phase = "created"
			pending.Remote = &remote
		} else {
			retain = true
		}
		if err != nil || remote.ExternalID == "" {
			return zero, failure("publication_create_unconfirmed", pending)
		}
		result.Result = "created"
	}
	attempt := ""
	if pending != nil {
		attempt = pending.AttemptUID
	}
	mapping, events, err := store.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: attempt, Remote: remote, RestoreAdoption: pending == nil && r.Adopt != ""})
	if err != nil {
		return zero, failure("publication_attachment_failed", pending)
	}
	result.ExternalID = mapping.ExternalID
	result.URL = remote.URL
	return result, p.deliver(r.ProjectID, events, lease)
}

func (p *Publisher) deliver(projectID int64, events []db.Event, lease *activity.Lease) error {
	if len(events) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), runnerCleanupTimeout)
	defer cancel()
	if p.config.EventSinkFrom != nil {
		var fork activity.Admission
		if lease != nil {
			fork = lease.Fork
		}
		return p.config.EventSinkFrom(ctx, projectID, events, fork)
	}
	if p.config.EventSink != nil {
		return p.config.EventSink(ctx, projectID, events)
	}
	return nil
}
