package api

import "go.kenn.io/kata/internal/db"

// SetCronAccessRequest replaces both local cron grants with mandatory revision CAS.
type SetCronAccessRequest struct {
	CronProjectRequest
	Body struct {
		Define   db.CronGrant `json:"define"`
		Observe  db.CronGrant `json:"observe"`
		Revision int64        `json:"revision" minimum:"0"`
	}
}

// CronAccessResponse exposes owner-only local policy and its exact audit event.
type CronAccessResponse struct {
	Body struct {
		Policy db.ProjectCronAccessPolicy `json:"policy"`
		Event  *db.Event                  `json:"event,omitempty"`
	}
}
