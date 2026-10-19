package api

// PublishIssueRequest authorizes one foreground publication or recovery.
type PublishIssueRequest struct {
	ProjectID int64  `path:"project_id"`
	Provider  string `path:"provider"`
	Body      PublishIssueRequestBody
}

// PublishIssueRequestBody selects the native issue and explicit recovery action.
type PublishIssueRequestBody struct {
	IssueRef          string `json:"issue_ref" minLength:"1"`
	Actor             string `json:"actor,omitempty"`
	Adopt             string `json:"adopt,omitempty"`
	ResolveNotCreated bool   `json:"resolve_not_created,omitempty"`
	Attempt           string `json:"attempt,omitempty"`
	Message           string `json:"message,omitempty"`
}

// PublishIssueResponse wraps the publication identity returned to a client.
type PublishIssueResponse struct{ Body PublishIssueResponseBody }

// PublishIssueResponseBody reports the source mapping and publication outcome.
type PublishIssueResponseBody struct {
	Provider   string `json:"provider"`
	IssueUID   string `json:"issue_uid"`
	IssueRef   string `json:"issue_ref"`
	SourceKey  string `json:"source_key"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`
	Result     string `json:"result"`
}

// PendingIssuePublicationOut contains recovery identity, never saved content.
type PendingIssuePublicationOut struct {
	AttemptUID string `json:"attempt_uid"`
	Provider   string `json:"provider"`
	IssueUID   string `json:"issue_uid"`
	Phase      string `json:"phase"`
}
