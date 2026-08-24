package conformance

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/kata/pkg/connector"
)

const protocolTranscriptSchema = "kata.connector.conformance/v1"

//go:embed testdata/protocol-v1/*.json
var protocolV1TranscriptFiles embed.FS

type protocolTranscript struct {
	Schema       string                   `json:"schema"`
	Protocol     string                   `json:"protocol"`
	Name         string                   `json:"name"`
	FieldSamples []json.RawMessage        `json:"field_samples,omitempty"`
	Steps        []protocolTranscriptStep `json:"steps"`
}

type protocolTranscriptStep struct {
	Name          string                    `json:"name"`
	Request       json.RawMessage           `json:"request,omitempty"`
	RawRequest    string                    `json:"raw_request,omitempty"`
	Observe       string                    `json:"observe,omitempty"`
	Fault         Fault                     `json:"fault,omitempty"`
	MutateComment *transcriptValue          `json:"mutate_comment,omitempty"`
	When          []transcriptAssertion     `json:"when,omitempty"`
	Assert        []transcriptAssertion     `json:"assert,omitempty"`
	Capture       []transcriptCapture       `json:"capture,omitempty"`
	Repeat        *protocolTranscriptRepeat `json:"repeat,omitempty"`
}

type protocolTranscriptRepeat struct {
	Over  transcriptValue          `json:"over"`
	As    string                   `json:"as"`
	Steps []protocolTranscriptStep `json:"steps"`
}

type transcriptCapture struct {
	Name  string          `json:"name"`
	Value transcriptValue `json:"value"`
}

type transcriptAssertion struct {
	Op               string                `json:"op"`
	Actual           transcriptValue       `json:"actual,omitzero"`
	Expected         transcriptValue       `json:"expected,omitzero"`
	Start            transcriptValue       `json:"start,omitzero"`
	End              transcriptValue       `json:"end,omitzero"`
	Key              transcriptValue       `json:"key,omitzero"`
	Pattern          string                `json:"pattern,omitempty"`
	Paths            []string              `json:"paths,omitempty"`
	By               []string              `json:"by,omitempty"`
	As               string                `json:"as,omitempty"`
	Assert           []transcriptAssertion `json:"assert,omitempty"`
	ToleranceSeconds int                   `json:"tolerance_seconds,omitempty"`
}

type transcriptValue struct {
	Path    string          `json:"path,omitempty"`
	Literal json.RawMessage `json:"literal,omitempty"`
	Last    bool            `json:"last,omitempty"`
}

type transcriptRuntime struct {
	fixture Fixture
	root    map[string]any
}

type transcriptIdentity struct {
	connectorID string
	accountID   string
	rootLocator string
	rootKey     string
}

func runProtocolV1Transcripts(ctx context.Context, t *testing.T, fixture Fixture, instance string, settings any) {
	t.Helper()
	transcripts, err := loadProtocolV1Transcripts()
	if err != nil {
		t.Fatalf("load protocol-v1 transcripts: %v", err)
	}
	var baselineIdentity *transcriptIdentity
	for _, transcript := range transcripts {
		t.Run(transcript.Name, func(t *testing.T) {
			if err := fixture.Reset(ctx); err != nil {
				t.Fatalf("reset transcript fixture: %v", err)
			}
			samples := make([]any, 0, len(transcript.FieldSamples))
			for index, sample := range transcript.FieldSamples {
				decoded, err := decodeTranscriptJSON(sample)
				if err != nil {
					t.Fatalf("decode field sample %d: %v", index, err)
				}
				samples = append(samples, decoded)
			}
			runtime := &transcriptRuntime{fixture: fixture, root: map[string]any{
				"contract": map[string]any{"protocol": transcript.Protocol, "field_samples": samples},
				"fixture": map[string]any{
					"root_locator": fixture.RootLocator(),
					"invocation":   map[string]any{"instance": instance, "settings": cloneJSONValue(settings)},
				},
				"steps":    map[string]any{},
				"captures": map[string]any{},
				"vars":     map[string]any{},
			}}
			runtime.runSteps(ctx, t, transcript.Steps)
			identity, err := runtime.identity(ctx)
			if err != nil {
				t.Fatalf("read stable transcript identity: %v", err)
			}
			if baselineIdentity == nil {
				baselineIdentity = &identity
			} else if identity != *baselineIdentity {
				t.Fatalf("connector identity changed across transcript reset: got %+v want %+v", identity, *baselineIdentity)
			}
		})
	}
}

func (runtime *transcriptRuntime) identity(ctx context.Context) (transcriptIdentity, error) {
	connectorID, err := runtime.identityString("/captures/description/connector_id")
	if err != nil {
		return transcriptIdentity{}, err
	}
	accountID, err := runtime.identityString("/captures/description/account_identity")
	if err != nil {
		return transcriptIdentity{}, err
	}
	state, err := runtime.fixture.ExternalState(ctx)
	if err != nil {
		return transcriptIdentity{}, fmt.Errorf("read provider state: %w", err)
	}
	return transcriptIdentity{
		connectorID: connectorID,
		accountID:   accountID,
		rootLocator: runtime.fixture.RootLocator(),
		rootKey:     state.Root.Key,
	}, nil
}

func (runtime *transcriptRuntime) identityString(path string) (string, error) {
	value, err := runtime.resolve(transcriptValue{Path: path})
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("identity at %q is %T, want nonempty string", path, value)
	}
	return text, nil
}

func loadProtocolV1Transcripts() ([]protocolTranscript, error) {
	paths, err := fs.Glob(protocolV1TranscriptFiles, "testdata/protocol-v1/*.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	transcripts := make([]protocolTranscript, 0, len(paths))
	for _, path := range paths {
		encoded, err := protocolV1TranscriptFiles.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		var transcript protocolTranscript
		if err := decoder.Decode(&transcript); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		if err := requireTranscriptEOF(decoder); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		if transcript.Schema != protocolTranscriptSchema || transcript.Protocol != connector.ProtocolVersion ||
			strings.TrimSpace(transcript.Name) == "" || len(transcript.Steps) == 0 {
			return nil, fmt.Errorf("%s: invalid schema, protocol, name, or empty steps", path)
		}
		if err := validateTranscriptSteps(transcript.Steps); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		transcripts = append(transcripts, transcript)
	}
	if len(transcripts) < 3 {
		return nil, fmt.Errorf("protocol-v1 transcript count = %d, want at least 3", len(transcripts))
	}
	return transcripts, nil
}

func validateTranscriptSteps(steps []protocolTranscriptStep) error {
	for index, step := range steps {
		if strings.TrimSpace(step.Name) == "" {
			return fmt.Errorf("step %d lacks a name", index)
		}
		actions := 0
		if len(step.Request) != 0 {
			actions++
		}
		if step.RawRequest != "" {
			actions++
		}
		if step.Observe != "" {
			actions++
		}
		if step.Fault != "" {
			actions++
			if step.Fault != FaultPublishCommentCrashAfterMutation {
				return fmt.Errorf("step %q declares unsupported fault %q", step.Name, step.Fault)
			}
		}
		if step.MutateComment != nil {
			actions++
			if err := validateTranscriptValue(*step.MutateComment); err != nil {
				return fmt.Errorf("step %q mutate_comment: %w", step.Name, err)
			}
		}
		if step.Repeat != nil {
			actions++
		}
		if actions != 1 {
			return fmt.Errorf("step %q must declare exactly one request, raw_request, observe, fault, mutate_comment, or repeat action", step.Name)
		}
		if len(step.Assert) == 0 {
			return fmt.Errorf("step %q has no executable assertions", step.Name)
		}
		for _, assertion := range append(append([]transcriptAssertion(nil), step.When...), step.Assert...) {
			if err := validateTranscriptAssertion(assertion); err != nil {
				return fmt.Errorf("step %q: %w", step.Name, err)
			}
		}
		for _, capture := range step.Capture {
			if strings.TrimSpace(capture.Name) == "" || strings.Contains(capture.Name, "/") {
				return fmt.Errorf("step %q has an invalid capture name", step.Name)
			}
			if err := validateTranscriptValue(capture.Value); err != nil {
				return fmt.Errorf("step %q capture %q: %w", step.Name, capture.Name, err)
			}
		}
		if step.Repeat != nil {
			if strings.TrimSpace(step.Repeat.As) == "" || strings.Contains(step.Repeat.As, "/") || len(step.Repeat.Steps) == 0 {
				return fmt.Errorf("step %q has an invalid repeat", step.Name)
			}
			if err := validateTranscriptValue(step.Repeat.Over); err != nil {
				return fmt.Errorf("step %q repeat: %w", step.Name, err)
			}
			if err := validateTranscriptSteps(step.Repeat.Steps); err != nil {
				return fmt.Errorf("step %q repeat: %w", step.Name, err)
			}
		}
	}
	return nil
}

func validateTranscriptAssertion(assertion transcriptAssertion) error {
	known := map[string]bool{
		"present": true, "absent": true, "empty": true, "nonempty": true, "equal": true,
		"not_equal": true, "equal_except": true, "type": true, "matches": true, "not_matches": true,
		"trimmed": true, "max_length": true, "no_control": true, "unique": true, "contains": true,
		"not_contains": true, "subset": true, "count": true, "ordered": true, "timestamp": true,
		"time_lte": true, "time_gte": true, "time_gt": true, "time_between": true,
		"all": true, "any": true, "one": true, "entry_equal": true, "entries_equal": true, "array_appended": true,
		"or":         true,
		"valid_utf8": true,
	}
	if !known[assertion.Op] {
		return fmt.Errorf("unsupported generic assertion operation %q", assertion.Op)
	}
	if assertion.Op == "or" {
		if len(assertion.Assert) == 0 {
			return errors.New("or requires nested assertions")
		}
		for _, nested := range assertion.Assert {
			if err := validateTranscriptAssertion(nested); err != nil {
				return fmt.Errorf("or nested assertion: %w", err)
			}
		}
		return nil
	}
	if err := validateTranscriptValue(assertion.Actual); err != nil {
		return fmt.Errorf("%s actual: %w", assertion.Op, err)
	}
	requiresExpected := map[string]bool{
		"equal": true, "not_equal": true, "equal_except": true, "type": true, "max_length": true,
		"contains": true, "not_contains": true, "subset": true, "count": true, "time_lte": true,
		"time_gte": true, "time_gt": true, "entry_equal": true, "entries_equal": true, "array_appended": true,
	}
	if requiresExpected[assertion.Op] {
		if err := validateTranscriptValue(assertion.Expected); err != nil {
			return fmt.Errorf("%s expected: %w", assertion.Op, err)
		}
	}
	if assertion.Op == "present" || assertion.Op == "absent" {
		if assertion.Actual.Path == "" {
			return fmt.Errorf("%s requires a JSON Pointer actual", assertion.Op)
		}
	}
	if assertion.Op == "equal_except" && len(assertion.Paths) == 0 {
		return errors.New("equal_except requires at least one excluded pointer")
	}
	for _, pointer := range assertion.Paths {
		if err := validateJSONPointer(pointer, false); err != nil {
			return fmt.Errorf("%s excluded pointer: %w", assertion.Op, err)
		}
	}
	if assertion.Op == "matches" || assertion.Op == "not_matches" {
		if assertion.Pattern == "" {
			return fmt.Errorf("%s requires a pattern", assertion.Op)
		}
		if _, err := regexp.Compile(assertion.Pattern); err != nil {
			return fmt.Errorf("%s pattern: %w", assertion.Op, err)
		}
	}
	if assertion.Op == "unique" && len(assertion.By) != 1 {
		return errors.New("unique requires exactly one by pointer")
	}
	if assertion.Op == "ordered" && len(assertion.By) == 0 {
		return errors.New("ordered requires at least one by pointer")
	}
	for _, pointer := range assertion.By {
		if err := validateJSONPointer(pointer, true); err != nil {
			return fmt.Errorf("%s by pointer: %w", assertion.Op, err)
		}
	}
	if assertion.Op == "time_between" {
		if err := validateTranscriptValue(assertion.Start); err != nil {
			return fmt.Errorf("time_between start: %w", err)
		}
		if err := validateTranscriptValue(assertion.End); err != nil {
			return fmt.Errorf("time_between end: %w", err)
		}
		if assertion.ToleranceSeconds < 0 {
			return errors.New("time_between tolerance cannot be negative")
		}
	}
	if assertion.Op == "entry_equal" || assertion.Op == "entries_equal" {
		if err := validateTranscriptValue(assertion.Key); err != nil {
			return fmt.Errorf("%s key: %w", assertion.Op, err)
		}
	}
	if assertion.Op == "all" || assertion.Op == "any" || assertion.Op == "one" {
		if strings.TrimSpace(assertion.As) == "" || strings.Contains(assertion.As, "/") || len(assertion.Assert) == 0 {
			return fmt.Errorf("%s requires a loop variable and nested assertions", assertion.Op)
		}
	}
	for _, nested := range assertion.Assert {
		if err := validateTranscriptAssertion(nested); err != nil {
			return fmt.Errorf("%s nested assertion: %w", assertion.Op, err)
		}
	}
	return nil
}

func validateTranscriptValue(value transcriptValue) error {
	if (value.Path == "") == (len(value.Literal) == 0) {
		return errors.New("value must declare exactly one path or literal")
	}
	if value.Path != "" {
		if err := validateJSONPointer(value.Path, false); err != nil {
			return err
		}
	}
	if len(value.Literal) != 0 && !json.Valid(value.Literal) {
		return errors.New("literal is not valid JSON")
	}
	return nil
}

func requireTranscriptEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON")
	}
	return err
}

func (runtime *transcriptRuntime) runSteps(ctx context.Context, t *testing.T, steps []protocolTranscriptStep) {
	t.Helper()
	for _, step := range steps {
		t.Run(step.Name, func(t *testing.T) {
			matches, err := runtime.matchesAll(step.When)
			if err != nil {
				t.Fatalf("evaluate condition: %v", err)
			}
			if !matches {
				t.Skip("declarative condition does not match")
			}
			if err := runtime.performStep(ctx, step); err != nil {
				t.Fatalf("perform action: %v", err)
			}
			for index, assertion := range step.Assert {
				matches, err := runtime.matches(assertion)
				if err != nil {
					t.Fatalf("assertion %d %q: %v", index, assertion.Op, err)
				}
				if !matches {
					actual, _ := runtime.resolve(assertion.Actual)
					expected, _ := runtime.resolve(assertion.Expected)
					t.Fatalf("assertion %d %q did not match: actual=%#v expected=%#v", index, assertion.Op, actual, expected)
				}
			}
			captures := runtime.root["captures"].(map[string]any)
			for _, capture := range step.Capture {
				value, err := runtime.resolve(capture.Value)
				if err != nil {
					t.Fatalf("capture %q: %v", capture.Name, err)
				}
				captures[capture.Name] = cloneJSONValue(value)
			}
			if step.Repeat != nil {
				value, err := runtime.resolve(step.Repeat.Over)
				if err != nil {
					t.Fatalf("resolve repeat: %v", err)
				}
				items, ok := value.([]any)
				if !ok {
					t.Fatalf("repeat value is %T, want array", value)
				}
				variables := runtime.root["vars"].(map[string]any)
				previous, hadPrevious := variables[step.Repeat.As]
				for index, item := range items {
					variables[step.Repeat.As] = item
					t.Run(strconv.Itoa(index), func(t *testing.T) { runtime.runSteps(ctx, t, step.Repeat.Steps) })
				}
				if hadPrevious {
					variables[step.Repeat.As] = previous
				} else {
					delete(variables, step.Repeat.As)
				}
			}
		})
	}
}

func (runtime *transcriptRuntime) performStep(ctx context.Context, step protocolTranscriptStep) error {
	stepDocument := map[string]any{}
	runtime.root["steps"].(map[string]any)[step.Name] = stepDocument
	switch {
	case step.Repeat != nil:
		return nil
	case step.Fault != "":
		if err := runtime.fixture.InjectFault(ctx, step.Fault); err != nil {
			return fmt.Errorf("inject fault %q: %w", step.Fault, err)
		}
		stepDocument["fault"] = string(step.Fault)
		return nil
	case step.MutateComment != nil:
		value, err := runtime.resolve(*step.MutateComment)
		if err != nil {
			return fmt.Errorf("resolve comment mutation target: %w", err)
		}
		commentID, ok := value.(string)
		if !ok || strings.TrimSpace(commentID) == "" {
			return fmt.Errorf("comment mutation target is %T, want nonempty string", value)
		}
		if err := runtime.fixture.MutateComment(ctx, commentID); err != nil {
			return fmt.Errorf("mutate provider comment %q: %w", commentID, err)
		}
		stepDocument["comment_id"] = commentID
		return nil
	case step.Observe != "":
		if step.Observe != "provider_state" {
			return fmt.Errorf("unsupported observation %q", step.Observe)
		}
		state, err := runtime.fixture.ExternalState(ctx)
		if err != nil {
			return fmt.Errorf("read provider state: %w", err)
		}
		comments := state.Comments
		if comments == nil {
			comments = []connector.Comment{}
		}
		mutations := state.Mutations
		if mutations == nil {
			mutations = []Mutation{}
		}
		normalized, err := normalizeTranscriptJSON(map[string]any{
			"root": state.Root, "comments": comments, "fields": state.Fields, "mutations": mutations,
		})
		if err != nil {
			return fmt.Errorf("normalize provider state: %w", err)
		}
		stepDocument["provider"] = normalized
		return nil
	case len(step.Request) != 0 || step.RawRequest != "":
		return runtime.exchangeStep(ctx, step, stepDocument)
	default:
		return errors.New("step has no action")
	}
}

func (runtime *transcriptRuntime) exchangeStep(ctx context.Context, step protocolTranscriptStep, stepDocument map[string]any) error {
	raw := []byte(step.RawRequest)
	if len(step.Request) != 0 {
		request, err := decodeTranscriptJSON(step.Request)
		if err != nil {
			return fmt.Errorf("decode request template: %w", err)
		}
		request, err = runtime.expandTemplate(request)
		if err != nil {
			return fmt.Errorf("expand request template: %w", err)
		}
		if err := runtime.stripUnadvertisedConditionalExpected(request); err != nil {
			return err
		}
		stepDocument["request"] = request
		encoded, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		raw = append(encoded, '\n')
	} else {
		stepDocument["raw_request"] = step.RawRequest
	}
	started := time.Now().UTC()
	output, exchangeErr := runtime.fixture.Exchange(ctx, raw)
	finished := time.Now().UTC()
	exchange := map[string]any{
		"error": exchangeErr != nil, "output": strings.TrimSpace(string(output)),
		"started_at": started.Format(time.RFC3339Nano), "finished_at": finished.Format(time.RFC3339Nano),
	}
	if exchangeErr != nil {
		exchange["error_message"] = exchangeErr.Error()
	}
	var documents []any
	var decodeErr error
	if !utf8.Valid(output) {
		decodeErr = errors.New("connector response is not valid UTF-8")
	} else {
		documents, decodeErr = decodeTranscriptJSONDocuments(output)
	}
	exchange["response_count"] = json.Number(strconv.Itoa(len(documents)))
	exchange["decode_error"] = decodeErr != nil
	if decodeErr != nil {
		exchange["decode_error_message"] = decodeErr.Error()
	}
	stepDocument["exchange"] = exchange
	if len(documents) > 0 {
		stepDocument["response"] = documents[0]
	}
	return nil
}

func decodeTranscriptJSON(encoded []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := requireTranscriptEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeTranscriptJSONDocuments(encoded []byte) ([]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var documents []any
	for {
		var value any
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return documents, err
		}
		documents = append(documents, value)
	}
}

func normalizeTranscriptJSON(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return decodeTranscriptJSON(encoded)
}

func (runtime *transcriptRuntime) expandTemplate(value any) (any, error) {
	switch typed := value.(type) {
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			expanded, err := runtime.expandTemplate(item)
			if err != nil {
				return nil, err
			}
			result[index] = expanded
		}
		return result, nil
	case map[string]any:
		if len(typed) == 1 {
			if path, ok := typed["$ref"].(string); ok {
				resolved, found, err := resolveJSONPointer(runtime.root, path)
				if err != nil {
					return nil, err
				}
				if !found {
					return nil, fmt.Errorf("template reference %q does not exist", path)
				}
				return cloneJSONValue(resolved), nil
			}
			if entries, ok := typed["$entries"].([]any); ok {
				result := make(map[string]any, len(entries))
				for _, entry := range entries {
					entryMap, ok := entry.(map[string]any)
					if !ok {
						return nil, errors.New("$entries item is not an object")
					}
					key, err := runtime.expandTemplate(entryMap["key"])
					if err != nil {
						return nil, err
					}
					keyString, ok := key.(string)
					if !ok || keyString == "" {
						return nil, errors.New("$entries key is not a nonempty string")
					}
					expanded, err := runtime.expandTemplate(entryMap["value"])
					if err != nil {
						return nil, err
					}
					result[keyString] = expanded
				}
				return result, nil
			}
		}
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			expanded, err := runtime.expandTemplate(item)
			if err != nil {
				return nil, err
			}
			result[key] = expanded
		}
		return result, nil
	default:
		return typed, nil
	}
}

func (runtime *transcriptRuntime) matchesAll(assertions []transcriptAssertion) (bool, error) {
	for _, assertion := range assertions {
		matches, err := runtime.matches(assertion)
		if err != nil || !matches {
			return matches, err
		}
	}
	return true, nil
}

func (runtime *transcriptRuntime) matches(assertion transcriptAssertion) (bool, error) {
	if assertion.Op == "or" {
		for _, nested := range assertion.Assert {
			matches, err := runtime.matches(nested)
			if err != nil {
				return false, err
			}
			if matches {
				return true, nil
			}
		}
		return false, nil
	}
	if assertion.Op == "present" || assertion.Op == "absent" {
		_, found, err := resolveJSONPointer(runtime.root, assertion.Actual.Path)
		if err != nil {
			return false, err
		}
		return found == (assertion.Op == "present"), nil
	}
	actual, err := runtime.resolve(assertion.Actual)
	if err != nil {
		return false, err
	}
	switch assertion.Op {
	case "empty", "nonempty":
		empty := transcriptEmpty(actual)
		return empty == (assertion.Op == "empty"), nil
	case "equal", "not_equal":
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		equal := reflect.DeepEqual(actual, expected)
		return equal == (assertion.Op == "equal"), nil
	case "equal_except":
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		left, right := cloneJSONValue(actual), cloneJSONValue(expected)
		for _, path := range assertion.Paths {
			if err := removeJSONPointer(left, path); err != nil {
				return false, err
			}
			if err := removeJSONPointer(right, path); err != nil {
				return false, err
			}
		}
		return reflect.DeepEqual(left, right), nil
	case "type":
		expected, err := runtime.resolve(assertion.Expected)
		return err == nil && transcriptType(actual) == expected, err
	case "matches", "not_matches":
		value, ok := actual.(string)
		if !ok {
			return false, fmt.Errorf("%s actual is %T, want string", assertion.Op, actual)
		}
		pattern, err := regexp.Compile(assertion.Pattern)
		if err != nil {
			return false, err
		}
		matched := pattern.MatchString(value)
		return matched == (assertion.Op == "matches"), nil
	case "trimmed":
		value, ok := actual.(string)
		return ok && strings.TrimSpace(value) == value, nil
	case "valid_utf8":
		value, ok := actual.(string)
		return ok && utf8.ValidString(value), nil
	case "max_length":
		value, ok := actual.(string)
		if !ok {
			return false, fmt.Errorf("max_length actual is %T, want string", actual)
		}
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		maximum, err := transcriptInteger(expected)
		return err == nil && len(value) <= maximum, err
	case "no_control":
		value, ok := actual.(string)
		if !ok {
			return false, fmt.Errorf("no_control actual is %T, want string", actual)
		}
		for _, char := range value {
			if unicode.IsControl(char) {
				return false, nil
			}
		}
		return true, nil
	case "contains", "not_contains":
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		contains, err := transcriptContains(actual, expected)
		return err == nil && contains == (assertion.Op == "contains"), err
	case "subset":
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		items, ok := actual.([]any)
		if !ok {
			return false, fmt.Errorf("subset actual is %T, want array", actual)
		}
		for _, item := range items {
			contains, err := transcriptContains(expected, item)
			if err != nil || !contains {
				return false, err
			}
		}
		return true, nil
	case "count":
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		want, err := transcriptInteger(expected)
		return err == nil && transcriptLength(actual) == want, err
	case "unique":
		items, ok := actual.([]any)
		if !ok || len(assertion.By) != 1 {
			return false, fmt.Errorf("unique requires an array and one by pointer")
		}
		seen := make(map[string]bool, len(items))
		for _, item := range items {
			key, found, err := resolveJSONPointer(item, assertion.By[0])
			if err != nil || !found {
				return false, err
			}
			encoded, _ := json.Marshal(key)
			if seen[string(encoded)] {
				return false, nil
			}
			seen[string(encoded)] = true
		}
		return true, nil
	case "ordered":
		items, ok := actual.([]any)
		if !ok {
			return false, fmt.Errorf("ordered actual is %T, want array", actual)
		}
		for index := 1; index < len(items); index++ {
			comparison, err := compareTranscriptItems(items[index-1], items[index], assertion.By)
			if err != nil {
				return false, err
			}
			if comparison > 0 {
				return false, nil
			}
		}
		return true, nil
	case "timestamp":
		_, err := transcriptTime(actual)
		return err == nil, err
	case "time_lte", "time_gte", "time_gt":
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		left, err := transcriptTime(actual)
		if err != nil {
			return false, err
		}
		right, err := transcriptTime(expected)
		if err != nil {
			return false, err
		}
		if assertion.Op == "time_lte" {
			return !left.After(right), nil
		}
		if assertion.Op == "time_gte" {
			return !left.Before(right), nil
		}
		return left.After(right), nil
	case "time_between":
		startValue, err := runtime.resolve(assertion.Start)
		if err != nil {
			return false, err
		}
		endValue, err := runtime.resolve(assertion.End)
		if err != nil {
			return false, err
		}
		valueTime, err := transcriptTime(actual)
		if err != nil {
			return false, err
		}
		startTime, err := transcriptTime(startValue)
		if err != nil {
			return false, err
		}
		endTime, err := transcriptTime(endValue)
		if err != nil {
			return false, err
		}
		tolerance := time.Duration(assertion.ToleranceSeconds) * time.Second
		return !valueTime.Before(startTime.Add(-tolerance)) && !valueTime.After(endTime.Add(tolerance)), nil
	case "all", "any", "one":
		items, ok := actual.([]any)
		if !ok || assertion.As == "" {
			return false, fmt.Errorf("%s requires an array and loop variable", assertion.Op)
		}
		matchCount := 0
		variables := runtime.root["vars"].(map[string]any)
		previous, hadPrevious := variables[assertion.As]
		for _, item := range items {
			variables[assertion.As] = item
			matched, err := runtime.matchesAll(assertion.Assert)
			if err != nil {
				return false, err
			}
			if matched {
				matchCount++
			}
		}
		if hadPrevious {
			variables[assertion.As] = previous
		} else {
			delete(variables, assertion.As)
		}
		if assertion.Op == "all" {
			return matchCount == len(items), nil
		}
		if assertion.Op == "any" {
			return matchCount > 0, nil
		}
		return matchCount == 1, nil
	case "entry_equal":
		object, ok := actual.(map[string]any)
		if !ok {
			return false, fmt.Errorf("entry_equal actual is %T, want object", actual)
		}
		key, err := runtime.resolve(assertion.Key)
		if err != nil {
			return false, err
		}
		keyString, ok := key.(string)
		if !ok {
			return false, fmt.Errorf("entry_equal key is %T, want string", key)
		}
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		value, found := object[keyString]
		return found && reflect.DeepEqual(value, expected), nil
	case "entries_equal":
		object, ok := actual.(map[string]any)
		if !ok {
			return false, fmt.Errorf("entries_equal actual is %T, want object", actual)
		}
		expected, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		expectedObject, ok := expected.(map[string]any)
		if !ok {
			return false, fmt.Errorf("entries_equal expected is %T, want object", expected)
		}
		key, err := runtime.resolve(assertion.Key)
		if err != nil {
			return false, err
		}
		keyString, ok := key.(string)
		if !ok {
			return false, fmt.Errorf("entries_equal key is %T, want string", key)
		}
		actualValue, actualFound := object[keyString]
		expectedValue, expectedFound := expectedObject[keyString]
		return actualFound && expectedFound && reflect.DeepEqual(actualValue, expectedValue), nil
	case "array_appended":
		after, ok := actual.([]any)
		if !ok {
			return false, fmt.Errorf("array_appended actual is %T, want array", actual)
		}
		beforeValue, err := runtime.resolve(assertion.Expected)
		if err != nil {
			return false, err
		}
		before, ok := beforeValue.([]any)
		if !ok {
			return false, fmt.Errorf("array_appended expected is %T, want array", beforeValue)
		}
		return len(after) == len(before)+1 && reflect.DeepEqual(after[:len(before)], before), nil
	default:
		return false, fmt.Errorf("unsupported operation %q", assertion.Op)
	}
}

func (runtime *transcriptRuntime) resolve(reference transcriptValue) (any, error) {
	if len(reference.Literal) != 0 {
		return decodeTranscriptJSON(reference.Literal)
	}
	value, found, err := resolveJSONPointer(runtime.root, reference.Path)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("JSON Pointer %q does not exist", reference.Path)
	}
	if reference.Last {
		items, ok := value.([]any)
		if !ok || len(items) == 0 {
			return nil, fmt.Errorf("JSON Pointer %q is not a nonempty array", reference.Path)
		}
		return items[len(items)-1], nil
	}
	return value, nil
}

func resolveJSONPointer(document any, pointer string) (any, bool, error) {
	if err := validateJSONPointer(pointer, true); err != nil {
		return nil, false, err
	}
	if pointer == "" {
		return document, true, nil
	}
	current := document
	for rawToken := range strings.SplitSeq(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[token]
			if !ok {
				return nil, false, nil
			}
		case []any:
			index, err := parseJSONPointerArrayIndex(token, pointer)
			if err != nil {
				return nil, false, err
			}
			if index < 0 || index >= len(typed) {
				return nil, false, nil
			}
			current = typed[index]
		default:
			return nil, false, nil
		}
	}
	return current, true, nil
}

func validateJSONPointer(pointer string, allowRoot bool) error {
	if pointer == "" {
		if allowRoot {
			return nil
		}
		return errors.New("JSON Pointer cannot select the document root here")
	}
	if !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("path %q is not an RFC 6901 JSON Pointer", pointer)
	}
	for index := 0; index < len(pointer); index++ {
		if pointer[index] != '~' {
			continue
		}
		if index+1 >= len(pointer) || (pointer[index+1] != '0' && pointer[index+1] != '1') {
			return fmt.Errorf("path %q contains an invalid RFC 6901 escape", pointer)
		}
		index++
	}
	return nil
}

func removeJSONPointer(document any, pointer string) error {
	if pointer == "" {
		return errors.New("cannot remove the document root")
	}
	separator := strings.LastIndex(pointer, "/")
	parent, found, err := resolveJSONPointer(document, pointer[:separator])
	if err != nil || !found {
		return err
	}
	token := strings.ReplaceAll(strings.ReplaceAll(pointer[separator+1:], "~1", "/"), "~0", "~")
	switch typed := parent.(type) {
	case map[string]any:
		delete(typed, token)
	case []any:
		index, err := parseJSONPointerArrayIndex(token, pointer)
		if err != nil || index >= len(typed) {
			if err == nil {
				err = fmt.Errorf("JSON Pointer %q has an invalid array index", pointer)
			}
			return err
		}
		typed[index] = nil
	default:
		return fmt.Errorf("pointer parent is not a container")
	}
	return nil
}

func parseJSONPointerArrayIndex(token, pointer string) (int, error) {
	if token != "0" && (token == "" || token[0] == '0') {
		return 0, fmt.Errorf("JSON Pointer %q has a non-canonical array index", pointer)
	}
	for _, char := range token {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("JSON Pointer %q has a non-numeric array index", pointer)
		}
	}
	index, err := strconv.Atoi(token)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("JSON Pointer %q has an invalid array index", pointer)
	}
	return index, nil
}

func cloneJSONValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	cloned, err := decodeTranscriptJSON(encoded)
	if err != nil {
		return value
	}
	return cloned
}

func transcriptEmpty(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func transcriptType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func transcriptContains(container, candidate any) (bool, error) {
	switch typed := container.(type) {
	case []any:
		for _, item := range typed {
			if reflect.DeepEqual(item, candidate) {
				return true, nil
			}
		}
		return false, nil
	case string:
		value, ok := candidate.(string)
		if !ok {
			return false, fmt.Errorf("string containment candidate is %T", candidate)
		}
		return strings.Contains(typed, value), nil
	default:
		return false, fmt.Errorf("contains actual is %T, want array or string", container)
	}
}

func transcriptLength(value any) int {
	switch typed := value.(type) {
	case string:
		return len(typed)
	case []any:
		return len(typed)
	case map[string]any:
		return len(typed)
	default:
		return -1
	}
}

func transcriptInteger(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("value is %T, want number", value)
	}
	integer, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, err
	}
	return integer, nil
}

func transcriptTime(value any) (time.Time, error) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("timestamp is %T, want string", value)
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", text, err)
	}
	return parsed, nil
}

func compareTranscriptItems(left, right any, paths []string) (int, error) {
	for _, path := range paths {
		leftValue, leftFound, err := resolveJSONPointer(left, path)
		if err != nil || !leftFound {
			return 0, fmt.Errorf("ordered left value at %q is unavailable", path)
		}
		rightValue, rightFound, err := resolveJSONPointer(right, path)
		if err != nil || !rightFound {
			return 0, fmt.Errorf("ordered right value at %q is unavailable", path)
		}
		leftString, leftOK := leftValue.(string)
		rightString, rightOK := rightValue.(string)
		if !leftOK || !rightOK {
			return 0, fmt.Errorf("ordered values at %q are not strings", path)
		}
		leftTime, leftTimeErr := time.Parse(time.RFC3339Nano, leftString)
		rightTime, rightTimeErr := time.Parse(time.RFC3339Nano, rightString)
		if leftTimeErr == nil && rightTimeErr == nil {
			if leftTime.Before(rightTime) {
				return -1, nil
			}
			if leftTime.After(rightTime) {
				return 1, nil
			}
			continue
		}
		if leftString < rightString {
			return -1, nil
		}
		if leftString > rightString {
			return 1, nil
		}
	}
	return 0, nil
}

func (runtime *transcriptRuntime) stripUnadvertisedConditionalExpected(request any) error {
	object, ok := request.(map[string]any)
	if !ok || object["method"] != "write_fields" {
		return nil
	}
	params, ok := object["params"].(map[string]any)
	if !ok {
		return nil
	}
	if _, conditional := params["expected"]; !conditional {
		return nil
	}
	value, found, err := resolveJSONPointer(runtime.root, "/captures/description/capabilities")
	if err != nil {
		return fmt.Errorf("resolve advertised connector capabilities: %w", err)
	}
	capabilities, ok := value.([]any)
	if !found || !ok {
		return errors.New("captured connector description is missing capabilities")
	}
	for _, capability := range capabilities {
		if capability == string(connector.CapabilityConditionalFields) {
			return nil
		}
	}
	delete(params, "expected")
	return nil
}
