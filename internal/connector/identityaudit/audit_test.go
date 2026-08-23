package identityaudit

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidateRejectsForbiddenKeyVariantsInsideFieldValues(t *testing.T) {
	for _, key := range []string{
		"kata_uid", "kataUid", "kata-uid", "kata.uid",
		"kata_ref", "kataRef", "kata-ref", "kata.ref",
		"kata_project_id", "kataProjectId", "kata-project-id", "kata.project.id",
		"kata_binding_id", "kataBindingId", "kata-binding-id", "kata.binding.id",
		"kata_work_branch", "kataWorkBranch", "kata-work-branch", "kata.work.branch",
	} {
		raw, err := json.Marshal(map[string]any{
			"root_key": "root-example",
			"fields": map[string]any{
				"kata_uid": map[string]any{"kind": "date", "value": "2026-08-20", key: "neutral-forbidden"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		err = Validate("write_fields", raw, Options{ExternalRootKey: "root-example"})
		var auditErr *Error
		if !errors.As(err, &auditErr) || auditErr.Code != CodeForbiddenKey {
			t.Fatalf("key %q error = %#v, want forbidden-key error", key, err)
		}
	}
}

func TestValidateAllowsOpaqueFieldIDs(t *testing.T) {
	raw := json.RawMessage(`{"root_key":"root-example","fields":{"kata_uid":{"kind":"date","value":"2026-08-20"},"katakana_start":{"kind":"null"}}}`)
	if err := Validate("write_fields", raw, Options{ExternalRootKey: "root-example"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsArbitraryStructuralKataKeys(t *testing.T) {
	for _, key := range []string{"kataOwnerId", "kata-owner-ref", "kata.project.token", "kata"} {
		raw, err := json.Marshal(map[string]any{
			"root_key": "root-example",
			"fields": map[string]any{
				"katakana_start": map[string]any{"kind": "date", "value": "2026-08-20", key: "neutral-forbidden"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		err = Validate("write_fields", raw, Options{ExternalRootKey: "root-example"})
		var auditErr *Error
		if !errors.As(err, &auditErr) || auditErr.Code != CodeForbiddenKey {
			t.Fatalf("key %q error = %#v, want forbidden-key error", key, err)
		}
	}

	raw := json.RawMessage(`{"root_key":"root-example","fields":{"katakana_start":{"kind":"null"}}}`)
	if err := Validate("write_fields", raw, Options{ExternalRootKey: "root-example"}); err != nil {
		t.Fatalf("katakana field rejected: %v", err)
	}
}

func TestValidateRejectsWrongParameterTypes(t *testing.T) {
	tests := []struct {
		method string
		raw    json.RawMessage
	}{
		{method: "resolve_root", raw: json.RawMessage(`{"locator":7}`)},
		{method: "read_root", raw: json.RawMessage(`{"root_key":false}`)},
		{method: "list_comments", raw: json.RawMessage(`{"root_key":[]}`)},
		{method: "publish_comment", raw: json.RawMessage(`{"root_key":"root-example","body":{},"operation_id":"operation-example"}`)},
		{method: "publish_comment", raw: json.RawMessage(`{"root_key":3,"body":"example","operation_id":"operation-example"}`)},
		{method: "publish_comment", raw: json.RawMessage(`{"root_key":"root-example","body":"example","operation_id":3}`)},
		{method: "complete_root", raw: json.RawMessage(`{"root_key":null}`)},
		{method: "read_fields", raw: json.RawMessage(`{"root_key":{},"field_ids":[]}`)},
		{method: "read_fields", raw: json.RawMessage(`{"root_key":"root-example","field_ids":"field-example"}`)},
		{method: "read_fields", raw: json.RawMessage(`{"root_key":"root-example","field_ids":[3]}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":true,"fields":{}}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":"field-example"}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":null}}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{}}}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{"kind":4}}}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{"kind":"date","value":{}}}}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{"kind":"instant","timezone":[]}}}`)},
		{method: "write_fields", raw: json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{"kind":"null","metadata":"unexpected"}}}`)},
	}
	for _, test := range tests {
		err := Validate(test.method, test.raw, Options{ExternalRootKey: "root-example"})
		var auditErr *Error
		if !errors.As(err, &auditErr) || auditErr.Code != CodeInvalidJSON {
			t.Errorf("%s params %s error = %#v, want invalid-json error", test.method, test.raw, err)
		}
	}
}

func TestValidateTreatsReadFieldSelectorsAsOpaque(t *testing.T) {
	options := Options{
		ExternalRootKey: "root-example",
		LongUIDs:        []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		ShortIDs:        []string{"root-short"},
	}
	raw := json.RawMessage(`{"root_key":"root-example","field_ids":["root-short","prefix-01ARZ3NDEKTSV4RRFFQ69G5FAV-suffix","kataOwnerId"]}`)
	if err := Validate("read_fields", raw, options); err != nil {
		t.Fatalf("opaque field selectors rejected: %v", err)
	}

	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"root_key":"root-short","field_ids":[]}`),
		json.RawMessage(`{"root_key":"root-example","field_ids":[],"kataOwnerId":"neutral-forbidden"}`),
	} {
		if err := Validate("read_fields", invalid, options); err == nil {
			t.Fatalf("surrounding identity channel accepted: %s", invalid)
		}
	}
}

func TestValidateLocalIdentityMatching(t *testing.T) {
	options := Options{
		ExternalRootKey: "root-example",
		LongUIDs:        []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW"},
		ShortIDs:        []string{"root-short", "child-short"},
	}
	for _, body := range []string{
		"prefix-01ARZ3NDEKTSV4RRFFQ69G5FAV-suffix",
		"prefix-01ARZ3NDEKTSV4RRFFQ69G5FAW-suffix",
		"root-short",
		"child-short",
	} {
		raw, _ := json.Marshal(map[string]any{"root_key": "root-example", "body": body, "operation_id": "operation-example"})
		if err := Validate("publish_comment", raw, options); err == nil {
			t.Fatalf("local identity body %q was accepted", body)
		}
	}
	for _, body := range []string{"prefix-root-short-suffix", "unrelated-01ARZ3NDEKTSV4RRFFQ69G5FAX-suffix"} {
		raw, _ := json.Marshal(map[string]any{"root_key": "root-example", "body": body, "operation_id": "operation-example"})
		if err := Validate("publish_comment", raw, options); err != nil {
			t.Fatalf("safe body %q rejected: %v", body, err)
		}
	}
}

func TestValidateAllMethodShapesAndExactEOF(t *testing.T) {
	valid := map[string]json.RawMessage{
		"describe":        json.RawMessage(`{}`),
		"resolve_root":    json.RawMessage(`{"locator":"fixture-root"}`),
		"read_root":       json.RawMessage(`{"root_key":"root-example"}`),
		"list_comments":   json.RawMessage(`{"root_key":"root-example"}`),
		"publish_comment": json.RawMessage(`{"root_key":"root-example","body":"Example","operation_id":"operation-example"}`),
		"complete_root":   json.RawMessage(`{"root_key":"root-example"}`),
		"list_fields":     json.RawMessage(`{}`),
		"read_fields":     json.RawMessage(`{"root_key":"root-example","field_ids":["field-example"]}`),
		"write_fields":    json.RawMessage(`{"root_key":"root-example","fields":{"field-example":{"kind":"null"}}}`),
	}
	for method, raw := range valid {
		if err := Validate(method, raw, Options{ExternalRootKey: "root-example"}); err != nil {
			t.Fatalf("%s valid params rejected: %v", method, err)
		}
	}
	err := Validate("read_root", json.RawMessage(`{"root_key":"root-example"} {}`), Options{ExternalRootKey: "root-example"})
	var auditErr *Error
	if !errors.As(err, &auditErr) || auditErr.Code != CodeTrailingJSON {
		t.Fatalf("trailing JSON error = %#v, want trailing-json error", err)
	}
	err = Validate("describe", json.RawMessage(`null`), Options{})
	if !errors.As(err, &auditErr) || auditErr.Code != CodeInvalidJSON {
		t.Fatalf("null parameter error = %#v, want invalid-json error", err)
	}
}
