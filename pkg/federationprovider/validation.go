package federationprovider

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"regexp"
	"strings"
	"uuid"

	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/tokenactor"
	"go.kenn.io/kata/internal/uid"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var authorizationFields = []string{"hub_url", "project", "spoke_instance_uid", "local_project_uid", "intent", "candidate_token"}
var readyFields = []string{"hub_url", "project_id", "project_uid", "enrollment_id", "actor", "capabilities"}

// The typed decoder rejects unknown and duplicate fields. The raw field map
// also distinguishes absent fields from null/zero fields forbidden in a status.
func decodeDocument(data []byte, destination any) (map[string]jsontext.Value, bool) {
	if len(data) > MaxDocumentBytes || json.Unmarshal(data, destination, json.RejectUnknownMembers(true)) != nil {
		return nil, false
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, false
	}
	for _, value := range fields {
		if bytes.Equal(value, []byte("null")) {
			return nil, false
		}
	}
	var requestID string
	if json.Unmarshal(fields["request_id"], &requestID) != nil || !canonicalUUID.MatchString(requestID) {
		return nil, false
	}
	return fields, true
}

func fieldsPresent(fields map[string]jsontext.Value, names []string, want bool) bool {
	for _, name := range names {
		_, exists := fields[name]
		if exists != want {
			return false
		}
	}
	return true
}

func decodeRequest(data []byte) (Request, error) {
	var request Request
	fields, ok := decodeDocument(data, &request)
	if !ok || !validRequest(request) {
		return Request{}, ErrInvalidRequest
	}
	if request.Operation == "authorize" && !fieldsPresent(fields, authorizationFields, true) {
		return Request{}, ErrInvalidRequest
	}
	if request.Operation == "release" {
		if _, present := fields["candidate_token"]; present {
			return Request{}, ErrInvalidRequest
		}
		for _, name := range authorizationFields[:len(authorizationFields)-1] {
			if value, present := fields[name]; present && bytes.Equal(value, []byte(`""`)) {
				return Request{}, ErrInvalidRequest
			}
		}
	}
	return request, nil
}

func validRequest(request Request) bool {
	if request.Version != 1 || request.RequestID == (uuid.UUID{}) {
		return false
	}
	if request.Operation == "release" {
		_, validBase := httpsBase(request.HubURL)
		return request.CandidateToken == "" &&
			(request.HubURL == "" || validBase) &&
			(request.Project == "" || strings.TrimSpace(request.Project) != "") &&
			(request.SpokeInstanceUID == "" || validUID(request.SpokeInstanceUID)) &&
			(request.LocalProjectUID == "" || validUID(request.LocalProjectUID)) &&
			(request.Intent == "" || validIntent(request.Intent))
	}
	if request.Operation != "authorize" || strings.TrimSpace(request.Project) == "" || !validUID(request.SpokeInstanceUID) || !validUID(request.LocalProjectUID) {
		return false
	}
	if _, ok := httpsBase(request.HubURL); !ok || !validIntent(request.Intent) {
		return false
	}
	token, err := base64.RawURLEncoding.Strict().DecodeString(request.CandidateToken)
	return err == nil && len(token) == 32 && base64.RawURLEncoding.EncodeToString(token) == request.CandidateToken
}

func decodeResponse(data []byte, request Request) (Response, error) {
	var response Response
	fields, ok := decodeDocument(data, &response)
	if !ok || response.Version != 1 || response.Operation != request.Operation || response.RequestID != request.RequestID {
		return Response{}, ErrInvalidResponse
	}
	if !fieldsPresent(fields, readyFields, response.Status == StatusReady) {
		return Response{}, ErrInvalidResponse
	}
	if expiry, present := fields["expires_at"]; present {
		var value string
		if response.Status != StatusReady || response.ExpiresAt.IsZero() || json.Unmarshal(expiry, &value) != nil || !strings.HasSuffix(value, "Z") {
			return Response{}, ErrInvalidResponse
		}
	}
	switch response.Status {
	case StatusReleased:
		if request.Operation == "release" {
			return response, nil
		}
	case StatusConflict, StatusDenied, StatusUnavailable:
		return response, nil
	case StatusApprovalRequired, StatusSignInRequired:
		if request.Operation == "authorize" {
			return response, nil
		}
	case StatusReady:
		base, validBase := httpsBase(response.HubURL)
		expectedBase, _ := httpsBase(request.HubURL)
		if request.Operation != "authorize" || !validBase || base != expectedBase || response.ProjectID <= 0 || response.EnrollmentID <= 0 || !validUID(response.ProjectUID) || tokenactor.Validate(response.Actor) != nil || !validCapabilities(request.Intent, response.Capabilities) {
			return Response{}, ErrInvalidResponse
		}
		response.HubURL = base
		return response, nil
	}
	return Response{}, ErrInvalidResponse
}

func validIntent(intent Intent) bool {
	switch intent {
	case IntentReadOnly, IntentCollaborate, IntentMigrate:
		return true
	default:
		return false
	}
}

func validCapabilities(intent Intent, capabilities string) bool {
	if intent == IntentReadOnly {
		return capabilities == "pull"
	}
	return capabilities == "pull,push" || capabilities == "claim,pull,push"
}

func validUID(value string) bool {
	return uid.Valid(value) && value == strings.ToUpper(value)
}

func httpsBase(value string) (string, bool) {
	base, err := httpurl.CanonicalHTTPBaseURL(value)
	return base, err == nil && strings.HasPrefix(base, "https://")
}
