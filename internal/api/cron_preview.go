package api

import "time"

// CronOccurrence shows a due instant in UTC and in the effective trigger zone.
type CronOccurrence struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}

// CronNextSweep is the first hypothetical planner batch at a captured clock.
// Anchor and Through are the stored checkpoint, before planning.
type CronNextSweep struct {
	Now     time.Time        `json:"now"`
	Anchor  *time.Time       `json:"anchor,omitzero"`
	Through *time.Time       `json:"through,omitzero"`
	Due     []CronOccurrence `json:"due,omitzero"`
	More    bool             `json:"more"`
	Error   string           `json:"error,omitzero"`
}

// CronPreview describes occurrences without changing delivery progress.
type CronPreview struct {
	JobUID             string           `json:"job_uid,omitzero"`
	ScheduledBy        string           `json:"scheduled_by"`
	Timezone           string           `json:"timezone"`
	ConfiguredTimezone string           `json:"configured_timezone,omitzero"`
	From               time.Time        `json:"from"`
	Anchor             *time.Time       `json:"anchor,omitzero"`
	AnchorSource       string           `json:"anchor_source,omitzero"`
	Count              int              `json:"count"`
	Occurrences        []CronOccurrence `json:"occurrences"`
	Exhausted          bool             `json:"exhausted"`
	Error              string           `json:"error,omitzero"`
	Active             *bool            `json:"active,omitzero"`
	InactiveReason     string           `json:"inactive_reason,omitzero"`
	NextSweep          *CronNextSweep   `json:"next_sweep,omitzero"`
	Note               string           `json:"note,omitzero"`
}

// PreviewCronJobRequest selects a stored job and optional preview bounds.
// Count stays textual so invalid bounds receive the ordinary validation envelope.
type PreviewCronJobRequest struct {
	CronDefinitionRequest
	Count string `query:"count" default:"10"`
	From  string `query:"from"`
}

// PreviewCronDefinitionBody accepts an unsaved definition with optional bounds.
type PreviewCronDefinitionBody struct {
	Definition CronJobDefinition `json:"definition"`
	Count      *int              `json:"count,omitempty"`
	From       string            `json:"from,omitempty"`
}

// PreviewCronDefinitionRequest previews within the selected project's authority.
type PreviewCronDefinitionRequest struct {
	ProjectID int64 `path:"project_id"`
	Body      PreviewCronDefinitionBody
}

// CronPreviewResponse wraps the read-only evaluation result.
type CronPreviewResponse struct {
	Body struct {
		Preview CronPreview `json:"preview"`
	}
}
