package client_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

// Handwritten tracker calls use the same authenticated/scoped request pipeline
// as generated calls. They must not create a second unscoped HTTP client.
func TestTrackerClientPreservesScopeEditorsAndOperationUID(t *testing.T) {
	const operation = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		require.Equal(t, `["id:7"]`, r.Header.Get("X-Kata-Project-Scope"))
		require.Equal(t, "mcp", r.Header.Get("X-Kata-Tracker-Client"))
		require.Equal(t, "/api/v1/projects/7/tracker-operations", r.URL.Path)
		var body kataclient.TrackerOperation
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		require.Equal(t, operation, body.UID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","uid":"` + operation + `","selection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAY","generation":"one"},"kind":"run","state":"pending"}`))
	}))
	t.Cleanup(server.Close)
	c, err := kataclient.NewWithHTTPClient(server.URL, server.Client(), kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer fixture-token")
		return nil
	}))
	require.NoError(t, err)
	c, err = c.WithProjectScope([]string{"id:7"})
	require.NoError(t, err)
	result, err := c.TrackerOperation(t.Context(), 7, kataclient.TrackerOperation{UID: operation, Kind: "run", Provider: "notion", Resource: "notion:source"}, kataclient.TrackerMCPClient)
	require.NoError(t, err)
	require.Equal(t, operation, result.UID)
	require.Equal(t, "pending", string(result.State))
	_, err = c.TrackerOperationReceipt(t.Context(), 7, "../escape")
	require.Error(t, err)
}

func trackerClientReceiptFixture() kataclient.TrackerReceipt {
	return kataclient.TrackerReceipt{RootInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Selection: kataclient.TrackerSelection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "one"}, Kind: "run", State: "complete", Result: &kataclient.TrackerOperationResult{Code: "ok"}}
}
func TestTrackerClientRejectsInvalidReceiptClassifications(t *testing.T) {
	cases := map[string]func(*kataclient.TrackerReceipt){
		"private-code":      func(r *kataclient.TrackerReceipt) { r.State = "failed"; r.Result.Code = "fixture-private-marker" },
		"complete-error":    func(r *kataclient.TrackerReceipt) { r.Result.Code = "tracker_timeout" },
		"pending-result":    func(r *kataclient.TrackerReceipt) { r.State = "pending" },
		"wrong-uid":         func(r *kataclient.TrackerReceipt) { r.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAY" },
		"missing-root":      func(r *kataclient.TrackerReceipt) { r.RootInstanceUID = "" },
		"missing-project":   func(r *kataclient.TrackerReceipt) { r.ProjectUID = "" },
		"missing-selection": func(r *kataclient.TrackerReceipt) { r.Selection.Generation = "" },
		"private-kind":      func(r *kataclient.TrackerReceipt) { r.Kind = "fixture-private-marker" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			response := trackerClientReceiptFixture()
			change(&response)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, response)
			}))
			t.Cleanup(server.Close)
			c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			receipt, err := c.TrackerOperationReceipt(t.Context(), 7, "01ARZ3NDEKTSV4RRFFQ69G5FAZ")
			require.Error(t, err)
			require.Nil(t, receipt)
			require.Equal(t, http.StatusOK, kataclient.StatusCode(err))
			require.NotContains(t, err.Error(), "fixture-private-marker")
		})
	}
}
func TestTrackerClientRejectsMalformedReceiptWithoutDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"fixture-private-marker"`))
	}))
	t.Cleanup(server.Close)
	c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	receipt, err := c.TrackerOperationReceipt(t.Context(), 7, "01ARZ3NDEKTSV4RRFFQ69G5FAZ")
	require.Error(t, err)
	require.Nil(t, receipt)
	require.Equal(t, http.StatusOK, kataclient.StatusCode(err))
	require.NotContains(t, err.Error(), "fixture-private-marker")
}
func TestTrackerClientSettlementRequiresExactOwnerFacts(t *testing.T) {
	mutations := map[string]func(*tracker.ControlSettlement){
		"valid":           func(*tracker.ControlSettlement) {},
		"private-kind":    func(s *tracker.ControlSettlement) { s.Kind = "fixture-private-marker" },
		"missing-time":    func(s *tracker.ControlSettlement) { s.RecordedAt = time.Time{} },
		"wrong-authority": func(s *tracker.ControlSettlement) { s.Operator.AuthorityInstanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FAY" },
		"member":          func(s *tracker.ControlSettlement) { s.Operator.Kind = tracker.SubjectAccount },
		"wrong-subject":   func(s *tracker.ControlSettlement) { s.Operator.Subject = "fixture-private-marker" },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			response := trackerClientReceiptFixture()
			response.State = tracker.ControlFailed
			response.Result.Code = "tracker_outcome_unknown"
			response.Settlement = &tracker.ControlSettlement{Operator: tracker.Principal{AuthorityInstanceUID: response.RootInstanceUID, Kind: tracker.SubjectOwnerLocal, Subject: response.RootInstanceUID}, Kind: "abandon_unresolved_control", RecordedAt: time.Now().UTC()}
			change(response.Settlement)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, response)
			}))
			t.Cleanup(server.Close)
			c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			receipt, err := c.TrackerOperationReceipt(t.Context(), 7, response.UID)
			if name == "valid" {
				require.NoError(t, err)
				require.Equal(t, response.Settlement, receipt.Settlement)
			} else {
				require.Error(t, err)
				require.Nil(t, receipt)
				require.NotContains(t, err.Error(), "fixture-private-marker")
			}
		})
	}
}
func TestTrackerClientErrorKeepsOnlyPublicClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(504)
		_, _ = w.Write([]byte(`{"status":504,"error":{"code":"tracker_timeout","message":"fixture-private-marker","hint":"fixture-private-hint","data":{"private":"fixture-private-data"}}}`))
	}))
	t.Cleanup(server.Close)
	c, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	_, err = c.TrackerOperationReceipt(t.Context(), 7, "01ARZ3NDEKTSV4RRFFQ69G5FAZ")
	require.Error(t, err)
	require.Equal(t, "tracker_timeout", kataclient.ErrorCode(err))
	require.Equal(t, 504, kataclient.StatusCode(err))
	require.NotContains(t, err.Error(), "fixture-private")
	var envelope generated.ErrorEnvelope
	require.ErrorAs(t, err, &envelope)
	require.Empty(t, envelope.ErrorData.Data)
	require.Nil(t, envelope.ErrorData.Hint)
}

// Public error projection is independent of arbitrary diagnostics/statuses.
// These contract classes form the oracle; it does not call the validator.
func TestTrackerClientPublicErrorProjectionProperty(t *testing.T) {
	classes := []string{"validation", "not_found", "tracker_denied", "tracker_unavailable", "tracker_outcome_unknown", "tracker_activation_required", "tracker_binding_changed", "tracker_operation_unsupported", "tracker_restored_inactive", "tracker_binding_disabled", "tracker_running", "tracker_project_already_bound", "tracker_timeout", "tracker_principal_unsupported", "tracker_capability_unavailable", "tracker_ambiguous", "tracker_selection_required", "tracker_operation_conflict", "tracker_operation_pending", "tracker_capacity"}
	known := map[string]bool{}
	for _, code := range classes {
		known[code] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		status, _ := strconv.Atoi(q.Get("status"))
		bodyStatus, _ := strconv.ParseInt(q.Get("body_status"), 10, 64)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.MarshalWrite(w, map[string]any{"status": bodyStatus, "error": map[string]any{"code": q.Get("code"), "message": q.Get("diagnostic"), "hint": q.Get("diagnostic"), "data": map[string]any{"private": q.Get("diagnostic")}}})
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			code = hegel.Draw(ht, hegel.SampledFrom(classes))
		}
		diagnostic := hegel.Draw(ht, hegel.Text())
		// Bound materialized HTTP fixtures, keeping the full drawn text domain.
		codeRunes := []rune(code)
		code = string(codeRunes[:min(len(codeRunes), 512)])
		diagnosticRunes := []rune(diagnostic)
		diagnostic = "fixture-private-" + string(diagnosticRunes[:min(len(diagnosticRunes), 512)])
		status := hegel.Draw(ht, hegel.Integers(201, 599))
		bodyStatus := hegel.Draw(ht, hegel.Integers[int32](math.MinInt32, math.MaxInt32))
		_, failure := client.TrackerOperationReceipt(t.Context(), 7, "01ARZ3NDEKTSV4RRFFQ69G5FAZ", func(_ context.Context, r *http.Request) error {
			r.URL.RawQuery = url.Values{"status": {strconv.Itoa(status)}, "body_status": {strconv.FormatInt(int64(bodyStatus), 10)}, "code": {code}, "diagnostic": {diagnostic}}.Encode()
			return nil
		})
		require.Error(ht, failure)
		expected := ""
		if known[code] && status != http.StatusNoContent && status != http.StatusNotModified {
			expected = code
		}
		require.Equal(ht, expected, kataclient.ErrorCode(failure))
		require.Equal(ht, status, kataclient.StatusCode(failure))
		require.NotContains(ht, failure.Error(), "fixture-private-")
		var envelope generated.ErrorEnvelope
		projected := errors.As(failure, &envelope)
		require.Equal(ht, expected != "", projected)
		if projected {
			require.Equal(ht, int64(status), envelope.Status)
			require.Empty(ht, envelope.ErrorData.Data)
			require.Nil(ht, envelope.ErrorData.Hint)
			require.NotContains(ht, envelope.ErrorData.Message, "fixture-private-")
		}
	}, hegel.WithTestCases(100))
}
