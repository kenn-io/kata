package tui

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/tui/cron/records"
)

func TestCronClientUsesActiveRoute(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/7/cron/jobs":
			_, _ = w.Write([]byte(`{"jobs":[]}`))
		case "/api/v1/projects/7/cron/workflows":
			_, _ = w.Write([]byte(`{"workflows":[]}`))
		case "/api/v1/projects/7/cron/runs":
			_, _ = w.Write([]byte(`{"runs":[],"next_before_uid":"older"}`))
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewClient(server.URL, server.Client())
	if _, err := c.ListCronJobs(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListCronWorkflows(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	page, err := c.ListCronRuns(context.Background(), 7, "job-uid", "before")
	if err != nil {
		t.Fatal(err)
	}
	if page.NextBeforeUID != "older" {
		t.Fatal(page)
	}
	if len(requests) != 3 {
		t.Fatal(requests)
	}
}

func TestCronClientPreservesExactOptions(t *testing.T) {
	const definition = `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"hello"},"overlap":"allow","catchup":"skip","options":{"number":9007199254740993}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"uid":"job-uid","name":"example-job","definition_event_uid":"guard","definition":` + definition + `}]}`))
	}))
	defer server.Close()
	jobs, err := NewClient(server.URL, server.Client()).ListCronJobs(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Options map[string]jsontext.Value `json:"options"`
	}
	if err = json.Unmarshal(jobs[0].Definition, &d); err != nil {
		t.Fatal(err)
	}
	if string(d.Options["number"]) != "9007199254740993" {
		t.Fatal(string(jobs[0].Definition))
	}
}

func TestCronJobPausePreservesDefinition(t *testing.T) {
	const def = `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"hello"},"issue":{"kind":"per-run","title":"Summary","scheduled_offset_seconds":123},"overlap":"allow","catchup":"skip","options":{"number":9007199254740993,"agent":"portable"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/v1/projects/7/cron/jobs/job-uid" {
			t.Error(r.Method, r.URL.Path)
		}
		var body struct {
			Name, Actor      string
			ExpectedEventUID string         `json:"expected_event_uid"`
			Definition       jsontext.Value `json:"definition"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ExpectedEventUID != "original-guard" || body.Actor != "example-actor" {
			t.Error(body)
		}
		if !strings.Contains(string(body.Definition), "9007199254740993") || !strings.Contains(string(body.Definition), `"enabled":false`) {
			t.Error(string(body.Definition))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job":{"uid":"job-uid","name":"Summary","definition_event_uid":"new-guard","definition":` + string(body.Definition) + `}}`))
	}))
	defer server.Close()
	svc := cronProjectClient{client: NewClient(server.URL, server.Client()), projectID: 7, actor: "example-actor"}
	saved, err := svc.Pause(records.Job{UID: "job-uid", Name: "Summary", DefinitionEventUID: "original-guard", Definition: jsontext.Value(def)})
	if err != nil {
		t.Fatal(err)
	}
	if saved.DefinitionEventUID != "new-guard" {
		t.Fatal(saved)
	}
}
