package api

import "go.kenn.io/kata/internal/db"

// ReapCronRunBody records an attributed operator decision against current evidence.
type ReapCronRunBody struct {
	Actor            string `json:"actor,omitempty"`
	ExpectedRevision int64  `json:"expected_revision" minimum:"1"`
	Message          string `json:"message,omitempty" maxLength:"1024"`
	Force            bool   `json:"force,omitempty"`
}

// ReapCronRunRequest scopes the decision to one project run.
type ReapCronRunRequest struct {
	CronRunRequest
	Body ReapCronRunBody
}

// ReapCronRunResponse returns the unknown outcome and exact committed event.
type ReapCronRunResponse struct {
	Body struct {
		Run    CronRun    `json:"run"`
		Events []db.Event `json:"events"`
	}
}
