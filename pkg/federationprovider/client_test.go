package federationprovider_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/federationprovider"
)

func providerCommand(t *testing.T, mode string) []string {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("KATA_PROVIDER_PROCESS", "1")
	t.Setenv("KATA_PROVIDER_EXAMPLE", "inherited-value")
	return []string{executable, "-test.run=^TestProviderProcess$", "--", mode, "literal argument $(not-a-command)"}
}

func TestExchangeDeliversSavedRequest(t *testing.T) {
	command := providerCommand(t, "ready")
	for range 2 {
		response, err := federationprovider.Exchange(t.Context(), command, authorizationRequest())
		require.NoError(t, err)
		require.Equal(t, federationprovider.StatusReady, response.Status)
		require.Equal(t, int64(7), response.ProjectID)
		require.Equal(t, int64(9), response.EnrollmentID)
		require.Equal(t, "claim,pull,push", response.Capabilities)
		require.Equal(t, "2030-01-01T00:00:00Z", response.ExpiresAt.Format(time.RFC3339))
	}
}

func TestExchangeDomainResults(t *testing.T) {
	for _, operation := range []string{"authorize", "release"} {
		for _, status := range []string{"approval_required", "sign_in_required", "denied", "conflict", "unavailable", "released"} {
			t.Run(operation+"/"+status, func(t *testing.T) {
				request := authorizationRequest()
				if operation == "release" {
					request = federationprovider.Request{Version: 1, Operation: "release", RequestID: request.RequestID}
				}
				response, err := federationprovider.Exchange(t.Context(), providerCommand(t, status), request)
				if operation == "release" && (status == "approval_required" || status == "sign_in_required") ||
					operation == "authorize" && status == "released" {
					require.ErrorIs(t, err, federationprovider.ErrInvalidResponse)
					require.Zero(t, response)
					return
				}
				require.NoError(t, err)
				require.Equal(t, federationprovider.Status(status), response.Status)
			})
		}
	}
}

func TestExchangeRejectsUnusableResults(t *testing.T) {
	invalid := map[string]string{
		"different hub":               strings.Replace(readyJSON, "hub.example", "other.example", 1),
		"different mount":             strings.Replace(readyJSON, "/tools/tasks", "/tools/other", 1),
		"different request":           strings.Replace(readyJSON, requestID, "8b60f249-b495-4f17-8999-c64382e05681", 1),
		"different operation":         strings.Replace(readyJSON, `"authorize"`, `"release"`, 1),
		"unknown status":              strings.Replace(readyJSON, `"ready"`, `"allowed"`, 1),
		"display capability":          strings.Replace(readyJSON, "claim,pull,push", "lease,pull,push", 1),
		"reordered capabilities":      strings.Replace(readyJSON, "claim,pull,push", "pull,push,claim", 1),
		"downgraded permission":       strings.Replace(readyJSON, "claim,pull,push", "pull", 1),
		"null expiry":                 strings.Replace(readyJSON, `"2030-01-01T00:00:00Z"`, "null", 1),
		"non UTC expiry":              strings.Replace(readyJSON, "2030-01-01T00:00:00Z", "2030-01-01T01:00:00+01:00", 1),
		"missing actor":               strings.Replace(readyJSON, `,"actor":"Example Operator"`, "", 1),
		"zero enrollment":             strings.Replace(readyJSON, `"enrollment_id":9`, `"enrollment_id":0`, 1),
		"zero project":                strings.Replace(readyJSON, `"project_id":7`, `"project_id":0`, 1),
		"extra field":                 strings.Replace(readyJSON, `"version":1`, `"version":1,"candidate_token":"unexpected"`, 1),
		"duplicate field":             strings.Replace(readyJSON, `"version":1`, `"version":1,"version":1`, 1),
		"ready fields on denial":      strings.Replace(readyJSON, `"ready"`, `"denied"`, 1),
		"empty ready field on denial": `{"version":1,"operation":"authorize","request_id":"` + requestID + `","status":"denied","actor":""}`,
		"truncated":                   readyJSON[:len(readyJSON)-1],
		"extra document":              readyJSON + "{}",
		"extra prose":                 "starting provider\n" + readyJSON,
		"empty":                       "",
		"oversize":                    readyJSON + strings.Repeat(" ", 16*1024),
	}
	for name, result := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KATA_PROVIDER_RESPONSE", result)
			response, err := federationprovider.Exchange(t.Context(), providerCommand(t, "response"), authorizationRequest())
			require.ErrorIs(t, err, federationprovider.ErrInvalidResponse)
			require.Zero(t, response)
		})
	}
}

func TestExchangeAcceptsReadOnlyAndCanonicalTarget(t *testing.T) {
	request := authorizationRequest()
	request.Intent = "read_only"
	t.Setenv("KATA_PROVIDER_RESPONSE", strings.ReplaceAll(strings.ReplaceAll(readyJSON, "claim,pull,push", "pull"), "https://hub.example/tools/tasks", "https://HUB.example:443/tools/tasks/"))
	response, err := federationprovider.Exchange(t.Context(), providerCommand(t, "response"), request)
	require.NoError(t, err)
	require.Equal(t, "pull", response.Capabilities)
	require.Equal(t, request.HubURL, response.HubURL)
}

func TestExchangeNonzeroExitDiscardsEvenCompleteResult(t *testing.T) {
	for _, mode := range []string{"exit-one", "exit-two"} {
		t.Run(mode, func(t *testing.T) {
			response, err := federationprovider.Exchange(t.Context(), providerCommand(t, mode), authorizationRequest())
			if mode == "exit-two" {
				require.ErrorIs(t, err, federationprovider.ErrInvalidRequest)
			} else {
				require.ErrorIs(t, err, federationprovider.ErrProviderFailed)
			}
			require.Zero(t, response)
			if strings.Contains(err.Error(), authorizationRequest().CandidateToken) {
				t.Fatal("provider error contains credential from child diagnostics")
			}
		})
	}
}

func TestExchangeRejectsBeforeStartingProvider(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("KATA_PROVIDER_MARKER", marker)
	request := authorizationRequest()
	request.Intent = "admin"
	_, err := federationprovider.Exchange(t.Context(), providerCommand(t, "marker"), request)
	require.ErrorIs(t, err, federationprovider.ErrInvalidRequest)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = federationprovider.Exchange(ctx, providerCommand(t, "marker"), authorizationRequest())
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestExchangeCancelsRunningProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := federationprovider.Exchange(ctx, providerCommand(t, "wait"), authorizationRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestExchangeStopsProviderAtOutputLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := federationprovider.Exchange(ctx, providerCommand(t, "overflow-wait"), authorizationRequest())
	require.ErrorIs(t, err, federationprovider.ErrInvalidResponse)
}

// TestProviderProcess is a real executable peer. It checks request literals
// without using the production codec, so a shared codec bug cannot pass both ends.
func TestProviderProcess(_ *testing.T) {
	if os.Getenv("KATA_PROVIDER_PROCESS") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) != separator+3 || os.Args[separator+2] != "literal argument $(not-a-command)" || os.Getenv("KATA_PROVIDER_EXAMPLE") != "inherited-value" {
		os.Exit(10)
	}
	mode := os.Args[separator+1]
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(11)
	}
	var request map[string]any
	if json.Unmarshal(data, &request) != nil || request["version"] != float64(1) || request["request_id"] != requestID {
		os.Exit(12)
	}
	if mode == "ready" {
		if len(request) != 9 || request["operation"] != "authorize" || request["hub_url"] != "https://hub.example/tools/tasks" || request["project"] != "hub-project" || request["candidate_token"] != authorizationRequest().CandidateToken || request["intent"] != "collaborate" || request["spoke_instance_uid"] != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || request["local_project_uid"] != "01ARZ3NDEKTSV4RRFFQ69G5FAW" {
			os.Exit(13)
		}
	}
	if request["operation"] == "release" && len(request) != 3 {
		os.Exit(14)
	}
	var result string
	switch mode {
	case "ready", "exit-one", "exit-two":
		result = readyJSON
	case "response":
		result = os.Getenv("KATA_PROVIDER_RESPONSE")
	case "overflow-wait":
		if _, err := io.WriteString(os.Stdout, strings.Repeat(" ", 32*1024)); err != nil {
			os.Exit(19)
		}
		time.Sleep(time.Hour)
		os.Exit(20)
	case "wait":
		time.Sleep(time.Hour)
		os.Exit(15)
	case "marker":
		// #nosec G703 -- parent test supplies a marker inside its own t.TempDir.
		if err := os.WriteFile(os.Getenv("KATA_PROVIDER_MARKER"), []byte("invoked"), 0600); err != nil {
			os.Exit(16)
		}
	default:
		result = fmt.Sprintf(`{"version":1,"operation":%q,"request_id":"%s","status":%q}`, request["operation"], requestID, mode)
	}
	if _, err := io.WriteString(os.Stdout, result); err != nil {
		os.Exit(17)
	}
	if mode == "exit-one" || mode == "exit-two" {
		if _, err := fmt.Fprint(os.Stderr, request["candidate_token"]); err != nil {
			os.Exit(18)
		}
		if mode == "exit-two" {
			os.Exit(2)
		}
		os.Exit(1)
	}
	os.Exit(0)
}
