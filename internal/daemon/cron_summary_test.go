package daemon_test

import (
	"context"
	"crypto/ed25519"
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
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type summaryRoundTripper func(*http.Request) (*http.Response, error)

func (f summaryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCronSummaryCapabilitiesAndAPI(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil)
	require.Equal(t, 200, status, string(raw))
	var caps struct {
		EventFeatures []string `json:"event_features"`
	}
	require.NoError(t, json.Unmarshal(raw, &caps))
	require.Equal(t, []string{"cron_run_summary_v2", "cron_v1"}, caps.EventFeatures)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Actor: "operator", Name: "Review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	summary := map[string]any{"version": 1, "reason": "agent_quota", "trigger": "request", "usage": map[string]any{"model": "model-large", "cost_micros": 0, "currency": "USD"}, "steps": []any{map[string]any{"key": "review", "status": "failed", "exit_code": 0}}, "log_ref": "urn:example:run"}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t))
	body := map[string]any{"actor": "operator", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": summary, "expected_revision": 0}
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equal(t, 200, status, string(raw))
	var observed api.ObserveCronRunResponse
	require.NoError(t, json.Unmarshal(raw, &observed.Body))
	require.Equal(t, "agent_quota", observed.Body.Run.Summary.Reason)
	require.Equal(t, "request", observed.Body.Run.Summary.Trigger)
	require.NotNil(t, observed.Body.Run.Summary.Usage.CostMicros)
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equal(t, 200, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &observed.Body))
	require.True(t, observed.Body.Replayed)
	for _, invalid := range []any{nil, "", "INVALID"} {
		summary["reason"] = invalid
		status, raw = cronHTTP(t, env, http.MethodPut, path, body)
		require.Equal(t, 400, status, string(raw))
	}
	delete(summary, "reason")
	for _, invalid := range []any{nil, "", "Scheduled", "unknown"} {
		summary["trigger"] = invalid
		status, raw = cronHTTP(t, env, http.MethodPut, path, body)
		require.Equal(t, 400, status, string(raw))
	}
}

func TestCronSummarySpokeAdmission(t *testing.T) {
	for _, mode := range []string{"supports", "old", "trigger-old", "offline", "redirect", "credential-mismatch", "rejoin", "v1"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
				if mode == "credential-mismatch" {
					cred := f.credential
					cred.HubURL = "https://other.daemon.example"
					require.NoError(t, config.WriteFederationCredential(f.sp.UID, cred))
				}
				cfg.FederationHubClient = func(_ context.Context, cred config.FederationCredential) (*http.Client, error) {
					require.Equal(t, f.credential.HubURL, cred.HubURL)
					return &http.Client{Transport: summaryRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						if mode == "offline" {
							return nil, errors.New("fixture offline")
						}
						require.True(t, strings.HasSuffix(r.URL.Path, "/federation/metadata"))
						status := 200
						h := http.Header{db.EventFeaturesHeader: []string{"cron_run_summary_v2,cron_v1"}}
						if mode == "old" || mode == "trigger-old" {
							h.Set(db.EventFeaturesHeader, "cron_v1")
						}
						if mode == "redirect" {
							status = 302
							h.Set("Location", "https://other.daemon.example/metadata")
						}
						if mode == "rejoin" {
							binding, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
							require.NoError(t, err)
							binding.HubProjectID++
							_, err = f.spoke.DB.UpsertFederationBinding(t.Context(), binding)
							require.NoError(t, err)
						}
						return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
					})}, nil
				}
			})
			job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			summary := map[string]any{"version": 1, "reason": "failed"}
			if mode == "trigger-old" {
				summary = map[string]any{"version": 1, "trigger": "request"}
			}
			if mode == "v1" {
				delete(summary, "reason")
			}
			id := cronHTTPUID(t)
			before, err := f.spoke.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", f.sp.ID, id), map[string]any{"actor": "tester", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": summary, "expected_revision": 0})
			if mode == "supports" || mode == "v1" {
				require.Equal(t, 200, status, string(raw))
			} else {
				require.Equal(t, 409, status, string(raw))
				require.Contains(t, string(raw), "unsupported_event_features")
				_, err = f.spoke.DB.CronRun(t.Context(), f.sp.ID, id)
				require.ErrorIs(t, err, db.ErrNotFound)
				after, err := f.spoke.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			if mode == "v1" || mode == "credential-mismatch" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestCronSummaryAdmissionRechecksSupportAndReleasesCoordination(t *testing.T) {
	support, calls := true, 0
	f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
		cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
			return &http.Client{Transport: summaryRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				headers := http.Header{}
				if support {
					headers.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
				} else {
					headers.Set(db.EventFeaturesHeader, db.CronEventFeature)
				}
				return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			})}, nil
		}
	})
	job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	body := map[string]any{"actor": "tester", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "failed", "summary": map[string]any{"version": 1, "steps": []any{}}, "expected_revision": 0}
	for _, supported := range []bool{true, false, true} {
		support = supported
		id := cronHTTPUID(t)
		before, err := f.spoke.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw := cronHTTP(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", f.sp.ID, id), body)
		if support {
			require.Equal(t, 200, status, string(raw))
		} else {
			require.Equal(t, 409, status, string(raw))
			_, err := f.spoke.DB.CronRun(t.Context(), f.sp.ID, id)
			require.ErrorIs(t, err, db.ErrNotFound)
			after, err := f.spoke.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
		}
	}
	require.Equal(t, 3, calls, "a previous success or failure does not bypass the next probe")
}

func TestCronSummaryRelayMetadataCapabilities(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.private.ID, "admin")
		require.NoError(t, err)
		public, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		parent, err := store.ResolveAPIToken(t.Context(), "member-test-token")
		require.NoError(t, err)
		grant, err := store.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "summary-relay-fixture-token"})
		require.NoError(t, err)
		code, headers, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/metadata", f.private.ID), "", nil, map[string]string{"Authorization": "Bearer " + grant.Token, db.EventFeaturesHeader: db.SupportedEventFeatures})
		require.Equal(t, 200, code, string(raw))
		require.Equal(t, db.SupportedEventFeatures, headers.Get(db.EventFeaturesHeader))
	})
}

func TestCronRunReasonValidation(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "reason-project")
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs?reason=INVALID", project.ID), nil)
	require.Equal(t, 400, status, string(raw))
	require.Contains(t, string(raw), "validation")
}
