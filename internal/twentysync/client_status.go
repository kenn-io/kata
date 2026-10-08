package twentysync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

// StatusSession provides independently verified workflow reads and writes.
// Admission must run after pacing, immediately before the single PATCH attempt.
type StatusSession interface {
	ReadStatus(context.Context, Config, string) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, Config, string, string, func() error) (issuesync.StatusObservation, error)
}

func blockedStatus(message string) error {
	return &issuesync.StatusError{Message: message, Blocked: true}
}
func statusReadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if classified, ok := errors.AsType[*issuesync.StatusError](err); ok {
		return classified
	}
	return blockedStatus(err.Error())
}

func (s *clientSession) readStatus(ctx context.Context, c Config, id string) (issuesync.StatusObservation, Schema, error) {
	var zero issuesync.StatusObservation
	c, err := normalizeConfig(c)
	if err != nil {
		return zero, Schema{}, statusReadError(err)
	}
	if err := s.validate(c, true); err != nil {
		return zero, Schema{}, statusReadError(err)
	}
	id, err = CanonicalID(id)
	if err != nil {
		return zero, Schema{}, statusReadError(err)
	}
	schema, err := s.Schema(ctx, c)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	raw, err := s.read(ctx, http.MethodGet, "/rest/tasks/"+id+"?depth=0", nil)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	var envelope struct {
		Data *struct {
			Task jsontext.Value `json:"task"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data == nil {
		return zero, schema, blockedStatus("invalid Twenty task status response")
	}
	task, err := parseTask(envelope.Data.Task, false)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	if task.ID != id {
		return zero, schema, blockedStatus("Twenty task status response has wrong identity")
	}
	if task.Status != nil && !slices.Contains(schema.StatusOptions, *task.Status) {
		return zero, schema, blockedStatus("Twenty task status is absent from the live schema")
	}
	status, reason, err := classifyStatus(c, task.Status)
	if err != nil {
		return zero, schema, statusReadError(err)
	}
	observed := issuesync.StatusObservation{RawStatus: task.Status, Status: status, ClosedReason: reason, Version: task.UpdatedAt.UTC().Truncate(time.Millisecond)}
	if status == "closed" {
		observed.ClosedAt = new(observed.Version)
	}
	return observed, schema, nil
}

func (s *clientSession) ReadStatus(ctx context.Context, c Config, id string) (issuesync.StatusObservation, error) {
	observed, _, err := s.readStatus(ctx, c, id)
	return observed, err
}

// WriteStatus preserves matching substates and acknowledges only verified reads.
// A dropped response or unsuccessful readback leaves the shared intent pending.
func (s *clientSession) WriteStatus(ctx context.Context, c Config, id, desired string, admission func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if c.StatusSync != "two-way" || admission == nil {
		return zero, blockedStatus("Twenty writes require two-way mode and delivery admission")
	}
	if desired != "open" && desired != "closed" {
		return zero, blockedStatus("Twenty desired status must be open or closed")
	}
	c, err := normalizeConfig(c)
	if err != nil {
		return zero, statusReadError(err)
	}
	id, err = CanonicalID(id)
	if err != nil {
		return zero, statusReadError(err)
	}
	observed, schema, err := s.readStatus(ctx, c, id)
	if err != nil || observed.Status == desired {
		return observed, err
	}
	target := c.ClosedStatus
	if desired == "open" {
		target = c.OpenStatus
	}
	if !slices.Contains(schema.StatusOptions, target) {
		return zero, blockedStatus("Twenty status write target is missing from the live schema")
	}
	payload, err := json.Marshal(map[string]string{"status": target})
	if err != nil {
		return zero, blockedStatus("cannot encode Twenty status write")
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
	response, err := s.request(traced, http.MethodPatch, "/rest/tasks/"+id+"?depth=0", payload)
	if err != nil {
		return zero, &issuesync.StatusError{Message: "Twenty status write response failed", Ambiguous: sent()}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == 429 || response.StatusCode >= 500 {
		s.client.deferRequests(response.Header.Get("Retry-After"), 0)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		delivery := &issuesync.StatusError{Message: fmt.Sprintf("Twenty status write failed (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, Ambiguous: response.StatusCode >= 500, Blocked: response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != 429 && response.StatusCode != 409}
		if response.StatusCode == 429 {
			delivery.RetryAfter = s.client.cooldownRemaining()
		}
		return zero, delivery
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return zero, &issuesync.StatusError{Message: "Twenty status write response was incomplete", Ambiguous: true}
	}
	var envelope struct {
		Data *struct {
			Task *struct {
				ID string `json:"id"`
			} `json:"updateTask"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data == nil || envelope.Data.Task == nil {
		return zero, &issuesync.StatusError{Message: "Twenty status write response has no task identity", Ambiguous: true}
	}
	returned, err := CanonicalID(envelope.Data.Task.ID)
	if err != nil || returned != id {
		return zero, &issuesync.StatusError{Message: "Twenty status write response has wrong task identity", Ambiguous: true}
	}
	observed, err = s.ReadStatus(ctx, c, id)
	if err != nil || observed.Status != desired || observed.RawStatus == nil || *observed.RawStatus != target {
		return zero, &issuesync.StatusError{Message: "Twenty status write could not be verified", Ambiguous: true}
	}
	return observed, nil
}

var _ StatusSession = (*clientSession)(nil)
