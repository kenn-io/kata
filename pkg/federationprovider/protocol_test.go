package federationprovider_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/federationprovider"
)

const requestID = "8b60f249-b495-4f17-8999-c64382e05680"

// These wire fixtures are independent of the package's encoder.
const readyJSON = `{"version":1,"operation":"authorize","request_id":"` + requestID + `","status":"ready","hub_url":"https://hub.example/tools/tasks","project_id":7,"project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","enrollment_id":9,"actor":"Example Operator","capabilities":"claim,pull,push","expires_at":"2030-01-01T00:00:00Z"}`

func authorizationRequest() federationprovider.Request {
	return federationprovider.Request{
		Version: 1, Operation: "authorize", RequestID: uuid.MustParse(requestID),
		HubURL: "https://hub.example/tools/tasks", Project: "hub-project",
		SpokeInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		LocalProjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAW", Intent: "collaborate",
		CandidateToken: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)),
	}
}

func authorizationJSON() string {
	return fmt.Sprintf(`{"version":1,"operation":"authorize","request_id":"%s","hub_url":"https://hub.example/tools/tasks","project":"hub-project","spoke_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","local_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","intent":"collaborate","candidate_token":"%s"}`, requestID, authorizationRequest().CandidateToken)
}

func TestDecodeRequest(t *testing.T) {
	request, err := federationprovider.DecodeRequest(strings.NewReader(authorizationJSON()))
	require.NoError(t, err)
	require.Equal(t, authorizationRequest(), request)

	release := `{"version":1,"operation":"release","request_id":"` + requestID + `"}`
	request, err = federationprovider.DecodeRequest(strings.NewReader(release))
	require.NoError(t, err)
	require.Equal(t, federationprovider.Request{Version: 1, Operation: "release", RequestID: uuid.MustParse(requestID)}, request)

	invalid := map[string]string{
		"unknown version":    strings.Replace(authorizationJSON(), `"version":1`, `"version":2`, 1),
		"unknown operation":  strings.Replace(authorizationJSON(), `"authorize"`, `"rotate"`, 1),
		"wrong case":         strings.Replace(authorizationJSON(), `"version"`, `"Version"`, 1),
		"duplicate":          strings.Replace(authorizationJSON(), `"version":1`, `"version":1,"version":1`, 1),
		"unknown permission": strings.Replace(authorizationJSON(), `"collaborate"`, `"admin"`, 1),
		"unknown field":      strings.Replace(authorizationJSON(), `"version":1`, `"version":1,"extra":true`, 1),
		"missing token":      strings.Replace(authorizationJSON(), `,"candidate_token":"`+authorizationRequest().CandidateToken+`"`, "", 1),
		"padded token":       strings.Replace(authorizationJSON(), authorizationRequest().CandidateToken, authorizationRequest().CandidateToken+"=", 1),
		"short token":        strings.Replace(authorizationJSON(), authorizationRequest().CandidateToken, "short", 1),
		"uppercase UUID":     strings.ReplaceAll(authorizationJSON(), requestID, strings.ToUpper(requestID)),
		"compact UUID":       strings.ReplaceAll(authorizationJSON(), requestID, strings.ReplaceAll(requestID, "-", "")),
		"nil UUID":           strings.ReplaceAll(authorizationJSON(), requestID, "00000000-0000-0000-0000-000000000000"),
		"lowercase ULID":     strings.ReplaceAll(authorizationJSON(), "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01arz3ndektsv4rrffq69g5faw"),
		"bad ULID":           strings.ReplaceAll(authorizationJSON(), "01ARZ3NDEKTSV4RRFFQ69G5FAW", "not-a-project-uid"),
		"null field":         strings.Replace(authorizationJSON(), `"project":"hub-project"`, `"project":null`, 1),
		"empty project":      strings.Replace(authorizationJSON(), `"project":"hub-project"`, `"project":" "`, 1),
		"plaintext target":   strings.Replace(authorizationJSON(), "https://", "http://", 1),
		"target userinfo":    strings.Replace(authorizationJSON(), "hub.example", "user@hub.example", 1),
		"target query":       strings.Replace(authorizationJSON(), "/tools/tasks", "/tools/tasks?token=x", 1),
		"invalid UTF8":       strings.Replace(authorizationJSON(), "hub-project", "\xff", 1),
		"extra document":     authorizationJSON() + "{}",
		"oversize":           authorizationJSON() + strings.Repeat(" ", 16*1024),
		"release with token": strings.TrimSuffix(release, "}") + `,"candidate_token":""}`,
		"release null field": strings.TrimSuffix(release, "}") + `,"hub_url":null}`,
	}
	for name, document := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := federationprovider.DecodeRequest(strings.NewReader(document))
			require.ErrorIs(t, err, federationprovider.ErrInvalidRequest)
			if strings.Contains(err.Error(), authorizationRequest().CandidateToken) {
				t.Fatal("request error contains credential")
			}
		})
	}
}

func TestWriteResponseChecksRequestBeforeWriting(t *testing.T) {
	request := authorizationRequest()
	response := federationprovider.Response{
		Version: 1, Operation: "authorize", RequestID: request.RequestID, Status: "ready",
		HubURL: request.HubURL, ProjectID: 7, ProjectUID: request.SpokeInstanceUID,
		EnrollmentID: 9, Actor: "Example Operator", Capabilities: "claim,pull,push",
		ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	var out bytes.Buffer
	require.NoError(t, federationprovider.WriteResponse(&out, request, response))
	require.JSONEq(t, readyJSON, out.String())

	response.HubURL = "https://elsewhere.example/tools/tasks"
	out.Reset()
	require.ErrorIs(t, federationprovider.WriteResponse(&out, request, response), federationprovider.ErrInvalidResponse)
	require.Empty(t, out.String(), "an invalid response must not be partially written")

	response = federationprovider.Response{Version: 1, Operation: "authorize", RequestID: request.RequestID, Status: "approval_required", Message: "Ask a project manager to approve this request."}
	require.NoError(t, federationprovider.WriteResponse(&out, request, response))
	require.JSONEq(t, `{"version":1,"operation":"authorize","request_id":"`+requestID+`","status":"approval_required","message":"Ask a project manager to approve this request."}`, out.String())

	response.Status = federationprovider.StatusReleased
	out.Reset()
	require.ErrorIs(t, federationprovider.WriteResponse(&out, request, response), federationprovider.ErrInvalidResponse)
	require.Empty(t, out.String(), "release cannot answer an authorization request")
}

func TestDecodeResponseChecksRawContractAndTarget(t *testing.T) {
	request := authorizationRequest()
	response, err := federationprovider.DecodeResponse(strings.NewReader(readyJSON), request)
	require.NoError(t, err)
	require.Equal(t, int64(9), response.EnrollmentID)
	require.Equal(t, "claim,pull,push", response.Capabilities)
	for name, raw := range map[string]string{
		"changed target":         strings.Replace(readyJSON, "hub.example", "other.example", 1),
		"null extra field":       strings.Replace(readyJSON, `"status":"ready"`, `"status":"ready","message":null`, 1),
		"extra document":         readyJSON + "{}",
		"oversize":               readyJSON + strings.Repeat(" ", 16*1024),
		"ready fields on denial": strings.Replace(readyJSON, `"status":"ready"`, `"status":"denied"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := federationprovider.DecodeResponse(strings.NewReader(raw), request)
			require.ErrorIs(t, err, federationprovider.ErrInvalidResponse)
		})
	}
}

func TestReadyGrantKeepsReadOnlyAndAllowsOptionalClaimAndExpiry(t *testing.T) {
	for _, intent := range []string{"read_only", "collaborate", "migrate"} {
		for _, capabilities := range []string{"pull", "pull,push", "claim,pull,push", "claim,pull", "push", "push,pull", "pull,pull,push"} {
			t.Run(intent+"/"+capabilities, func(t *testing.T) {
				request := authorizationRequest()
				request.Intent = federationprovider.Intent(intent)
				raw := strings.Replace(readyJSON, "claim,pull,push", capabilities, 1)
				raw = strings.Replace(raw, `,"expires_at":"2030-01-01T00:00:00Z"`, "", 1)
				response, err := federationprovider.DecodeResponse(strings.NewReader(raw), request)
				allowed := intent == "read_only" && capabilities == "pull" || intent != "read_only" && (capabilities == "pull,push" || capabilities == "claim,pull,push")
				if !allowed {
					require.ErrorIs(t, err, federationprovider.ErrInvalidResponse)
					return
				}
				require.NoError(t, err)
				require.True(t, response.ExpiresAt.IsZero())
				var out bytes.Buffer
				require.NoError(t, federationprovider.WriteResponse(&out, request, response))
				require.JSONEq(t, raw, out.String())
			})
		}
	}
	for _, actor := range []string{"", " ", "bootstrap", " BoOtStRaP "} {
		raw := strings.Replace(readyJSON, "Example Operator", actor, 1)
		_, err := federationprovider.DecodeResponse(strings.NewReader(raw), authorizationRequest())
		require.ErrorIs(t, err, federationprovider.ErrInvalidResponse)
	}
}

func TestReleaseAcceptsOriginalTargetContextWithoutToken(t *testing.T) {
	raw := strings.Replace(authorizationJSON(), `"authorize"`, `"release"`, 1)
	raw = strings.Replace(raw, `,"candidate_token":"`+authorizationRequest().CandidateToken+`"`, "", 1)
	request, err := federationprovider.DecodeRequest(strings.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, "hub-project", request.Project)
	require.Equal(t, "https://hub.example/tools/tasks", request.HubURL)
	require.Empty(t, request.CandidateToken)
	for _, invalid := range []string{
		strings.Replace(raw, "https://", "http://", 1),
		strings.Replace(raw, "hub-project", "", 1),
		strings.Replace(raw, `"collaborate"`, `"admin"`, 1),
		strings.TrimSuffix(raw, "}") + `,"candidate_token":""}`,
	} {
		_, err := federationprovider.DecodeRequest(strings.NewReader(invalid))
		require.ErrorIs(t, err, federationprovider.ErrInvalidRequest)
	}
}
