package planesync

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

// StatusSession optionally extends a read session with independently verified
// workflow reads and state-only writes. Admission runs after shared pacing.
type StatusSession interface {
	ReadStatus(context.Context, Config, string) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, Config, string, string, func() error) (issuesync.StatusObservation, error)
}

var _ StatusSession = (*clientSession)(nil)

func (s *clientSession) statusSchema(ctx context.Context, c Config) (statusSchema, error) {
	if err := s.validate(c); err != nil {
		return statusSchema{}, statusReadError(err)
	}
	if _, err := s.Project(ctx, c); err != nil {
		return statusSchema{}, statusReadError(err)
	}
	states, err := s.States(ctx, c)
	if err != nil {
		return statusSchema{}, statusReadError(err)
	}
	return resolveStatusSchema(states)
}
func (s *clientSession) readStatusCached(ctx context.Context, c Config, id string, cache *statusSchema) (issuesync.StatusObservation, statusSchema, error) {
	var zero issuesync.StatusObservation
	canonical, err := CanonicalID(id)
	if err != nil {
		return zero, statusSchema{}, statusReadError(err)
	}
	if err := s.validate(c); err != nil {
		return zero, statusSchema{}, statusReadError(err)
	}
	schema := statusSchema{}
	if cache != nil && cache.groups != nil {
		schema = *cache
	} else {
		schema, err = s.statusSchema(ctx, c)
		if err != nil {
			return zero, schema, err
		}
		if cache != nil {
			*cache = schema
		}
	}
	raw, err := s.get(ctx, s.path()+"work-items/"+canonical+"/")
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	var item struct {
		ID          string     `json:"id"`
		Project     string     `json:"project"`
		State       *string    `json:"state"`
		UpdatedAt   time.Time  `json:"updated_at"`
		CompletedAt *time.Time `json:"completed_at"`
		ArchivedAt  *string    `json:"archived_at"`
		DeletedAt   *string    `json:"deleted_at"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return zero, schema, blockedStatus("invalid Plane status response")
	}
	returnedID, idErr := CanonicalID(item.ID)
	project, projectErr := CanonicalID(item.Project)
	if idErr != nil || projectErr != nil || returnedID != canonical || project != c.ProjectID || item.ArchivedAt != nil || item.DeletedAt != nil {
		return zero, schema, blockedStatus("Plane status work item is unavailable or outside the selected project")
	}
	if item.State == nil || !validSourceTime(item.UpdatedAt) {
		return zero, schema, blockedStatus("incomplete Plane status response")
	}
	state, err := CanonicalID(*item.State)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	group, ok := schema.groups[state]
	if !ok {
		schema, err = s.statusSchema(ctx, c)
		if err != nil {
			return zero, schema, err
		}
		if cache != nil {
			*cache = schema
		}
		group, ok = schema.groups[state]
	}
	if !ok {
		return zero, schema, blockedStatus("Plane work item state is absent from the project workflow")
	}
	observed := issuesync.StatusObservation{RawStatus: &state, Status: "open", Version: item.UpdatedAt.UTC().Truncate(time.Millisecond), SchemaFingerprint: schema.fingerprint}
	if group == "completed" || group == "cancelled" {
		observed.Status = "closed"
		observed.ClosedReason = "done"
		at := observed.Version
		if group == "cancelled" {
			observed.ClosedReason = "wontfix"
		} else if item.CompletedAt != nil {
			if !validSourceTime(*item.CompletedAt) || item.CompletedAt.After(item.UpdatedAt) {
				return zero, schema, blockedStatus("invalid Plane completion timestamp")
			}
			at = item.CompletedAt.UTC().Truncate(time.Millisecond)
		}
		observed.ClosedAt = &at
	}
	return observed, schema, nil
}
func (s *clientSession) ReadStatus(ctx context.Context, c Config, id string) (issuesync.StatusObservation, error) {
	observed, _, err := s.readStatusCached(ctx, c, id, nil)
	return observed, err
}

// WriteStatus sends one PATCH and verifies identity, live workflow membership,
// and resulting state using a fresh read. A failed PATCH is never auto-retried.
func (s *clientSession) WriteStatus(ctx context.Context, c Config, id, desired string, admission func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if c.StatusSync != "two-way" || admission == nil {
		return zero, blockedStatus("Plane writes require two-way mode and delivery admission")
	}
	if desired != "open" && desired != "closed" {
		return zero, blockedStatus("Plane status must be open or closed")
	}
	observed, schema, err := s.readStatusCached(ctx, c, id, nil)
	if err != nil || observed.Status == desired {
		return observed, err
	}
	target, err := schema.target(c, desired)
	if err != nil {
		return zero, err
	}
	id, err = CanonicalID(id)
	if err != nil {
		return zero, statusReadError(err)
	}
	payload, err := json.Marshal(map[string]string{"state": target})
	if err != nil {
		return zero, err
	}
	if err := s.client.admit(ctx); err != nil {
		return zero, err
	}
	if err := admission(); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, s.config.APIOrigin+s.path()+"work-items/"+id+"/", bytes.NewReader(payload))
	if err != nil {
		return zero, err
	}
	req.Header.Set("X-API-Key", s.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.http.Do(req)
	if err != nil {
		return zero, &issuesync.StatusError{Message: "Plane status write response was lost", Ambiguous: true}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == 429 || response.StatusCode >= 500 {
		s.client.deferRequests(response.Header.Get("Retry-After"), 0)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return zero, &issuesync.StatusError{Message: fmt.Sprintf("Plane status write failed (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, Ambiguous: response.StatusCode >= 500, Blocked: response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != 429 && response.StatusCode != 409}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return zero, &issuesync.StatusError{Message: "Plane status write response was incomplete", Ambiguous: true}
	}
	var identity struct {
		ID      string `json:"id"`
		Project string `json:"project"`
	}
	returned, project := "", ""
	if json.Unmarshal(raw, &identity) == nil {
		returned, _ = CanonicalID(identity.ID)
		project, _ = CanonicalID(identity.Project)
	}
	if returned != id || project != c.ProjectID {
		return zero, &issuesync.StatusError{Message: "Plane status write response has wrong object identity", Ambiguous: true}
	}
	observed, err = s.ReadStatus(ctx, c, id)
	if err != nil || observed.Status != desired || observed.RawStatus == nil || *observed.RawStatus != target {
		return zero, &issuesync.StatusError{Message: "Plane status write could not be verified", Ambiguous: true}
	}
	return observed, nil
}
