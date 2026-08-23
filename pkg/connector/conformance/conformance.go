// Package conformance provides the provider-neutral behavioral contract for
// protocol-v1 external root connector handlers. Adapters need advertise only
// the lossless field kinds they support; Kata rejects unsupported descriptor
// kinds when operators configure field mappings.
package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"go.kenn.io/kata/internal/connector/identityaudit"
	"go.kenn.io/kata/pkg/connector"
)

// Fixture owns one disposable external root and exposes its provider-side
// state. Reset must restore the same stable connector and root identities.
type Fixture interface {
	// Exchange sends one complete JSON request line through the candidate's
	// own protocol boundary and returns its raw response bytes.
	Exchange(context.Context, []byte) ([]byte, error)
	// Invocation supplies the connector instance and raw settings used for
	// every protocol request in the transcripts.
	Invocation() connector.Invocation
	RootLocator() string
	Reset(context.Context) error
	ExternalState(context.Context) (RootState, error)
	// MutateComment edits one existing provider comment, advances its revision,
	// and preserves its ID.
	MutateComment(context.Context, string) error
	// InjectFault arms one controlled failure for the next matching exchange.
	InjectFault(context.Context, Fault) error
}

// Fault names a provider-side failure window exercised by the conformance kit.
type Fault string

const (
	// FaultPublishCommentCrashAfterMutation exits after persisting a publication
	// but before returning its protocol response.
	FaultPublishCommentCrashAfterMutation Fault = "publish_comment_crash_after_mutation"
)

// RootState is the provider-neutral state a conformance fixture exposes for
// readback after handler mutations.
type RootState struct {
	Root      connector.Root
	Comments  []connector.Comment
	Fields    map[string]connector.FieldValue
	Mutations []Mutation
}

// Mutation records the unfiltered provider-facing parameters received by a
// mutating v1 call. Params must preserve every nested key so Run can audit the
// candidate's actual external mutation surface.
type Mutation struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Run executes the protocol-v1 behavioral contract against a disposable
// fixture. Maintained Go adapters should call Run from their own test suite.
func Run(t *testing.T, fixture Fixture) {
	t.Helper()
	if fixture == nil {
		t.Fatal("connector conformance fixture is nil")
	}
	if strings.TrimSpace(fixture.RootLocator()) == "" {
		t.Fatal("connector conformance root locator is empty")
	}
	invocation := fixture.Invocation()
	if invocation.Instance == "" || strings.TrimSpace(invocation.Instance) != invocation.Instance || !utf8.ValidString(invocation.Instance) {
		t.Fatal("connector conformance instance is empty or noncanonical")
	}
	if !utf8.Valid(invocation.Settings) {
		t.Fatal("connector conformance settings are not valid UTF-8")
	}
	settings, err := decodeTranscriptJSON(invocation.Settings)
	if err != nil {
		t.Fatalf("decode connector conformance settings: %v", err)
	}
	if _, ok := settings.(map[string]any); !ok {
		t.Fatal("connector conformance settings must be a JSON object")
	}
	t.Run("language-neutral protocol transcripts", func(t *testing.T) {
		runProtocolV1Transcripts(t.Context(), t, fixture, invocation.Instance, settings)
	})
}

func auditMutationParams(method string, raw json.RawMessage, rootKey string) error {
	return identityaudit.Validate(method, raw, identityaudit.Options{ExternalRootKey: rootKey})
}
