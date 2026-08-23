package fakeconnector

import (
	"encoding/json"
	"testing"
)

func TestAuditExternalSurfaceUsesSharedParameterValidation(t *testing.T) {
	for _, call := range []Call{
		{Method: "resolve_root", Params: json.RawMessage(`{"locator":7}`)},
		{Method: "write_fields", Params: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{"kind":"null","kataOwnerId":"neutral-forbidden"}}}`)},
	} {
		current := State{Calls: []Call{call}}
		if err := AuditExternalSurface(current, "root-example", nil, nil); err == nil {
			t.Fatalf("%s invalid params accepted: %s", call.Method, call.Params)
		}
	}
}

func TestAuditExternalSurfaceTreatsReadFieldSelectorsAsOpaque(t *testing.T) {
	current := State{Calls: []Call{{
		Method: "read_fields",
		Params: json.RawMessage(`{"root_key":"root-example","field_ids":["root-short","prefix-01ARZ3NDEKTSV4RRFFQ69G5FAV-suffix","kataOwnerId"]}`),
	}}}
	if err := AuditExternalSurface(
		current,
		"root-example",
		[]string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		[]string{"root-short"},
	); err != nil {
		t.Fatalf("opaque field selectors rejected: %v", err)
	}
}
