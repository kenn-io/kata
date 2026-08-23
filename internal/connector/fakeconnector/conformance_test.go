package fakeconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/kata/pkg/connector"
	connectorconformance "go.kenn.io/kata/pkg/connector/conformance"
)

func TestPublicConformance(t *testing.T) {
	fixture := &conformanceFixture{path: filepath.Join(t.TempDir(), "state.json")}
	connectorconformance.Run(t, fixture)
}

type conformanceFixture struct{ path string }

func (f *conformanceFixture) RootLocator() string { return "fixture-root" }

func (f *conformanceFixture) Invocation() connector.Invocation {
	return connector.Invocation{Instance: "example-instance", Settings: json.RawMessage(`{}`)}
}

func (f *conformanceFixture) Exchange(_ context.Context, request []byte) ([]byte, error) {
	var response bytes.Buffer
	if code := Run(f.path, bytes.NewReader(request), &response); code != 0 {
		return response.Bytes(), fmt.Errorf("fake connector exited with status %d", code)
	}
	return response.Bytes(), nil
}

func (f *conformanceFixture) InjectFault(_ context.Context, fault connectorconformance.Fault) error {
	if fault != connectorconformance.FaultPublishCommentCrashAfterMutation {
		return fmt.Errorf("unsupported fault %q", fault)
	}
	return Update(f.path, func(current *State) error {
		if current.Behavior.CrashAfterMutation == nil {
			current.Behavior.CrashAfterMutation = make(map[string]int)
		}
		current.Behavior.CrashAfterMutation["publish_comment"]++
		return nil
	})
}

func (f *conformanceFixture) MutateComment(_ context.Context, commentID string) error {
	return Update(f.path, func(current *State) error {
		for rootIndex := range current.Roots {
			for commentIndex := range current.Roots[rootIndex].Comments {
				comment := &current.Roots[rootIndex].Comments[commentIndex]
				if comment.ID != commentID {
					continue
				}
				comment.Body = "Externally edited comment"
				comment.UpdatedAt = comment.UpdatedAt.Add(time.Minute)
				comment.Revision = "comment-revision-after-edit"
				return nil
			}
		}
		return fmt.Errorf("provider state lacks comment %q", commentID)
	})
}

func (f *conformanceFixture) Reset(context.Context) error {
	observed := time.Now().UTC()
	updated := observed.Add(-10 * time.Minute)
	return Write(f.path, State{
		Description: connector.Description{
			ConnectorID: "example.connector", DisplayName: "Example Connector", Protocol: connector.ProtocolVersion,
			Capabilities: []connector.Capability{connector.CapabilityFields, connector.CapabilityPublishComment},
			ConfigSchema: []byte(`{"type":"object","additionalProperties":false}`), SelfActorID: "actor-self",
			AccountIdentity: "account-example",
		},
		Roots: []StoredRoot{{
			Locator: "fixture-root",
			Root: connector.Root{
				Key: "root-example", IdentityKey: "account-example", Title: "Example root", Body: "Example body",
				State: "open", Revision: "revision-1", UpdatedAt: updated, ObservedAt: observed,
				Fields: map[string]connector.FieldValue{
					"field-date":     {Kind: "date", Value: "2026-08-20"},
					"field-local":    {Kind: "local_datetime", Value: "2026-08-20T11:30:00", Timezone: "Europe/Paris"},
					"field-instant":  {Kind: "instant", Value: "2026-08-20T09:30:00Z"},
					"field-null":     {Kind: "null"},
					"field-readonly": {Kind: "date", Value: "2026-08-20"},
				},
			},
			Comments: []connector.Comment{
				{ID: "comment-before", Revision: "comment-revision-1", Body: "Existing comment", Author: connector.Actor{ID: "actor-history", DisplayName: "Historical Reviewer"}, CreatedAt: updated.Add(-time.Minute), UpdatedAt: updated.Add(-time.Minute)},
				{ID: "comment-active", Revision: "comment-revision-2", Body: "Active comment", Author: connector.Actor{ID: "actor-a", DisplayName: "Reviewer A"}, CreatedAt: updated.Add(time.Minute), UpdatedAt: updated.Add(time.Minute)},
				{ID: "comment-edited", Revision: "comment-revision-3", Body: "Corrected comment", Author: connector.Actor{ID: "actor-b", DisplayName: "Reviewer B"}, CreatedAt: updated.Add(2 * time.Minute), UpdatedAt: updated.Add(3 * time.Minute)},
				{ID: "comment-deleted", Revision: "comment-revision-4", Author: connector.Actor{ID: "actor-c", DisplayName: "Reviewer C"}, CreatedAt: updated.Add(4 * time.Minute), UpdatedAt: updated.Add(5 * time.Minute), Deleted: true},
			},
			Fields: map[string]connector.FieldValue{
				"field-date":     {Kind: "date", Value: "2026-08-20"},
				"field-local":    {Kind: "local_datetime", Value: "2026-08-20T11:30:00", Timezone: "Europe/Paris"},
				"field-instant":  {Kind: "instant", Value: "2026-08-20T09:30:00Z"},
				"field-null":     {Kind: "null"},
				"field-readonly": {Kind: "date", Value: "2026-08-20"},
			},
		}},
		Fields: []connector.FieldDescriptor{
			{ID: "field-date", DisplayName: "Date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
			{ID: "field-local", DisplayName: "Local", AcceptedKinds: []string{"local_datetime"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
			{ID: "field-instant", DisplayName: "Instant", AcceptedKinds: []string{"instant"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
			{ID: "field-null", DisplayName: "Nullable", AcceptedKinds: []string{"date", "local_datetime", "instant"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
			{ID: "field-readonly", DisplayName: "Read only", AcceptedKinds: []string{"date"}, Writable: false, SchemaRevision: "schema-1"},
		},
		Behavior: Behavior{CrashBeforeReply: map[string]int{}, CrashAfterMutation: map[string]int{}, Errors: map[string]connector.Error{}},
	})
}

func (f *conformanceFixture) ExternalState(context.Context) (connectorconformance.RootState, error) {
	current, err := Load(f.path)
	if err != nil {
		return connectorconformance.RootState{}, err
	}
	if len(current.Roots) != 1 {
		return connectorconformance.RootState{}, fmt.Errorf("fake connector roots = %d, want 1", len(current.Roots))
	}
	mutations := make([]connectorconformance.Mutation, len(current.Mutations))
	for index, mutation := range current.Mutations {
		mutations[index] = connectorconformance.Mutation{Method: mutation.Method, Params: mutation.Params}
	}
	return connectorconformance.RootState{
		Root: current.Roots[0].Root, Comments: current.Roots[0].Comments,
		Fields: current.Roots[0].Fields, Mutations: mutations,
	}, nil
}
