package main

import (
	"context"
	"fmt"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/tracker"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrackerOperationFlagsRejectIncompleteSelectionBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"sync", "tracker", "operation", "run", "--uid", "01ARZ3NDEKTSV4RRFFQ69G5FAZ", "--provider", "notion", "--resource", "notion:source", "--connection", "01ARZ3NDEKTSV4RRFFQ69G5FAY"},
		{"sync", "tracker", "operation", "run", "--provider", "notion", "--resource", "notion:source"},
		{"sync", "tracker", "operation", "run", "--uid", "bad", "--provider", "notion", "--resource", "notion:source"},
	} {
		command := newRootCmd()
		command.SetArgs(args)
		err := command.Execute()
		require.Error(t, err)
	}
}

// Submitting an operation classifies a failed effect; reading its receipt is
// data retrieval and can still print the retained failed state successfully.
func TestTrackerOperationFailuresUseExitTaxonomy(t *testing.T) {
	for _, c := range []struct {
		code string
		exit int
	}{
		{"validation", ExitValidation}, {"tracker_denied", ExitConflict}, {"tracker_binding_changed", ExitConflict}, {"tracker_unavailable", ExitDaemonUnavail}, {"tracker_outcome_unknown", ExitConflict}, {"tracker_timeout", ExitDaemonUnavail}, {"not_found", ExitNotFound},
	} {
		err := trackerOperationOutcomeError(&kataclient.TrackerReceipt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", State: tracker.ControlFailed, Result: &kataclient.TrackerOperationResult{Code: c.code}})
		var failure *cliError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, c.exit, failure.ExitCode)
		require.Equal(t, c.code, failure.Code)
	}
	require.NoError(t, trackerOperationOutcomeError(&kataclient.TrackerReceipt{State: tracker.ControlPending}))
}

// Exercise the actual SDK error wrapper from non-200 HTTP responses, rather
// than only retained failed receipts already decoded as successful data.
func TestTrackerCLIClassifiesHTTPEnvelopeFailures(t *testing.T) {
	for _, c := range []struct {
		code         string
		status, exit int
	}{{"tracker_timeout", 504, ExitDaemonUnavail}, {"not_found", 404, ExitNotFound}, {"tracker_outcome_unknown", 409, ExitConflict}, {"fixture_untrusted", 500, ExitDaemonUnavail}} {
		t.Run(c.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"error":{"code":%q,"message":"fixture private diagnostic"}}`, c.status, c.code)
			}))
			t.Cleanup(server.Close)
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			_, err = client.TrackerOperation(t.Context(), 7, kataclient.TrackerOperation{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: "run", Provider: "notion", Resource: "notion:source"})
			err = trackerCLIRequestError(err)
			var failure *cliError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, c.exit, failure.ExitCode)
			wantCode := c.code
			if c.code == "fixture_untrusted" {
				wantCode = "tracker_unavailable"
			}
			require.Equal(t, wantCode, failure.Code)
			require.NotContains(t, failure.Message, "fixture private diagnostic")
		})
	}
	require.Nil(t, trackerCLIRequestError(nil))
	var failure *cliError
	require.ErrorAs(t, trackerCLIRequestError(context.DeadlineExceeded), &failure)
	require.Equal(t, "tracker_timeout", failure.Code)
}

func TestTrackerFailedReceiptCannotRenderUnclassifiedDiagnostic(t *testing.T) {
	err := trackerOperationOutcomeError(&kataclient.TrackerReceipt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", State: tracker.ControlFailed, Result: &kataclient.TrackerOperationResult{Code: "fixture-private-marker"}})
	var failure *cliError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "tracker_unavailable", failure.Code)
	require.NotContains(t, failure.Message, "fixture-private-marker")
}

// The same public failure has the same CLI meaning whether it arrives as an
// HTTP error or a retained failed receipt. Diagnostic text is never a class.
func TestTrackerCLIHTTPAndRetainedErrorMeaningProperty(t *testing.T) {
	cases := []struct {
		code string
		exit int
	}{{"validation", ExitValidation}, {"not_found", ExitNotFound}, {"tracker_timeout", ExitDaemonUnavail}, {"tracker_unavailable", ExitDaemonUnavail}, {"tracker_outcome_unknown", ExitConflict}, {"tracker_denied", ExitConflict}, {"tracker_binding_changed", ExitConflict}, {"tracker_activation_required", ExitConflict}, {"tracker_binding_disabled", ExitConflict}, {"tracker_running", ExitConflict}, {"tracker_project_already_bound", ExitConflict}, {"tracker_restored_inactive", ExitConflict}, {"tracker_operation_unsupported", ExitValidation}, {"fixture-private-code", ExitDaemonUnavail}}
	hegel.Test(t, func(ht *hegel.T) {
		c := hegel.Draw(ht, hegel.SampledFrom(cases))
		status := hegel.Draw(ht, hegel.Integers(400, 599))
		diagnostic := "fixture-private-" + hegel.Draw(ht, hegel.Text())
		apiErr := runtime.NewClientAPIError(generated.ErrorEnvelope{Status: int64(status), ErrorData: generated.ErrorBody{Code: c.code, Message: diagnostic}}, runtime.WithStatusCode(status))
		if hegel.Draw(ht, hegel.Booleans()) {
			apiErr = fmt.Errorf("%s: %w", diagnostic, apiErr)
		}
		retained := trackerOperationOutcomeError(&kataclient.TrackerReceipt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", State: tracker.ControlFailed, Result: &kataclient.TrackerOperationResult{Code: c.code}})
		var httpFailure, receiptFailure *cliError
		require.ErrorAs(ht, trackerCLIRequestError(apiErr), &httpFailure)
		require.ErrorAs(ht, retained, &receiptFailure)
		require.Equal(ht, c.exit, httpFailure.ExitCode)
		require.Equal(ht, c.exit, receiptFailure.ExitCode)
		require.Equal(ht, receiptFailure.Kind, httpFailure.Kind)
		require.Equal(ht, receiptFailure.Code, httpFailure.Code)
		require.NotContains(ht, httpFailure.Message, "fixture-private-")
	}, hegel.WithTestCases(100))
}
