package daemon_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

func TestCronSummaryReplaySurvivesUnavailableUpstream(t *testing.T) {
	for _, mode := range []string{"offline", "unsupported", "busy"} {
		t.Run(mode, func(t *testing.T) {
			available, calls := true, 0
			f := newCronForwardFixture(t, func(_ *cronForwardFixture, cfg *daemon.ServerConfig) {
				cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
					return &http.Client{Transport: daemon.OutcomeTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						if !available && mode == "offline" {
							return nil, errors.New("fixture offline")
						}
						features := db.SupportedEventFeatures
						if !available {
							features = db.CronEventFeature
						}
						return &http.Response{StatusCode: 200, Header: http.Header{db.EventFeaturesHeader: []string{features}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
					})}, nil
				}
			})
			job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			id := cronHTTPUID(t)
			summary := map[string]any{"version": 1, "reason": "failed"}
			body := map[string]any{"actor": "tester", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": summary, "expected_revision": 0}
			request := func(uid string) (int, []byte) {
				return cronHTTP(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", f.sp.ID, uid), body)
			}
			status, raw := request(id)
			require.Equal(t, 200, status, string(raw))
			var first api.ObserveCronRunResponse
			require.NoError(t, json.Unmarshal(raw, &first.Body))
			require.EqualValues(t, 1, first.Body.Run.Revision)
			before, err := f.spoke.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			available = false
			var release func()
			if mode == "busy" {
				release, err = federationcoord.BeginRebind(t.Context(), federationcoord.Key(f.spoke.DB.InstanceUID(), f.sp.ID), f.spoke.DB, f.sp.ID)
				require.NoError(t, err)
				defer func() {
					if release != nil {
						release()
					}
				}()
			}
			status, raw = request(id)
			require.Equal(t, 200, status, string(raw))
			var replay api.ObserveCronRunResponse
			require.NoError(t, json.Unmarshal(raw, &replay.Body))
			require.True(t, replay.Body.Replayed)
			require.Equal(t, first.Body.Run, replay.Body.Run)
			require.Empty(t, replay.Body.Events)
			require.Equal(t, 1, calls, "a pure replay never needs an upstream probe")
			expected := 409
			if mode == "busy" {
				expected = 503
			}
			summary["message"] = "changed evidence"
			body["expected_revision"] = 1
			status, raw = request(id)
			require.Equal(t, expected, status, string(raw))
			body["expected_revision"] = 0
			status, raw = request(cronHTTPUID(t))
			require.Equal(t, expected, status, string(raw))
			after, err := f.spoke.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			// Immutable identity conflicts must never become an unauthenticated replay.
			delete(summary, "message")
			body["executor_label"] = "different-executor"
			status, raw = request(id)
			require.Equal(t, 409, status, string(raw))
			require.Contains(t, string(raw), "cron_conflict")
			delete(body, "executor_label")
			if release != nil {
				release()
				release = nil
			}
			// A rejected fresh write may fall back to v1, then enrich at its returned revision.
			fallbackID := cronHTTPUID(t)
			body["summary"] = map[string]any{"version": 1, "message": "reason: failed"}
			status, raw = request(fallbackID)
			require.Equal(t, 200, status, string(raw))
			var fallback api.ObserveCronRunResponse
			require.NoError(t, json.Unmarshal(raw, &fallback.Body))
			available = true
			body["summary"] = summary
			body["expected_revision"] = fallback.Body.Run.Revision
			status, raw = request(fallbackID)
			require.Equal(t, 200, status, string(raw))
			var enriched api.ObserveCronRunResponse
			require.NoError(t, json.Unmarshal(raw, &enriched.Body))
			require.EqualValues(t, 2, enriched.Body.Run.Revision)
			require.False(t, enriched.Body.Replayed)
		})
	}
}

func TestCronSummaryConcurrentEvidenceBetweenAdmissionPasses(t *testing.T) {
	var inject func()
	f := newCronForwardFixture(t, func(_ *cronForwardFixture, cfg *daemon.ServerConfig) {
		cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
			return &http.Client{Transport: daemon.OutcomeTransport(func(r *http.Request) (*http.Response, error) {
				if inject != nil {
					inject()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{db.EventFeaturesHeader: []string{db.SupportedEventFeatures}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			})}, nil
		}
	})
	job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	id := cronHTTPUID(t)
	in := db.ObserveCronRun{ProjectID: f.sp.ID, UID: id, Actor: "tester", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1}}
	first, err := f.spoke.DB.ObserveCronRun(t.Context(), in)
	require.NoError(t, err)
	inject = func() {
		in.ExpectedRevision = first.Run.Revision
		in.Summary.Message = "concurrent v1 update"
		_, err := f.spoke.DB.ObserveCronRun(t.Context(), in)
		require.NoError(t, err)
	}
	status, raw := cronHTTP(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", f.sp.ID, id), map[string]any{"actor": "tester", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": map[string]any{"version": 1, "reason": "failed"}, "expected_revision": first.Run.Revision})
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "cron_conflict")
	saved, err := f.spoke.DB.CronRun(t.Context(), f.sp.ID, id)
	require.NoError(t, err)
	require.EqualValues(t, 2, saved.Revision)
	require.Equal(t, "concurrent v1 update", saved.Summary.Message)
	require.Empty(t, saved.Summary.Reason)
}

func TestCronSummaryForwardingPreservesSharedHubClient(t *testing.T) {
	marker := errors.New("original redirect policy")
	var shared *http.Client
	f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
		var err error
		shared, err = cfg.FederationHubClient(t.Context(), f.credential)
		require.NoError(t, err)
		shared.CheckRedirect = func(*http.Request, []*http.Request) error { return marker }
		cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) { return shared, nil }
	})
	resp, raw := f.put(t, cronHTTPUID(t), "")
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.ErrorIs(t, shared.CheckRedirect(nil, nil), marker, "forwarding must clone a caller-owned HTTP client")
}

type summaryFinalizationStore struct{ db.Storage }

func (s summaryFinalizationStore) ObserveCronRun(context.Context, db.ObserveCronRun) (db.CronRunObservationResult, error) {
	return db.CronRunObservationResult{}, errors.Join(db.ErrCronSummaryAdmissionRequired, db.ErrTransactionFinalizationFailed)
}
func TestCronSummaryFinalizationFailureDoesNotAdmit(t *testing.T) {
	calls := 0
	f := newCronForwardFixture(t, func(_ *cronForwardFixture, cfg *daemon.ServerConfig) {
		cfg.DB = summaryFinalizationStore{cfg.DB}
		cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
			calls++
			return nil, errors.New("admission must not start")
		}
	})
	job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	status, raw := cronHTTP(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", f.sp.ID, cronHTTPUID(t)), map[string]any{"actor": "tester", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": map[string]any{"version": 1, "reason": "failed"}, "expected_revision": 0})
	require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
	require.Contains(t, string(raw), "access_unavailable")
	require.Zero(t, calls)
}

type summaryBindingReadFailureStore struct {
	db.Storage
	reads   int
	failure error
}

func (s *summaryBindingReadFailureStore) FederationBindingByProject(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	s.reads++
	if s.reads == 2 {
		return db.FederationBinding{}, s.failure
	}
	return s.Storage.FederationBindingByProject(ctx, projectID)
}
func TestCronSummaryAdmissionBindingReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		status  int
		code    string
	}{
		{"database", errors.New("fixture binding read failed"), http.StatusInternalServerError, "internal"},
		{"removed binding", db.ErrNotFound, http.StatusConflict, "unsupported_event_features"},
		{"finalization", db.ErrTransactionFinalizationFailed, http.StatusServiceUnavailable, "access_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var wrapped *summaryBindingReadFailureStore
			f := newCronForwardFixture(t, func(_ *cronForwardFixture, cfg *daemon.ServerConfig) {
				wrapped = &summaryBindingReadFailureStore{Storage: cfg.DB, failure: tc.failure}
				cfg.DB = wrapped
				cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
					calls++
					return nil, errors.New("must not probe")
				}
			})
			job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			uid := cronHTTPUID(t)
			before, err := f.spoke.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", f.sp.ID, uid), map[string]any{"actor": "tester", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": map[string]any{"version": 1, "reason": "failed"}, "expected_revision": 0})
			require.Equal(t, tc.status, status, string(raw))
			require.Contains(t, string(raw), `"code":"`+tc.code+`"`)
			require.Equal(t, 2, wrapped.reads)
			require.Zero(t, calls)
			_, err = f.spoke.DB.CronRun(t.Context(), f.sp.ID, uid)
			require.ErrorIs(t, err, db.ErrNotFound)
			after, err := f.spoke.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			release, err := federationcoord.BeginRebind(t.Context(), federationcoord.Key(f.spoke.DB.InstanceUID(), f.sp.ID), f.spoke.DB, f.sp.ID)
			require.NoError(t, err)
			release()
		})
	}
}
