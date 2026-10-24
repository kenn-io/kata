package api_test

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/api"
)

func TestScopedContractPublicDTOPrivacy(t *testing.T) {
	forbidden := map[string]bool{"revision": true, "signatures": true, "cut": true, "sequence": true, "previous_digest": true, "capture_uid": true}
	visited := map[reflect.Type]bool{}
	var checkType func(reflect.Type)
	checkType = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		if visited[typ] || typ.Kind() != reflect.Struct {
			return
		}
		visited[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if forbidden[name] {
				t.Errorf("public type %s exposes transport field %s", typ, name)
			}
			checkType(field.Type)
		}
	}
	for _, value := range []any{api.ScopedCurrentOut{}, api.ScopedIssueOut{}, api.ScopedMutationOut{}, api.ScopedOperationOut{}, api.ScopedControlOut{}} {
		checkType(reflect.TypeOf(value))
	}

	union := api.IssueReadUnion{Canonical: &api.IssueOut{}}
	direct, err := json.Marshal(union.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(union)
	if err != nil || !bytes.Equal(direct, encoded) {
		t.Fatalf("canonical v1 envelope changed: %s %v", encoded, err)
	}
	union.Scoped = &api.ScopedIssueOut{}
	if _, err := json.Marshal(union); err == nil {
		t.Fatal("ambiguous public union accepted")
	}
	union.Canonical = nil
	direct, err = json.Marshal(union.Scoped)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(union)
	if err != nil || !bytes.Equal(direct, encoded) {
		t.Fatalf("scoped response branch changed: %s %v", encoded, err)
	}
	union.Scoped = nil
	if _, err := json.Marshal(union); err == nil {
		t.Fatal("empty public union accepted")
	}
}
