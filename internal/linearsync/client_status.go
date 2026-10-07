package linearsync

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

// StatusSession supports independent reads and state-only admitted writes.
type StatusSession interface {
	ReadStatus(context.Context, Config, string) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, Config, string, string, func() error) (issuesync.StatusObservation, error)
}

const statusQuery = `query KataLinearStatus($id: String!) { issue(id: $id) { id updatedAt archivedAt trashed completedAt canceledAt team { id } project { id } state { id } } }`
const updateQuery = `mutation KataLinearUpdate($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success issue { id } } }`

func (s *clientSession) statusSchema(ctx context.Context, c Config) (statusSchema, error) {
	if _, err := s.Scope(ctx, c); err != nil {
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
	var schema statusSchema
	if cache != nil && cache.types != nil {
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
	raw, err := s.query(ctx, statusQuery, map[string]any{"id": canonical})
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	var data struct {
		Issue jsontext.Value `json:"issue"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return zero, schema, blockedStatus("invalid Linear status response")
	}
	i, err := decodeIssue(data.Issue, c, false)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	if i.ID != canonical || i.ArchivedAt != nil || i.Trashed {
		return zero, schema, blockedStatus("Linear mapped issue is unavailable or archived")
	}
	typ, ok := schema.types[i.StateID]
	if !ok {
		schema, err = s.statusSchema(ctx, c)
		if err != nil {
			return zero, schema, err
		}
		if cache != nil {
			*cache = schema
		}
		typ, ok = schema.types[i.StateID]
	}
	if !ok {
		return zero, schema, blockedStatus("Linear issue state is absent from selected team workflow")
	}
	status, reason, at, err := issueStatus(i, typ)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	obs := issuesync.StatusObservation{RawStatus: &i.StateID, Status: status, ClosedAt: at, Version: i.UpdatedAt.UTC().Truncate(time.Millisecond)}
	if reason != nil {
		obs.ClosedReason = *reason
	}
	return obs, schema, nil
}
func (s *clientSession) ReadStatus(ctx context.Context, c Config, id string) (issuesync.StatusObservation, error) {
	obs, _, err := s.readStatusCached(ctx, c, id, nil)
	return obs, err
}

// WriteStatus dispatches once. Unproven execution retains durable pending intent.
func (s *clientSession) WriteStatus(ctx context.Context, c Config, id, desired string, admission func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if c.StatusSync != "two-way" || admission == nil {
		return zero, blockedStatus("Linear writes require two-way mode and delivery admission")
	}
	if desired != "open" && desired != "closed" {
		return zero, blockedStatus("Linear status must be open or closed")
	}
	obs, schema, err := s.readStatusCached(ctx, c, id, nil)
	if err != nil || obs.Status == desired {
		return obs, err
	}
	target, err := schema.target(c, desired)
	if err != nil {
		return zero, err
	}
	id, err = CanonicalID(id)
	if err != nil {
		return zero, statusReadError(err)
	}
	payload, err := json.Marshal(map[string]any{"query": updateQuery, "variables": map[string]any{"id": id, "input": map[string]string{"stateId": target}}})
	if err != nil {
		return zero, blockedStatus("cannot encode Linear status write")
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
	traced, sent := issuesync.TrackRequestWrite(ctx)
	req, err := http.NewRequestWithContext(traced, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return zero, blockedStatus("cannot build Linear status write")
	}
	req.Header.Set("Authorization", s.authorization)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := s.client.http.Do(req)
	if err != nil {
		return zero, &issuesync.StatusError{Message: "Linear status write response unavailable", Ambiguous: sent()}
	}
	raw, readErr := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	_ = res.Body.Close()
	s.client.headersCooldown(res.Header, false, 0)
	var envelope graphEnvelope
	decodeErr := json.Unmarshal(raw, &envelope)
	limited, auth := graphClassification(envelope.Errors)
	limited = limited || res.StatusCode == 429
	if limited || res.StatusCode >= 500 {
		s.client.headersCooldown(res.Header, true, 0)
	}
	// A rejection is definitive only when no mutation data accompanies it.
	hasData := len(envelope.Data) > 0 && string(envelope.Data) != "null" && string(envelope.Data) != "{}"
	if !hasData && (limited || auth || res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode >= 300 && res.StatusCode < 400) {
		return zero, &issuesync.StatusError{Message: "Linear status write rejected", HTTPStatus: res.StatusCode, Blocked: !limited, RetryAfter: s.client.cooldownRemaining()}
	}
	if res.StatusCode != 200 || readErr != nil || len(raw) > maxResponseBytes || decodeErr != nil || len(envelope.Errors) > 0 {
		return zero, &issuesync.StatusError{Message: "Linear status write outcome is unproven", HTTPStatus: res.StatusCode, Ambiguous: true}
	}
	var data struct {
		Update *struct {
			Success *bool     `json:"success"`
			Issue   *identity `json:"issue"`
		} `json:"issueUpdate"`
	}
	if json.Unmarshal(envelope.Data, &data) != nil || data.Update == nil || data.Update.Success == nil || !*data.Update.Success || data.Update.Issue == nil || data.Update.Issue.ID != id {
		return zero, &issuesync.StatusError{Message: "Linear status write payload is unproven", Ambiguous: true}
	}
	obs, err = s.ReadStatus(ctx, c, id)
	if err != nil || obs.Status != desired || obs.RawStatus == nil || *obs.RawStatus != target {
		return zero, &issuesync.StatusError{Message: "Linear status write could not be verified", Ambiguous: true}
	}
	return obs, nil
}
