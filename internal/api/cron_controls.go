package api

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
)

// CronProjectPause supplies an inline nullable object schema. Huma does not
// support nullable references to ordinary object schemas.
type CronProjectPause db.CronProjectPause

// Schema implements huma.SchemaProvider for an optional policy object.
func (CronProjectPause) Schema(r huma.Registry) *huma.Schema {
	schema := huma.SchemaFromType(r, reflect.TypeFor[db.CronProjectPause]())
	schema.Nullable = true
	return schema
}

// CronProjectPauseFromMetadata preserves absence as a JSON null.
func CronProjectPauseFromMetadata(metadata db.JSONBlob) *CronProjectPause {
	return (*CronProjectPause)(db.CronProjectPauseFromMetadata(metadata))
}

// CronProjectPauseBody changes root-owned project delivery policy.
type CronProjectPauseBody struct {
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// CronProjectPauseRequest permits an empty body for attributed principals.
type CronProjectPauseRequest struct {
	CronProjectRequest
	Body *CronProjectPauseBody `required:"false"`
}

// CronProjectPauseResponse returns retained policy and whether an event committed.
type CronProjectPauseResponse struct {
	Body struct {
		ProjectPause *CronProjectPause `json:"project_pause"`
		Changed      bool              `json:"changed"`
	}
}
