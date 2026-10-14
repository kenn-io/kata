package forgejo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/kata/pkg/connector"
	"go.kenn.io/kata/pkg/connector/conformance"
)

func TestPublicConformance(t *testing.T) {
	f := &conformanceFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	conformance.Run(t, f)
}

func TestConformanceFixtureForwardsMalformedRequestsToServeOne(t *testing.T) {
	f := &conformanceFixture{}
	output, err := f.Exchange(t.Context(), []byte("{"))
	if err == nil || !strings.HasPrefix(err.Error(), "decode connector request:") {
		t.Fatalf("malformed request error = %v, want ServeOne decode error", err)
	}
	if len(output) != 0 {
		t.Fatalf("malformed request produced response %q", output)
	}
}

type conformanceFixture struct {
	mu              sync.Mutex
	server          *httptest.Server
	issue           issue
	comments        []comment
	publishedBodies map[int64]string
	mutations       []conformance.Mutation
	request         connector.Request
	crash, crashed  bool
}

func (f *conformanceFixture) RootLocator() string { return "owner/renamed#7" }
func (f *conformanceFixture) Invocation() connector.Invocation {
	settings, _ := json.Marshal(map[string]string{"origin": f.server.URL})
	return connector.Invocation{Instance: "example-instance", Settings: settings}
}
func (f *conformanceFixture) FieldSamples(field string) []connector.FieldValue {
	switch field {
	case "due_date":
		return []connector.FieldValue{{Kind: "date", Value: "2026-08-21"}}
	case "labels":
		return []connector.FieldValue{{Kind: "string_set", Values: []string{"15", "2"}}, {Kind: "string_set"}}
	case "assignees":
		return []connector.FieldValue{{Kind: "string_set", Values: []string{"reviewer-a", "reviewer-z"}}, {Kind: "string_set"}}
	case "milestone":
		return []connector.FieldValue{{Kind: "string", Value: "19"}}
	}
	return nil
}
func (f *conformanceFixture) Reset(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := json.Unmarshal([]byte(issueJSON), &f.issue); err != nil {
		return fmt.Errorf("decode fixture issue: %w", err)
	}
	f.issue.UpdatedAt = time.Now().UTC().Add(-time.Hour)
	f.issue.DueDate = nil
	f.issue.Labels = nil
	f.issue.Assignees = nil
	f.issue.Milestone = nil
	at := time.Now().UTC().Add(-30 * time.Minute)
	f.comments = []comment{{ID: 10, Body: "Existing comment", User: &user{ID: 4, Login: "author"}, CreatedAt: at, UpdatedAt: at}, {ID: 11, Body: "Edited native comment", User: &user{ID: 5, Login: "reviewer-a"}, CreatedAt: at.Add(time.Minute), UpdatedAt: at.Add(2 * time.Minute)}}
	f.publishedBodies = make(map[int64]string)
	f.mutations = nil
	f.crash = false
	f.crashed = false
	return nil
}
func (f *conformanceFixture) Exchange(ctx context.Context, raw []byte) ([]byte, error) {
	var request connector.Request
	_ = json.Unmarshal(raw, &request)
	f.mu.Lock()
	f.request = request
	f.crashed = false
	f.mu.Unlock()
	var out bytes.Buffer
	err := connector.ServeOne(ctx, bytes.NewReader(raw), &out, New("secret-test-token"))
	f.mu.Lock()
	crashed := f.crashed
	f.mu.Unlock()
	if crashed {
		return nil, fmt.Errorf("controlled crash after native comment POST")
	}
	return out.Bytes(), err
}
func (f *conformanceFixture) InjectFault(_ context.Context, fault conformance.Fault) error {
	if fault != conformance.FaultPublishCommentCrashAfterMutation {
		return fmt.Errorf("unsupported fault")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crash = true
	return nil
}
func (f *conformanceFixture) MutateComment(_ context.Context, commentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.comments {
		if id(f.comments[index].ID) == commentID {
			f.comments[index].Body = "Externally edited comment"
			f.comments[index].UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	return fmt.Errorf("missing comment")
}
func (f *conformanceFixture) ExternalState(context.Context) (conformance.RootState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fields := fakeProviderFields(f.issue)
	comments := make([]connector.Comment, 0, len(f.comments))
	for _, raw := range f.comments {
		body := raw.Body
		if published, ok := f.publishedBodies[raw.ID]; ok {
			body = published
		}
		comments = append(comments, fakeProviderComment(raw, body))
	}
	return conformance.RootState{
		Root: fakeProviderRoot(f.issue, f.server.URL, fields), Fields: fields,
		Comments: comments, Mutations: slices.Clone(f.mutations),
	}, nil
}

func fakeProviderRoot(value issue, origin string, fields map[string]connector.FieldValue) connector.Root {
	state := "open"
	if value.State == "closed" {
		state = "complete"
	}
	root := connector.Root{
		Key:         fmt.Sprintf("%d#%d@%d", value.Repository.ID, value.Number, value.ID),
		IdentityKey: origin, Title: value.Title, Body: value.Body, State: state,
		UpdatedAt: value.UpdatedAt, Fields: fields,
	}
	root.Revision = fakeProviderRevision(root)
	root.ObservedAt = time.Now().UTC()
	return root
}

func fakeProviderFields(value issue) map[string]connector.FieldValue {
	fields := map[string]connector.FieldValue{
		"due_date": {Kind: "null"}, "milestone": {Kind: "null"},
		"labels":    {Kind: "string_set", Values: []string{}},
		"assignees": {Kind: "string_set", Values: []string{}},
	}
	if value.DueDate != nil {
		fields["due_date"] = connector.FieldValue{Kind: "date", Value: value.DueDate.Format("2006-01-02")}
	}
	if value.Milestone != nil {
		fields["milestone"] = connector.FieldValue{Kind: "string", Value: strconv.FormatInt(value.Milestone.ID, 10)}
	}
	labels := make([]string, 0, len(value.Labels))
	for _, item := range value.Labels {
		labels = append(labels, strconv.FormatInt(item.ID, 10))
	}
	slices.Sort(labels)
	fields["labels"] = connector.FieldValue{Kind: "string_set", Values: labels}
	assignees := make([]string, 0, len(value.Assignees))
	for _, item := range value.Assignees {
		assignees = append(assignees, item.Login)
	}
	slices.Sort(assignees)
	fields["assignees"] = connector.FieldValue{Kind: "string_set", Values: assignees}
	return fields
}

func fakeProviderComment(value comment, visibleBody string) connector.Comment {
	result := connector.Comment{
		ID: strconv.FormatInt(value.ID, 10), Body: visibleBody,
		Author:    connector.Actor{ID: strconv.FormatInt(value.User.ID, 10), DisplayName: value.User.Login},
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	result.Revision = fakeProviderRevision(result)
	return result
}

func fakeProviderRevision(value any) string {
	encoded, _ := json.Marshal(value, json.Deterministic(true))
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Convert only documented native provider parameters to portable values. Keep
// unknown parameters intact so the public audit sees accidental extra keys.
func (f *conformanceFixture) record(raw map[string]any) {
	params := map[string]any{"root_key": "23#7@91"}
	switch f.request.Method {
	case "complete_root":
		for key, value := range raw {
			if key != "state" {
				params[key] = value
			}
		}
	case "publish_comment":
		var p connector.PublishCommentParams
		if err := json.Unmarshal(f.request.Params, &p); err != nil {
			params["invalid_request"] = true
		}
		body, _ := raw["body"].(string)
		suffix := "\n\n" + (&client{instance: "example-instance"}).marker("23#7@91", 8, p.OperationID)
		if !strings.HasSuffix(body, suffix) {
			params["invalid_marker"] = body
		} else {
			params["body"] = strings.TrimSuffix(body, suffix)
		}
		params["operation_id"] = p.OperationID
		for key, value := range raw {
			if key != "body" {
				params[key] = value
			}
		}
	case "write_fields":
		fields := map[string]connector.FieldValue{}
		for key, value := range raw {
			switch key {
			case "due_date":
				text, _ := value.(string)
				at, _ := time.Parse(time.RFC3339, text)
				fields[key] = connector.FieldValue{Kind: "date", Value: at.Format("2006-01-02")}
			case "unset_due_date":
				fields["due_date"] = connector.FieldValue{Kind: "null"}
			case "milestone":
				n, _ := value.(float64)
				v := connector.FieldValue{Kind: "null"}
				if n != 0 {
					v = connector.FieldValue{Kind: "string", Value: strconv.FormatInt(int64(n), 10)}
				}
				fields[key] = v
			case "labels", "assignees":
				vals := []string{}
				for _, item := range value.([]any) {
					if key == "labels" {
						vals = append(vals, strconv.FormatInt(int64(item.(float64)), 10))
					} else {
						vals = append(vals, item.(string))
					}
				}
				slices.Sort(vals)
				fields[key] = connector.FieldValue{Kind: "string_set", Values: vals}
			default:
				params[key] = value
			}
		}
		params["fields"] = fields
	}
	encoded, _ := json.Marshal(params)
	f.mutations = append(f.mutations, conformance.Mutation{Method: f.request.Method, Params: jsontext.Value(encoded)})
}
func (f *conformanceFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "token secret-test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/v1/user":
		writeJSON(w, user{ID: 8, Login: "reviewer"})
	case "/api/v1/repositories/23", "/api/v1/repos/owner/renamed":
		writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
	case "/api/v1/repos/owner/renamed/assignees":
		writeJSON(w, []user{{ID: 20, Login: "reviewer-a"}, {ID: 21, Login: "reviewer-z"}})
	case "/api/v1/repos/owner/renamed/labels", "/api/v1/repos/owner/renamed/milestones":
		if r.URL.Query().Get("page") != "1" {
			writeJSON(w, []any{})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			writeJSON(w, []label{{ID: 15}, {ID: 2}})
		default:
			writeJSON(w, []label{{ID: 19}})
		}
	case "/api/v1/repos/owner/renamed/issues/7":
		if r.Method == "PATCH" {
			var raw map[string]any
			if json.UnmarshalRead(r.Body, &raw) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			raw = appliedOptions(raw)
			f.record(raw)
			for key, value := range raw {
				switch key {
				case "state":
					f.issue.State = value.(string)
				case "due_date":
					at, _ := time.Parse(time.RFC3339, value.(string))
					f.issue.DueDate = &at
				case "unset_due_date":
					f.issue.DueDate = nil
				case "milestone":
					n := int64(value.(float64))
					f.issue.Milestone = nil
					if n != 0 {
						f.issue.Milestone = &label{ID: n}
					}
				case "assignees":
					f.issue.Assignees = []user{}
					for _, login := range value.([]any) {
						f.issue.Assignees = append(f.issue.Assignees, user{ID: 20, Login: login.(string)})
					}
				default:
					http.Error(w, "invalid parameter", 400)
					return
				}
			}
			f.issue.UpdatedAt = time.Now().UTC()
		}
		writeJSON(w, f.issue)
	case "/api/v1/repos/owner/renamed/issues/7/labels":
		var raw map[string]any
		if r.Method != "PUT" || json.UnmarshalRead(r.Body, &raw) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		raw = appliedOptions(raw)
		f.record(raw)
		f.issue.Labels = []label{}
		for _, item := range raw["labels"].([]any) {
			f.issue.Labels = append(f.issue.Labels, label{ID: int64(item.(float64))})
		}
		f.issue.UpdatedAt = time.Now().UTC()
		writeJSON(w, f.issue.Labels)
	case "/api/v1/repos/owner/renamed/issues/7/comments":
		if r.Method == "POST" {
			var raw map[string]any
			if json.UnmarshalRead(r.Body, &raw) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			f.record(raw)
			now := time.Now().UTC()
			created := comment{ID: int64(100 + len(f.comments)), Body: raw["body"].(string), User: &user{ID: 8, Login: "reviewer"}, CreatedAt: now, UpdatedAt: now}
			f.comments = append(f.comments, created)
			var publication connector.PublishCommentParams
			if err := json.Unmarshal(f.request.Params, &publication); err != nil {
				http.Error(w, "invalid publication request", http.StatusBadRequest)
				return
			}
			f.publishedBodies[created.ID] = publication.Body
			if f.crash {
				f.crash = false
				f.crashed = true
			}
			writeJSON(w, created)
			return
		}
		writeJSON(w, f.comments)
	default:
		http.NotFound(w, r)
	}
}
