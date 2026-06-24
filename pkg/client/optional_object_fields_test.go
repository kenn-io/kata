package client

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

// A changes block whose edit touched no parent carries no parent_set or
// parent_removed peer; the generated client must validate and round-trip it
// without inventing empty peer objects.
func TestLinkChangesWithoutParentPeersValidatesAndOmitsParentPeers(t *testing.T) {
	changes := generated.LinkChanges{}
	require.NoError(t, changes.Validate(),
		"a changes block without parent peers must validate")

	out, err := json.Marshal(changes)
	require.NoError(t, err)
	var roundTrip map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &roundTrip))
	require.NotContains(t, roundTrip, "parent_set",
		"absent parent_set must not marshal as an empty object")
	require.NotContains(t, roundTrip, "parent_removed",
		"absent parent_removed must not marshal as an empty object")
}

// A parentless issue response carries no parent peer. The generated client
// must keep IssueOut.Parent a pointer so a parentless IssueOut validates and
// round-trips without inventing an empty parent object — the regression that
// surfaces when an optional object response field regenerates as a value
// type (additionalProperties leaking onto the client-flavor schema).
func TestIssueOutWithoutParentValidatesAndOmitsParent(t *testing.T) {
	issue := generated.IssueOut{
		UID:         "01TESTISSUEAAAAAAAAAAAAAAA",
		ShortID:     "abc4",
		QualifiedID: "spoke-project#abc4",
		Title:       "parentless issue",
		Body:        "no parent here",
		Status:      "open",
		Author:      "tester",
		CreatedAt:   time.Unix(1, 0).UTC(),
		UpdatedAt:   time.Unix(1, 0).UTC(),
	}
	require.NoError(t, issue.Validate(),
		"a parentless issue must validate without a parent peer")

	out, err := json.Marshal(issue)
	require.NoError(t, err)
	var roundTrip map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &roundTrip))
	require.NotContains(t, roundTrip, "parent",
		"absent parent must not marshal as an empty object")
}

// A project with no issue sync binding returns status only. The generated
// client must keep IssueSyncBody.Binding optional so callers can represent
// the not_enabled state without inventing an empty binding object.
func TestIssueSyncBodyWithoutBindingValidatesAndOmitsBinding(t *testing.T) {
	body := generated.IssueSyncBody{
		Status: generated.IssueSyncStatusOut{
			ProjectID: 42,
			Provider:  "github",
			State:     "not_enabled",
		},
	}
	require.NoError(t, body.Validate(),
		"a not-enabled issue sync status must validate without a binding")

	out, err := json.Marshal(body)
	require.NoError(t, err)
	var roundTrip map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &roundTrip))
	require.NotContains(t, roundTrip, "binding",
		"absent issue sync binding must not marshal as an empty object")
}

// The run-once endpoint accepts an empty request object and returns a sync
// result object. The generated request options must point at the request-body
// schema so callers can naturally send {} without constructing a response
// body containing binding/status/import fields.
func TestRunIssueSyncOnceRequestOptionsUsesRequestBody(t *testing.T) {
	body := generated.RunIssueSyncOnceRequestBody{}
	opts := generated.RunIssueSyncOnceRequestOptions{
		PathParams: &generated.RunIssueSyncOncePath{ProjectID: 42, Provider: "github"},
		Body:       &body,
	}
	require.NoError(t, opts.Validate())
	require.Same(t, &body, opts.GetBody())
}

func TestIssueSyncConfigSupportsOpaqueProviderValues(t *testing.T) {
	request := generated.EnableIssueSyncRequestBody{
		Config: map[string]any{
			"host":    "github.com",
			"owner":   "example-org",
			"repo":    "example-repo",
			"repo_id": float64(12345),
		},
	}
	opts := generated.EnableIssueSyncRequestOptions{
		PathParams: &generated.EnableIssueSyncPath{ProjectID: 42, Provider: "github"},
		Body:       &request,
	}
	require.NoError(t, opts.Validate())

	responseJSON := []byte(`{
		"config":{"host":"github.com","owner":"example-org","repo":"example-repo","repo_id":12345},
		"created_at":"2026-06-23T00:00:00Z",
		"display_name":"example-org/example-repo",
		"enabled":true,
		"id":1,
		"interval_seconds":300,
		"project_id":42,
		"provider":"github",
		"remote_id":"R_exampleNode",
		"source_key":"github:R_exampleNode",
		"updated_at":"2026-06-23T00:00:00Z"
	}`)
	var binding generated.IssueSyncBindingOut
	require.NoError(t, json.Unmarshal(responseJSON, &binding))
	require.Equal(t, "example-org", binding.Config["owner"])
	require.Equal(t, float64(12345), binding.Config["repo_id"])
}
