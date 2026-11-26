package api

import "go.kenn.io/kata/internal/db"

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
		Replace   *bool  `json:"replace,omitempty" doc:"false answers 409 notification_held instead of replacing the recipient's held request; absent means true"`
	}
}

// ClearNotificationRequest removes a recipient's request while it is still one
// that the given actor sent.
type ClearNotificationRequest struct {
	ProjectTarget
	Ref  string `path:"ref" required:"true"`
	Body struct {
		Actor  string `json:"actor,omitempty"`
		To     string `json:"to" required:"true" doc:"Recipient address: <actor> or <actor>/<teammate>"`
		IfFrom string `json:"if_from" required:"true" doc:"Remove the request only while its stored from field equals this actor or, when this names the caller on a push-enabled spoke, the spoke's bound actor. A stored-field check, not proof of who wrote it"`
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
