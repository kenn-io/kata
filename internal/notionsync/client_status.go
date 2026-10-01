package notionsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/kata/internal/issuesync"
)

// StatusSession adds optional status-only operations to a read session.
// Admission runs after rate-limit pacing, immediately before a PATCH dispatch.
type StatusSession interface {
	ReadStatus(context.Context, Config, string) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, Config, string, string, func() error) (issuesync.StatusObservation, error)
}

var _ StatusSession = (*clientSession)(nil)

func retryableStatusSchemaError(err error) error {
	var statusErr *issuesync.StatusError
	if !errors.As(err, &statusErr) || !statusErr.Blocked {
		return err
	}
	return &issuesync.StatusError{
		Message:    err.Error(),
		HTTPStatus: statusErr.HTTPStatus,
		RetryAfter: statusErr.RetryAfter,
		Ambiguous:  statusErr.Ambiguous,
	}
}

func blockedStatusPageError(err error) error {
	if err == nil {
		return nil
	}
	return &issuesync.StatusError{Message: err.Error(), Blocked: true}
}

func (s *clientSession) statusSchema(ctx context.Context, cfg Config) (Config, StatusSchema, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return Config{}, StatusSchema{}, err
	}
	source, err := s.DataSource(ctx, cfg.DataSourceID)
	if err != nil {
		return Config{}, StatusSchema{}, retryableStatusSchemaError(err)
	}
	schema, err := ResolveStatusSchema(cfg, source)
	if err != nil {
		return Config{}, StatusSchema{}, err
	}
	database, err := s.Database(ctx, source.DatabaseID)
	if err != nil {
		return Config{}, StatusSchema{}, retryableStatusSchemaError(err)
	}
	for _, child := range database.DataSources {
		if child.ID == cfg.DataSourceID {
			cfg.DatabaseID = database.ID
			return cfg, schema, nil
		}
	}
	return Config{}, StatusSchema{}, fmt.Errorf("notion parent database no longer contains the data source")
}

type statusSchemaCache struct {
	config Config
	schema StatusSchema
	ready  bool
}

func (s *clientSession) readStatus(ctx context.Context, cfg Config, pageID string) (issuesync.StatusObservation, StatusSchema, error) {
	return s.readStatusCached(ctx, cfg, pageID, nil)
}

func (s *clientSession) readStatusCached(ctx context.Context, cfg Config, pageID string, cache *statusSchemaCache) (issuesync.StatusObservation, StatusSchema, error) {
	var zero issuesync.StatusObservation
	pageID, err := canonicalID(pageID)
	if err != nil {
		return zero, StatusSchema{}, blockedStatusPageError(fmt.Errorf("invalid Notion status page identity"))
	}
	var schema StatusSchema
	if cache != nil && cache.ready {
		cfg, schema = cache.config, cache.schema
	} else {
		cfg, schema, err = s.statusSchema(ctx, cfg)
		if err != nil {
			return zero, StatusSchema{}, err
		}
		if cache != nil {
			*cache = statusSchemaCache{config: cfg, schema: schema, ready: true}
		}
	}
	var wire wirePage
	if err := s.request(ctx, http.MethodGet, "/v1/pages/"+pageID, nil, &wire); err != nil {
		return zero, schema, err
	}
	page, available, err := wire.observation(cfg)
	if err != nil {
		return zero, schema, blockedStatusPageError(err)
	}
	if page.ID != pageID || !available {
		return zero, schema, blockedStatusPageError(fmt.Errorf("notion status page is unavailable or outside the selected data source"))
	}
	state, err := schema.Classify(page.StatusID)
	if err != nil {
		// A page can reference an option created between the schema and page
		// reads. Refresh once; never guess the classification of an unknown ID.
		cfg, schema, err = s.statusSchema(ctx, cfg)
		if err != nil {
			return zero, schema, err
		}
		if cfg.DatabaseID != page.DatabaseID {
			return zero, schema, blockedStatusPageError(fmt.Errorf("notion status page parent changed during schema refresh"))
		}
		if cache != nil {
			*cache = statusSchemaCache{config: cfg, schema: schema, ready: true}
		}
		state, err = schema.Classify(page.StatusID)
		if err != nil {
			return zero, schema, blockedStatusPageError(err)
		}
	}
	return issuesync.StatusObservation{RawStatus: page.StatusID, Status: state, Version: page.UpdatedAt, Locator: page.ID}, schema, nil
}

// ReadStatus fetches page metadata and workflow membership without page content.
func (s *clientSession) ReadStatus(ctx context.Context, cfg Config, pageID string) (issuesync.StatusObservation, error) {
	observed, _, err := s.readStatus(ctx, cfg, pageID)
	return observed, err
}

// WriteStatus preserves matching provider substates and verifies each write by
// reading back the page and its live schema. Ambiguous PATCHes are not retried.
func (s *clientSession) WriteStatus(ctx context.Context, cfg Config, pageID, desired string, admission func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if cfg.StatusSync != "two-way" || admission == nil {
		return zero, fmt.Errorf("notion status writes require two-way mode and delivery admission")
	}
	if desired != "open" && desired != "closed" {
		return zero, fmt.Errorf("notion status transition must be open or closed")
	}
	observed, schema, err := s.readStatus(ctx, cfg, pageID)
	if err != nil || observed.Status == desired {
		return observed, err
	}
	target, err := schema.Target(desired)
	if err != nil {
		return zero, err
	}
	payload := map[string]any{"properties": map[string]any{cfg.StatusPropertyID: map[string]any{"status": map[string]any{"id": target}}}}
	var response wirePage
	if err := s.requestWithAdmission(ctx, http.MethodPatch, "/v1/pages/"+observed.Locator, payload, &response, admission); err != nil {
		return zero, err
	}
	if _, err := validateResponseID(response.Object, "page", response.ID, observed.Locator); err != nil {
		return zero, &issuesync.StatusError{Message: "notion status response has wrong object identity", Ambiguous: true}
	}
	observed, err = s.ReadStatus(ctx, cfg, observed.Locator)
	if err != nil {
		return zero, &issuesync.StatusError{Message: "notion status write could not be verified", Ambiguous: true}
	}
	if observed.Status != desired {
		return zero, &issuesync.StatusError{Message: "notion status write readback differs from the requested state", Ambiguous: true}
	}
	return observed, nil
}
