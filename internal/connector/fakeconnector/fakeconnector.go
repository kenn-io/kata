// Package fakeconnector provides a deterministic protocol-v1 implementation
// for Kata's black-box connector tests.
package fakeconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

const (
	exitFailure            = 1
	exitCrashBeforeReply   = 41
	exitCrashAfterMutation = 42
)

// State is the complete test-owned on-disk connector state.
type State struct {
	Description  connector.Description        `json:"description"`
	Roots        []StoredRoot                 `json:"roots"`
	Fields       []connector.FieldDescriptor  `json:"fields"`
	Mutations    []Mutation                   `json:"mutations"`
	Calls        []Call                       `json:"calls"`
	Behavior     Behavior                     `json:"behavior"`
	NextComment  int                          `json:"next_comment"`
	Publications map[string]connector.Comment `json:"publications,omitempty"`
}

// StoredRoot joins a disposable locator to its provider-side root state.
type StoredRoot struct {
	Locator  string                          `json:"locator"`
	Root     connector.Root                  `json:"root"`
	Comments []connector.Comment             `json:"comments"`
	Fields   map[string]connector.FieldValue `json:"fields"`
}

// Mutation records one provider-side mutation and its raw parameters.
type Mutation struct {
	Sequence int             `json:"sequence"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
}

// Call records one protocol request and its raw parameters.
type Call struct {
	Sequence int             `json:"sequence"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
}

// Behavior configures deterministic crash and error responses.
type Behavior struct {
	CrashBeforeReply   map[string]int             `json:"crash_before_reply,omitempty"`
	CrashAfterMutation map[string]int             `json:"crash_after_mutation,omitempty"`
	Errors             map[string]connector.Error `json:"errors,omitempty"`
	ResponseProtocol   string                     `json:"response_protocol,omitempty"`
}

type handler struct {
	path       string
	params     json.RawMessage
	crashAfter bool
}

// NewHandler returns a handler backed by the test-owned state file.
func NewHandler(path string) connector.Handler { return &handler{path: path} }

// Run serves one complete protocol request using the test-owned state file.
func Run(path string, in io.Reader, out io.Writer) int {
	requestBytes, err := io.ReadAll(in)
	if err != nil {
		return exitFailure
	}
	var request connector.Request
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		return exitFailure
	}
	crash, err := consumeCounter(path, request.Method, true)
	if err != nil {
		return exitFailure
	}
	if crash {
		return exitCrashBeforeReply
	}
	if err := Update(path, func(current *State) error {
		current.Calls = append(current.Calls, Call{
			Sequence: len(current.Calls) + 1,
			Method:   request.Method,
			Params:   append(json.RawMessage(nil), request.Params...),
		})
		return nil
	}); err != nil {
		return exitFailure
	}

	h := &handler{path: path, params: append(json.RawMessage(nil), request.Params...)}
	var response bytes.Buffer
	if err := connector.ServeOne(context.Background(), bytes.NewReader(requestBytes), &response, h); err != nil {
		return exitFailure
	}
	if h.crashAfter {
		return exitCrashAfterMutation
	}
	current, err := Load(path)
	if err != nil {
		return exitFailure
	}
	if override := strings.TrimSpace(current.Behavior.ResponseProtocol); override != "" {
		var decoded connector.Response
		if err := json.Unmarshal(response.Bytes(), &decoded); err != nil {
			return exitFailure
		}
		decoded.Protocol = override
		response.Reset()
		if err := json.NewEncoder(&response).Encode(decoded); err != nil {
			return exitFailure
		}
	}
	if _, err := io.Copy(out, &response); err != nil {
		return exitFailure
	}
	return 0
}

func (h *handler) Describe(context.Context, connector.DescribeParams) (connector.Description, *connector.Error) {
	current, callErr := h.read("describe")
	if callErr != nil {
		return connector.Description{}, callErr
	}
	description := current.Description
	description.Capabilities = append([]connector.Capability(nil), description.Capabilities...)
	description.ConfigSchema = append(json.RawMessage(nil), description.ConfigSchema...)
	slices.Sort(description.Capabilities)
	return description, nil
}

func (h *handler) ResolveRoot(_ context.Context, params connector.ResolveRootParams) (connector.Root, *connector.Error) {
	current, callErr := h.read("resolve_root")
	if callErr != nil {
		return connector.Root{}, callErr
	}
	for _, candidate := range current.Roots {
		if candidate.Locator == params.Locator || candidate.Root.Key == params.Locator {
			return cloneRoot(candidate.Root), nil
		}
	}
	return connector.Root{}, notFound("root")
}

func (h *handler) ReadRoot(_ context.Context, params connector.ReadRootParams) (connector.Root, *connector.Error) {
	current, callErr := h.read("read_root")
	if callErr != nil {
		return connector.Root{}, callErr
	}
	root, ok := findRoot(&current, params.RootKey)
	if !ok {
		return connector.Root{}, notFound("root")
	}
	return cloneRoot(root.Root), nil
}

func (h *handler) ListComments(_ context.Context, params connector.ListCommentsParams) (connector.ListCommentsResult, *connector.Error) {
	current, callErr := h.read("list_comments")
	if callErr != nil {
		return connector.ListCommentsResult{}, callErr
	}
	root, ok := findRoot(&current, params.RootKey)
	if !ok {
		return connector.ListCommentsResult{}, notFound("root")
	}
	comments := append([]connector.Comment(nil), root.Comments...)
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	return connector.ListCommentsResult{Comments: comments}, nil
}

func (h *handler) CompleteRoot(_ context.Context, params connector.CompleteRootParams) (connector.Root, *connector.Error) {
	var result connector.Root
	callErr := h.mutate("complete_root", func(current *State) *connector.Error {
		root, ok := findRoot(current, params.RootKey)
		if !ok {
			return notFound("root")
		}
		current.record(h.recordedMutation("complete_root", params))
		if !strings.EqualFold(strings.TrimSpace(root.Root.State), "complete") {
			root.Root.State = "complete"
			advanceRoot(&root.Root, len(current.Mutations), nextProviderTime(root))
		}
		result = cloneRoot(root.Root)
		return nil
	})
	return result, callErr
}

func (h *handler) PublishComment(_ context.Context, params connector.PublishCommentParams) (connector.Comment, *connector.Error) {
	var result connector.Comment
	callErr := h.mutate("publish_comment", func(current *State) *connector.Error {
		root, ok := findRoot(current, params.RootKey)
		if !ok {
			return notFound("root")
		}
		if result, ok = current.Publications[params.OperationID]; ok {
			return nil
		}
		current.NextComment++
		stamp := nextProviderTime(root)
		result = connector.Comment{
			ID: fmt.Sprintf("published-%04d", current.NextComment), Revision: fmt.Sprintf("comment-revision-%04d", current.NextComment), Body: params.Body,
			Author:    connector.Actor{ID: current.Description.SelfActorID, DisplayName: "Connector Actor"},
			CreatedAt: stamp, UpdatedAt: stamp,
		}
		if current.Publications == nil {
			current.Publications = make(map[string]connector.Comment)
		}
		current.Publications[params.OperationID] = result
		root.Comments = append(root.Comments, result)
		current.record(h.recordedMutation("publish_comment", params))
		return nil
	})
	return result, callErr
}

func (h *handler) ListFields(context.Context, connector.ListFieldsParams) (connector.ListFieldsResult, *connector.Error) {
	current, callErr := h.read("list_fields")
	if callErr != nil {
		return connector.ListFieldsResult{}, callErr
	}
	fields := append([]connector.FieldDescriptor(nil), current.Fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].ID < fields[j].ID })
	for index := range fields {
		fields[index].AcceptedKinds = append([]string(nil), fields[index].AcceptedKinds...)
		sort.Strings(fields[index].AcceptedKinds)
	}
	return connector.ListFieldsResult{Fields: fields}, nil
}

func (h *handler) ReadFields(_ context.Context, params connector.ReadFieldsParams) (connector.ReadFieldsResult, *connector.Error) {
	current, callErr := h.read("read_fields")
	if callErr != nil {
		return connector.ReadFieldsResult{}, callErr
	}
	root, ok := findRoot(&current, params.RootKey)
	if !ok {
		return connector.ReadFieldsResult{}, notFound("root")
	}
	fields := make(map[string]connector.FieldValue, len(params.FieldIDs))
	for _, id := range params.FieldIDs {
		value, exists := root.Fields[id]
		if !exists {
			return connector.ReadFieldsResult{}, notFound("field")
		}
		fields[id] = value
	}
	return connector.ReadFieldsResult{Fields: fields}, nil
}

// fieldValueEquivalent compares two field values semantically so canonical
// formatting differences do not fail conditional writes.
func fieldValueEquivalent(a, b connector.FieldValue) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case "null":
		return true
	case "date", "local_datetime":
		return a.Value == b.Value && a.Timezone == b.Timezone
	case "instant":
		left, leftErr := time.Parse(time.RFC3339Nano, a.Value)
		right, rightErr := time.Parse(time.RFC3339Nano, b.Value)
		return leftErr == nil && rightErr == nil && left.Equal(right)
	default:
		return a == b
	}
}

func (h *handler) WriteFields(_ context.Context, params connector.WriteFieldsParams) (connector.WriteFieldsResult, *connector.Error) {
	result := connector.WriteFieldsResult{Fields: make(map[string]connector.FieldValue, len(params.Fields))}
	callErr := h.mutate("write_fields", func(current *State) *connector.Error {
		root, ok := findRoot(current, params.RootKey)
		if !ok {
			return notFound("root")
		}
		for id, value := range params.Fields {
			descriptor, exists := findField(current.Fields, id)
			if !exists {
				return notFound("field")
			}
			if !descriptor.Writable || !acceptedValue(descriptor, value) {
				return &connector.Error{Code: "invalid_field_value", Message: "field value is not accepted"}
			}
			// Compare-and-set applies only when expected values were sent.
			if expected, sent := params.Expected[id]; sent {
				current := connector.FieldValue{Kind: "null"}
				if value, ok := root.Fields[id]; ok {
					current = value
				}
				if !fieldValueEquivalent(current, expected) {
					return &connector.Error{Code: connector.ErrorCodeFieldConflict, Message: "field " + id + " changed since it was read"}
				}
			}
		}
		if root.Fields == nil {
			root.Fields = make(map[string]connector.FieldValue)
		}
		for id, value := range params.Fields {
			root.Fields[id] = value
			result.Fields[id] = value
		}
		current.record(h.recordedMutation("write_fields", params))
		return nil
	})
	return result, callErr
}

func (h *handler) read(method string) (State, *connector.Error) {
	current, err := Load(h.path)
	if err != nil {
		return State{}, &connector.Error{Code: "state_unavailable", Message: "fixture state is unavailable"}
	}
	if configured, ok := current.Behavior.Errors[method]; ok {
		cloned := configured
		return State{}, &cloned
	}
	return current, nil
}

func (h *handler) mutate(method string, apply func(*State) *connector.Error) *connector.Error {
	var callErr *connector.Error
	err := Update(h.path, func(current *State) error {
		if configured, ok := current.Behavior.Errors[method]; ok {
			cloned := configured
			callErr = &cloned
			return nil
		}
		if callErr = apply(current); callErr != nil {
			return nil
		}
		if current.Behavior.CrashAfterMutation[method] > 0 {
			current.Behavior.CrashAfterMutation[method]--
			h.crashAfter = true
		}
		return nil
	})
	if err != nil {
		return &connector.Error{Code: "state_unavailable", Message: "fixture state is unavailable"}
	}
	return callErr
}

func (h *handler) recordedMutation(method string, fallback any) Mutation {
	params := append(json.RawMessage(nil), h.params...)
	if len(params) == 0 {
		params, _ = json.Marshal(fallback)
	}
	return Mutation{Method: method, Params: params}
}

func consumeCounter(path, method string, before bool) (bool, error) {
	consumed := false
	err := Update(path, func(current *State) error {
		counters := current.Behavior.CrashAfterMutation
		if before {
			counters = current.Behavior.CrashBeforeReply
		}
		if counters[method] > 0 {
			counters[method]--
			consumed = true
		}
		return nil
	})
	return consumed, err
}

func (s *State) record(value Mutation) {
	value.Sequence = len(s.Mutations) + 1
	s.Mutations = append(s.Mutations, value)
}

func findRoot(current *State, key string) (*StoredRoot, bool) {
	for index := range current.Roots {
		if current.Roots[index].Root.Key == key {
			return &current.Roots[index], true
		}
	}
	return nil, false
}

func findField(fields []connector.FieldDescriptor, id string) (connector.FieldDescriptor, bool) {
	for _, descriptor := range fields {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return connector.FieldDescriptor{}, false
}

func acceptedValue(descriptor connector.FieldDescriptor, value connector.FieldValue) bool {
	if value.Kind == "null" {
		return descriptor.Nullable && value.Value == "" && value.Timezone == ""
	}
	return slices.Contains(descriptor.AcceptedKinds, value.Kind) && value.Value != ""
}

func advanceRoot(root *connector.Root, sequence int, stamp time.Time) {
	root.Revision = fmt.Sprintf("revision-%04d", sequence)
	root.UpdatedAt = stamp
	root.ObservedAt = root.UpdatedAt
}

func nextProviderTime(root *StoredRoot) time.Time {
	stamp := time.Now().UTC()
	providerTimes := []time.Time{root.Root.UpdatedAt, root.Root.ObservedAt}
	for _, comment := range root.Comments {
		providerTimes = append(providerTimes, comment.CreatedAt, comment.UpdatedAt)
	}
	for _, providerTime := range providerTimes {
		if !stamp.After(providerTime) {
			stamp = providerTime.Add(time.Nanosecond)
		}
	}
	return stamp
}

func cloneRoot(root connector.Root) connector.Root {
	root.Fields = cloneFields(root.Fields)
	if root.Actor != nil {
		actor := *root.Actor
		root.Actor = &actor
	}
	return root
}

func cloneFields(fields map[string]connector.FieldValue) map[string]connector.FieldValue {
	if fields == nil {
		return nil
	}
	cloned := make(map[string]connector.FieldValue, len(fields))
	maps.Copy(cloned, fields)
	return cloned
}

func notFound(kind string) *connector.Error {
	return &connector.Error{Code: kind + "_not_found", Message: kind + " was not found"}
}

// Load reads the complete test-owned state.
func Load(path string) (State, error) {
	data, err := os.ReadFile(path) // #nosec G304,G703 -- path is the explicit test-owned state file supplied by the harness.
	if err != nil {
		return State{}, err
	}
	var current State
	if err := json.Unmarshal(data, &current); err != nil {
		return State{}, err
	}
	return current, nil
}

// Update applies one locked atomic state transition.
func Update(path string, apply func(*State) error) error {
	unlock, err := lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := Load(path)
	if err != nil {
		return err
	}
	if err := apply(&current); err != nil {
		return err
	}
	return Write(path, current)
}

// Write atomically replaces the test-owned state file.
func Write(path string, current State) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".fake-connector-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporaryPath) // #nosec G703 -- temporaryPath was returned by os.CreateTemp in the test-owned state directory.
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(current); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil { // #nosec G703 -- both paths are confined to the explicit test-owned state directory.
		return err
	}
	remove = false
	return nil
}
