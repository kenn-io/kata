package forgejo

import (
	"encoding/json/v2"
	"net/http"
	"reflect"
	"testing"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

type fieldProvider struct {
	issue      map[string]any
	writes     []map[string]any
	failPatch  bool
	uiLocation *time.Location
}

func newFieldProvider(t *testing.T) (*fieldProvider, string) {
	t.Helper()
	p := &fieldProvider{}
	if err := json.Unmarshal([]byte(issueJSON), &p.issue); err != nil {
		t.Fatal(err)
	}
	p.issue["due_date"] = "2026-10-03T23:59:59-05:00"
	p.issue["labels"] = []map[string]any{{"id": 15}, {"id": 2}}
	p.issue["assignees"] = []map[string]any{{"id": 20, "login": "reviewer-z"}, {"id": 21, "login": "reviewer-a"}}
	p.issue["milestone"] = map[string]any{"id": 19}
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: 8, Login: "reviewer"})
		case "/api/v1/orgs/owner":
			http.NotFound(w, r)
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/assignees":
			writeJSON(w, []map[string]any{{"id": 20, "login": "reviewer-z"}, {"id": 21, "login": "reviewer-a"}})
		case "/api/v1/repos/owner/renamed/labels", "/api/v1/repos/owner/renamed/milestones":
			if r.URL.Query().Get("page") != "1" {
				writeJSON(w, []any{})
				return
			}
			switch r.URL.Path {
			case "/api/v1/repos/owner/renamed/labels":
				writeJSON(w, []map[string]any{{"id": 2}, {"id": 15}, {"id": 20}})
			case "/api/v1/repos/owner/renamed/milestones":
				writeJSON(w, []map[string]any{{"id": 19}, {"id": 24}})
			}
		case "/api/v1/repos/owner/renamed/issues/7/labels":
			if r.Method != "PUT" {
				t.Errorf("labels method %s", r.Method)
			}
			var payload map[string]any
			if err := json.UnmarshalRead(r.Body, &payload); err != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			p.writes = append(p.writes, payload)
			vals := []map[string]any{}
			for _, v := range payload["labels"].([]any) {
				vals = append(vals, map[string]any{"id": v})
			}
			p.issue["labels"] = vals
			writeJSON(w, vals)
		case "/api/v1/repos/owner/renamed/issues/7":
			if r.Method == "PATCH" {
				var payload map[string]any
				if err := json.UnmarshalRead(r.Body, &payload); err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				payload = appliedOptions(payload)
				p.writes = append(p.writes, payload)
				if p.failPatch {
					http.Error(w, "private-error", 500)
					return
				}
				for k, v := range payload {
					switch k {
					case "unset_due_date":
						p.issue["due_date"] = nil
					case "due_date":
						if p.uiLocation == nil {
							p.issue[k] = v
							break
						}
						text, ok := v.(string)
						if !ok {
							t.Errorf("due_date patch is %T, want string", v)
							break
						}
						at, err := time.Parse(time.RFC3339, text)
						if err != nil {
							t.Errorf("parse due_date patch: %v", err)
							break
						}
						// Forgejo 11 treats the date as a calendar value, stores
						// it at the end of that day in the submitted location,
						// then serializes it in the configured UI location.
						deadline := time.Date(at.Year(), at.Month(), at.Day(), 23, 59, 59, 0, at.Location())
						p.issue[k] = deadline.In(p.uiLocation).Format(time.RFC3339)
					case "milestone":
						if v == float64(0) {
							p.issue[k] = nil
						} else {
							p.issue[k] = map[string]any{"id": v}
						}
					case "assignees":
						vals := []map[string]any{}
						for _, login := range v.([]any) {
							vals = append(vals, map[string]any{"id": 20, "login": login})
						}
						p.issue[k] = vals
					default:
						t.Errorf("unexpected patch key %s", k)
					}
				}
			}
			writeJSON(w, p.issue)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	return p, s.URL
}
func TestFieldDescriptorsAndLosslessReadback(t *testing.T) {
	_, origin := newFieldProvider(t)
	h := New("secret-test-token")
	desc := result[connector.Description](t, exchange(t, h, origin, "describe", connector.DescribeParams{}))
	fieldsCap := false
	for _, cap := range desc.Capabilities {
		if cap == connector.CapabilityConditionalFields {
			t.Fatal("dishonest conditional capability")
		}
		fieldsCap = fieldsCap || cap == connector.CapabilityFields
	}
	if !fieldsCap {
		t.Fatal("missing fields capability")
	}
	descriptors := result[connector.ListFieldsResult](t, exchange(t, h, origin, "list_fields", connector.ListFieldsParams{}))
	if len(descriptors.Fields) != 4 {
		t.Fatalf("descriptors %#v", descriptors)
	}
	got := result[connector.ReadFieldsResult](t, exchange(t, h, origin, "read_fields", connector.ReadFieldsParams{RootKey: "23#7@91", FieldIDs: []string{"due_date", "labels", "assignees", "milestone"}}))
	want := map[string]connector.FieldValue{"due_date": {Kind: "date", Value: "2026-10-03"}, "labels": {Kind: "string_set", Values: []string{"15", "2"}}, "assignees": {Kind: "string_set", Values: []string{"reviewer-a", "reviewer-z"}}, "milestone": {Kind: "string", Value: "19"}}
	if !reflect.DeepEqual(got.Fields, want) {
		t.Fatalf("read %#v want %#v", got.Fields, want)
	}
}
func TestFieldsValidateAllBeforeWritesAndRejectConditionalRequests(t *testing.T) {
	p, origin := newFieldProvider(t)
	h := New("secret-test-token")
	for _, fields := range []map[string]connector.FieldValue{
		{"labels": {Kind: "string_set", Values: []string{"999"}}, "due_date": {Kind: "date", Value: "2026-11-01"}},
		{"labels": {Kind: "string_set", Values: []string{"02"}}, "due_date": {Kind: "date", Value: "2026-11-01"}},
		{"milestone": {Kind: "string", Value: "999"}, "labels": {Kind: "string_set", Values: []string{"2"}}},
		{"assignees": {Kind: "string_set", Values: []string{"unknown-user"}}, "labels": {Kind: "string_set", Values: []string{"2"}}},
		{"due_date": {Kind: "instant", Value: "2026-11-01T15:00:00Z"}},
		{"labels": {Kind: "null"}},
		{"unknown": {Kind: "string", Value: "value"}},
	} {
		r := exchange(t, h, origin, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: fields})
		if r.Error == nil || len(p.writes) != 0 {
			t.Fatalf("invalid fields mutated: %#v writes %#v", r, p.writes)
		}
	}
	fields := map[string]connector.FieldValue{"due_date": {Kind: "date", Value: "2026-11-01"}}
	r := exchange(t, h, origin, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: fields, Expected: fields})
	if r.Error == nil || r.Error.Code != "unsupported" || len(p.writes) != 0 {
		t.Fatalf("conditional request %#v writes %#v", r, p.writes)
	}
}
func TestFieldsWriteClearAndPartialFailure(t *testing.T) {
	p, origin := newFieldProvider(t)
	h := New("secret-test-token")
	fields := map[string]connector.FieldValue{"due_date": {Kind: "date", Value: "2026-11-01"}, "labels": {Kind: "string_set", Values: []string{"20"}}, "assignees": {Kind: "string_set", Values: []string{"reviewer-a"}}, "milestone": {Kind: "string", Value: "24"}}
	got := result[connector.WriteFieldsResult](t, exchange(t, h, origin, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: fields}))
	if !reflect.DeepEqual(got.Fields, fields) || len(p.writes) != 2 {
		t.Fatalf("write %#v mutations %#v", got, p.writes)
	}
	if p.writes[1]["due_date"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("date patch %#v", p.writes[1])
	}
	clears := map[string]connector.FieldValue{"due_date": {Kind: "null"}, "labels": {Kind: "string_set", Values: []string{}}, "assignees": {Kind: "string_set", Values: []string{}}, "milestone": {Kind: "null"}}
	got = result[connector.WriteFieldsResult](t, exchange(t, h, origin, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: clears}))
	if len(got.Fields) != 4 || got.Fields["due_date"].Kind != "null" || got.Fields["milestone"].Kind != "null" || got.Fields["labels"].Kind != "string_set" || len(got.Fields["labels"].Values) != 0 || got.Fields["assignees"].Kind != "string_set" || len(got.Fields["assignees"].Values) != 0 || p.writes[3]["unset_due_date"] != true || p.writes[3]["milestone"] != float64(0) {
		t.Fatalf("clears %#v mutations %#v", got, p.writes)
	}
	p.failPatch = true
	r := exchange(t, h, origin, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: fields})
	if r.Error == nil || r.Error.Code == connector.ErrorCodeFieldConflict {
		t.Fatalf("partial failure %#v", r)
	}
	actual := result[connector.ReadFieldsResult](t, exchange(t, h, origin, "read_fields", connector.ReadFieldsParams{RootKey: "23#7@91", FieldIDs: []string{"labels", "due_date"}}))
	if !reflect.DeepEqual(actual.Fields["labels"], fields["labels"]) || actual.Fields["due_date"].Kind != "null" {
		t.Fatalf("partial state %#v", actual)
	}
}

func TestFieldsWriteDatePreservesProviderCalendarInPositiveOffset(t *testing.T) {
	p, origin := newFieldProvider(t)
	p.uiLocation = time.FixedZone("provider-east", 5*60*60)
	h := New("secret-test-token")
	want := connector.FieldValue{Kind: "date", Value: "2026-11-01"}

	got := result[connector.WriteFieldsResult](t, exchange(t, h, origin, "write_fields", connector.WriteFieldsParams{
		RootKey: "23#7@91", Fields: map[string]connector.FieldValue{"due_date": want},
	}))
	if !reflect.DeepEqual(got.Fields["due_date"], want) {
		t.Fatalf("write returned due date %#v, want %#v", got.Fields["due_date"], want)
	}
	readback := result[connector.ReadFieldsResult](t, exchange(t, h, origin, "read_fields", connector.ReadFieldsParams{
		RootKey: "23#7@91", FieldIDs: []string{"due_date"},
	}))
	if !reflect.DeepEqual(readback.Fields["due_date"], want) {
		t.Fatalf("readback due date %#v, want %#v", readback.Fields["due_date"], want)
	}
	if len(p.writes) != 2 || p.writes[1]["due_date"] != "2026-11-01T00:00:00+05:00" {
		t.Fatalf("due-date corrections %#v, want one calendar-day correction in provider timezone", p.writes)
	}
}

func TestOwningOrganizationLabelsAreValidButForeignSelectionsAreRejected(t *testing.T) {
	selected := []label{}
	writes := 0
	orgReads := 0
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			if r.Method == "PATCH" {
				writes++
			}
			var i issue
			if err := json.Unmarshal([]byte(issueJSON), &i); err != nil {
				t.Errorf("decode fixture issue: %v", err)
				return
			}
			i.Labels = selected
			writeJSON(w, i)
		case "/api/v1/repos/owner/renamed/labels":
			if r.URL.Query().Get("page") == "1" {
				writeJSON(w, []label{{ID: 2}})
			} else {
				writeJSON(w, []label{})
			}
		case "/api/v1/orgs/owner":
			orgReads++
			writeJSON(w, map[string]any{"id": 30, "username": "owner"})
		case "/api/v1/orgs/owner/labels":
			if r.URL.Query().Get("page") == "1" {
				writeJSON(w, []label{{ID: 42}})
			} else {
				writeJSON(w, []label{})
			}
		case "/api/v1/repos/owner/renamed/issues/7/labels":
			writes++
			var payload struct {
				Labels []int64 `json:"labels"`
			}
			if err := json.UnmarshalRead(r.Body, &payload); err != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			selected = []label{}
			for _, n := range payload.Labels {
				selected = append(selected, label{ID: n})
			}
			writeJSON(w, selected)
		default:
			t.Errorf("unexpected owner lookup or path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	h := New("secret-test-token")
	result[connector.WriteFieldsResult](t, exchange(t, h, s.URL, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: map[string]connector.FieldValue{"labels": {Kind: "string_set", Values: []string{"2", "42"}}}}))
	if writes != 1 || orgReads != 1 || len(selected) != 2 || selected[1].ID != 42 {
		t.Fatalf("valid organization label writes %d org reads %d labels %#v", writes, orgReads, selected)
	}
	invalid := map[string]connector.FieldValue{"labels": {Kind: "string_set", Values: []string{"42", "77"}}, "due_date": {Kind: "date", Value: "2026-11-01"}}
	if r := exchange(t, h, s.URL, "write_fields", connector.WriteFieldsParams{RootKey: "23#7@91", Fields: invalid}); r.Error == nil || writes != 1 {
		t.Fatalf("foreign label mutated %#v writes %d", r, writes)
	}
}
