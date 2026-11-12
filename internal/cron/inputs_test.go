package cron

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestInputResolution(t *testing.T) {
	job := JobDefinition{Action: Action{Prompt: "hello"}, Options: jsontext.Value(`{"inputs":{"count":{"type":"int","min":1,"max":9,"default":2},"needed":{"type":"bool","required":true}},"input_values":{"count":4}}`)}
	result := ResolveInputs(job, nil)
	require.NoError(t, ValidateInputs(job.Options, job.Action.WorkflowUID, nil))
	require.Equal(t, "job", result.DeclaredBy)
	require.Equal(t, "count", result.Inputs[0].Name)
	require.Equal(t, "job", result.Inputs[0].Source)
	require.Len(t, result.Errors, 1)
	require.Equal(t, "missing_required", result.Errors[0].Code)
	require.Equal(t, "unset", result.Inputs[1].Source)
	job.Options = jsontext.Value(`{"inputs":{"a":{"type":"string","default":"fallback"},"b":{"type":"bool"}}}`)
	defaults := ResolveInputs(job, nil)
	require.Len(t, defaults.Inputs, 2)
	require.Equal(t, "a", defaults.Inputs[0].Name)
	require.Equal(t, "default", defaults.Inputs[0].Source)
	require.Equal(t, "fallback", defaults.Inputs[0].Value)
	require.Equal(t, "unset", defaults.Inputs[1].Source)
	job.Options = jsontext.Value(`{"inputs":42}`)
	malformed := ResolveInputs(job, nil)
	require.Len(t, malformed.Errors, 1)
	require.Equal(t, "declaration_invalid", malformed.Errors[0].Code)
	require.Error(t, ValidateInputs(job.Options, "", nil))
}

func TestInputStringProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[int](1, 300))
		value := strings.Repeat("x", n)
		raw, _ := json.Marshal(map[string]any{"inputs": map[string]any{"text": map[string]any{"type": "string"}}, "input_values": map[string]any{"text": value}})
		if (ValidateInputs(raw, "", nil) == nil) != (n <= 256) {
			ht.Fatalf("length %d", n)
		}
	}, hegel.WithTestCases(300))
	for _, tc := range []struct {
		value            string
		multiline, valid bool
	}{{"line\nbreak", false, false}, {"line\nbreak", true, true}, {"plain", false, true}} {
		raw, err := json.Marshal(map[string]any{"inputs": map[string]any{"text": map[string]any{"type": "string", "max_length": 10, "multiline": tc.multiline}}, "input_values": map[string]any{"text": tc.value}})
		require.NoError(t, err)
		require.Equal(t, tc.valid, ValidateInputs(raw, "", nil) == nil)
	}
}
func TestInputIntegerProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		a := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		b := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		if a > b {
			a, b = b, a
		}
		raw, err := json.Marshal(map[string]any{"inputs": map[string]any{"number": map[string]any{"type": "int", "min": a, "max": b}}, "input_values": map[string]any{"number": json.Number(strconv.FormatInt(n, 10))}})
		if err != nil {
			ht.Fatalf("marshal: %v", err)
		}
		if (ValidateInputs(raw, "", nil) == nil) != (n >= a && n <= b) {
			ht.Fatalf("int %d outside %d..%d", n, a, b)
		}
	}, hegel.WithTestCases(1000))
}

func TestInputEnumProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[int](1, 64))
		members := make([]string, n)
		for i := range members {
			members[i] = strconv.Itoa(i)
		}
		selected := hegel.Draw(ht, hegel.Integers[int](0, n-1))
		options := func(value string) []byte {
			raw, err := json.Marshal(map[string]any{"inputs": map[string]any{"choice": map[string]any{"type": "enum", "values": members}}, "input_values": map[string]any{"choice": value}})
			if err != nil {
				ht.Fatalf("marshal: %v", err)
			}
			return raw
		}
		if ValidateInputs(options(members[selected]), "", nil) != nil || ValidateInputs(options("outside"), "", nil) == nil {
			ht.Fatalf("enum membership for %v", members)
		}
	}, hegel.WithTestCases(300))
}

func TestInputWorkflowDeclarationAndLegacyValues(t *testing.T) {
	require.NoError(t, ValidateWorkflowInputs([]byte(`{"inputs":{"target":{"type":"enum","values":["staging"]}},"input_values":"legacy"}`)))
	require.Error(t, ValidateWorkflowInputs([]byte(`{"inputs":{"target":{"type":"int","required":null}}}`)))
	job := JobDefinition{Action: Action{WorkflowUID: "01J00000000000000000000020"}, Options: jsontext.Value(`{"input_values":{"target":"production"}}`)}
	workflow := WorkflowDefinition{Options: jsontext.Value(`{"inputs":{"target":{"type":"enum","values":["staging"]}}}`)}
	result := ResolveInputs(job, &workflow)
	require.Equal(t, "workflow", result.DeclaredBy)
	require.Equal(t, "invalid_value", result.Errors[0].Code)
	require.Error(t, ValidateInputs(job.Options, job.Action.WorkflowUID, workflow.Options))
}

func TestInputResolutionDiagnosticsContract(t *testing.T) {
	job := JobDefinition{Action: Action{Prompt: "hello"}, Options: jsontext.Value(`{"inputs":{"bad":{"type":"bad"},"count":{"type":"int"},"needed":{"type":"bool","required":true}},"input_values":{"count":{"bad":true},"extra":1}}`)}
	r := ResolveInputs(job, nil)
	require.Len(t, r.Errors, 4)
	require.Nil(t, r.Inputs[0].Value)
	job.Options = jsontext.Value(`{"inputs":{"needed":{"type":"bool","required":true}},"input_values":42}`)
	r = ResolveInputs(job, nil)
	require.Len(t, r.Errors, 2)
	require.Len(t, r.Inputs, 1)
	job.Options = jsontext.Value(`{"inputs":{"first":{"type":"bad"},"second":{"type":"bad"}}}`)
	r = ResolveInputs(job, nil)
	require.Len(t, r.Errors, 2)
	require.Equal(t, "first", r.Errors[0].Name)
	require.Equal(t, "second", r.Errors[1].Name)
	job.Action = Action{Kind: "notify"}
	r = ResolveInputs(job, nil)
	require.NotEmpty(t, r.Errors)
	require.Equal(t, "declaration_invalid", r.Errors[0].Code)
}

func TestInputResolutionLegacyNotifyIndependentDiagnostics(t *testing.T) {
	job := JobDefinition{Action: Action{Kind: "notify"}, Options: jsontext.Value(`{"inputs":{"bad":{"type":"bad"}},"input_values":42}`)}
	r := ResolveInputs(job, nil)
	require.Len(t, r.Errors, 3)
	var eligibility, declaration, values bool
	for _, problem := range r.Errors {
		eligibility = eligibility || problem.Code == "declaration_invalid" && strings.Contains(problem.Message, "notify jobs cannot use inputs or input_values")
		declaration = declaration || problem.Code == "declaration_invalid" && problem.Name == "bad" && strings.Contains(problem.Message, "invalid type")
		values = values || problem.Code == "invalid_value" && strings.Contains(problem.Message, "input_values must be an object")
	}
	require.True(t, eligibility, "missing eligibility diagnostic")
	require.True(t, declaration, "missing malformed declaration diagnostic")
	require.True(t, values, "missing invalid values object diagnostic")
}

func TestInputMissingWorkflowAndUnfixedJob(t *testing.T) {
	job := JobDefinition{Action: Action{WorkflowUID: "01J00000000000000000000020"}, Options: jsontext.Value(`{}`)}
	require.NoError(t, ValidateInputs(job.Options, job.Action.WorkflowUID, []byte(`{"inputs":42}`)))
	require.NotEmpty(t, ResolveInputs(job, nil).Errors)
	job.Options = jsontext.Value(`{"input_values":{"x":1}}`)
	require.Error(t, ValidateInputs(job.Options, job.Action.WorkflowUID, []byte(`{"inputs":42}`)))
}

func TestInputDeclarationRules(t *testing.T) {
	cases := []string{
		`{"Bad":{"type":"bool"}}`, `{"token":{"type":"bool"}}`,
		`{"x":{"type":"int","min":2,"max":1}}`, `{"x":{"type":"enum","values":["a","a"]}}`,
		`{"x":{"type":"string","max_length":0}}`, `{"x":{"type":"string","max_length":2049}}`,
		`{"x":{"type":"bool","min":0}}`, `{"x":{"type":"bool","unknown":true}}`,
		`{"x":{"type":"bool","default":"true"}}`,
		`{"x":{"type":"int","Default":"wrong"}}`, `{"x":{"type":"bool","Min":1}}`, `{"x":{"TYPE":"bool"}}`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) { require.Error(t, ValidateWorkflowInputs([]byte(`{"inputs":`+input+`}`))) })
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{{`true`, true}, {`false`, true}, {`"true"`, false}} {
		options := []byte(`{"inputs":{"x":{"type":"bool"}},"input_values":{"x":` + tc.value + `}}`)
		require.Equal(t, tc.valid, ValidateInputs(options, "", nil) == nil)
	}
	fields := map[string]any{}
	for i := range 33 {
		fields["x"+strconv.Itoa(i)] = map[string]any{"type": "bool"}
	}
	raw, err := json.Marshal(map[string]any{"inputs": fields})
	require.NoError(t, err)
	require.Error(t, ValidateWorkflowInputs(raw))
	for _, field := range []struct {
		name   string
		length int
	}{{"title", 81}, {"description", 401}} {
		raw, err := json.Marshal(map[string]any{"inputs": map[string]any{"x": map[string]any{"type": "bool", field.name: strings.Repeat("x", field.length)}}})
		require.NoError(t, err)
		require.Error(t, ValidateWorkflowInputs(raw))
	}
}

// TestInputNULDeliveryContract keeps every accepted value representable in an environment.
func TestInputNULDeliveryContract(t *testing.T) {
	for _, options := range []string{
		`{"inputs":{"text":{"type":"string","multiline":true,"default":"a\u0000b"}}}`,
		`{"inputs":{"text":{"type":"string","multiline":true}},"input_values":{"text":"a\u0000b"}}`,
		`{"inputs":{"choice":{"type":"enum","values":["a\u0000b"]}}}`,
	} {
		require.Error(t, ValidateInputs([]byte(options), "", nil), options)
	}
	valid := []byte(`{"inputs":{"text":{"type":"string","multiline":true}},"input_values":{"text":"\n\t\u0001"}}`)
	require.NoError(t, ValidateInputs(valid, "", nil))
	job := JobDefinition{Action: Action{Prompt: "hello"}, Options: jsontext.Value(`{"inputs":{"text":{"type":"string","multiline":true}},"input_values":{"text":"a\u0000b"}}`)}
	r := ResolveInputs(job, nil)
	require.Equal(t, "invalid_value", r.Errors[0].Code)
	require.Equal(t, "job", r.Inputs[0].Source)
	require.Nil(t, r.Inputs[0].Value)
}

// TestInputLegacyOverLimitDiagnostics retains each declared name and independent error.
func TestInputLegacyOverLimitDiagnostics(t *testing.T) {
	fields := map[string]any{}
	for i := range 33 {
		fields[fmt.Sprintf("item_%02d", i)] = map[string]any{"type": "bool"}
	}
	fields["item_00"] = map[string]any{"type": "invalid"}
	raw, err := json.Marshal(map[string]any{"inputs": fields, "input_values": map[string]any{"item_00": true, "item_01": true}})
	require.NoError(t, err)
	r := ResolveInputs(JobDefinition{Action: Action{Prompt: "hello"}, Options: raw}, nil)
	require.Len(t, r.Inputs, 32)
	require.Equal(t, "job", r.Inputs[0].Source)
	require.Len(t, r.Errors, 2)
	require.Equal(t, "declaration_invalid", r.Errors[0].Code)
	require.Equal(t, "item_00", r.Errors[1].Name)
	require.Equal(t, "declaration_invalid", r.Errors[1].Code)
}
