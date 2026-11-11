package api

import "go.kenn.io/kata/internal/db"

// ProjectNotificationActorRequest resolves the effective notification actor
// for one readable project. Authenticated identity takes precedence over the
// optional actor fallback.
type ProjectNotificationActorRequest struct {
	ProjectID      int64  `path:"project_id"`
	RequestedActor string `query:"requested_actor"`
}

// ProjectNotificationActorResponse returns only the actor needed to select a
// notification inbox.
type ProjectNotificationActorResponse struct {
	Body struct {
		EffectiveActor string `json:"effective_actor"`
	}
}

// NotifyIssueRequest resolves comment attention recipients at the writing daemon.
type NotifyIssueRequest struct {
	ProjectTarget
	Ref  string `path:"ref" required:"true"`
	Body struct {
		Actor     string `json:"actor,omitempty"`
		Teammate  string `json:"teammate,omitempty"`
		To        string `json:"to,omitempty"`
		Re        string `json:"re,omitempty"`
		Message   string `json:"message" required:"true"`
		Broadcast bool   `json:"broadcast,omitzero"`
		Teammates bool   `json:"teammates,omitzero"`
	}
}

// NotifyIssueResponse returns the committed attention mutation and audience.
type NotifyIssueResponse struct {
	ProjectNameHeader
	Body struct {
		Issue      db.Issue  `json:"issue"`
		Event      *db.Event `json:"event,omitempty"`
		Changed    bool      `json:"changed"`
		Recipients []string  `json:"recipients"`
	}
}
