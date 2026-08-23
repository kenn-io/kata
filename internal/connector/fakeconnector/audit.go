package fakeconnector

import (
	"encoding/json"
	"fmt"

	"go.kenn.io/kata/internal/connector/identityaudit"
)

// AuditExternalSurface validates every raw fake call and mutation against the
// provider-neutral protocol surface and the supplied local identities.
func AuditExternalSurface(current State, externalRootKey string, longUIDs, shortIDs []string) error {
	options := identityaudit.Options{
		ExternalRootKey: externalRootKey,
		LongUIDs:        longUIDs,
		ShortIDs:        shortIDs,
	}
	for _, recorded := range recordedExternalParams(current) {
		if err := identityaudit.Validate(recorded.method, recorded.params, options); err != nil {
			return fmt.Errorf("%s %q: %w", recorded.kind, recorded.method, err)
		}
	}
	return nil
}

type recordedParams struct {
	kind   string
	method string
	params json.RawMessage
}

func recordedExternalParams(current State) []recordedParams {
	result := make([]recordedParams, 0, len(current.Calls)+len(current.Mutations))
	for _, call := range current.Calls {
		result = append(result, recordedParams{kind: "call", method: call.Method, params: call.Params})
	}
	for _, mutation := range current.Mutations {
		result = append(result, recordedParams{kind: "mutation", method: mutation.Method, params: mutation.Params})
	}
	return result
}
