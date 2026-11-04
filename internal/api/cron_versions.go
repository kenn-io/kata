package api

import (
	"time"

	"go.kenn.io/kata/internal/db"
)

// ListCronVersionsRequest selects live, locally retained definition history.
type ListCronVersionsRequest struct {
	CronDefinitionRequest
	Limit          int    `query:"limit" minimum:"0" maximum:"100" default:"20"`
	BeforeEventUID string `query:"before_event_uid"`
}

// CronDefinitionVersion exposes exact stored intent rather than decoding
// opaque adapter options through float64 in the API or generated client.
type CronDefinitionVersion struct {
	EventUID      string               `json:"event_uid"`
	Operation     string               `json:"operation"`
	Actor         *string              `json:"actor"`
	DefinitionHLC db.CronDefinitionHLC `json:"definition_hlc"`
	Name          string               `json:"name"`
	Definition    JSONRawObject        `json:"definition"`
	UpdatedAt     time.Time            `json:"updated_at"`
	DeletedAt     *time.Time           `json:"deleted_at,omitempty"`
	Current       bool                 `json:"current"`
}

// ListCronVersionsResponse returns a live page of locally retained versions.
type ListCronVersionsResponse struct {
	Body struct {
		Versions           []CronDefinitionVersion `json:"versions"`
		NextBeforeEventUID string                  `json:"next_before_event_uid,omitempty"`
		SkippedEventUIDs   []string                `json:"skipped_event_uids,omitempty"`
	}
}

// CronVersionsFrom preserves raw definitions and optional baseline actors.
func CronVersionsFrom(page db.CronVersionPage) *ListCronVersionsResponse {
	out := &ListCronVersionsResponse{}
	out.Body.NextBeforeEventUID = page.NextBeforeEventUID
	out.Body.SkippedEventUIDs = page.SkippedEventUIDs
	out.Body.Versions = make([]CronDefinitionVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		out.Body.Versions = append(out.Body.Versions, CronDefinitionVersion{EventUID: v.EventUID, Operation: v.Operation, Actor: v.Actor, DefinitionHLC: v.DefinitionHLC, Name: v.Name, Definition: JSONRawObject(v.Definition), UpdatedAt: v.UpdatedAt, DeletedAt: v.DeletedAt, Current: v.Current})
	}
	return out
}
