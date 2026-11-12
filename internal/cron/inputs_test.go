package cron

import (
	"encoding/json/jsontext"
	"math"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestInputResolution(t *testing.T) {
	job := JobDefinition{Action: Action{Prompt: "hello"}, Options: jsontext.Value(`{"inputs":{"count":{"type":"int","min":1,"max":9,"default":2},"needed":{"type":"bool","required":true}},"input_values":{"count":4}}`)}
	result := ResolveInputs(job, nil)
	require.Empty(t, ValidateInputs(job.Options, job.Action.WorkflowUID, nil))
	require.Equal(t, "job", result.DeclaredBy)
	require.Equal(t, "count", result.Inputs[0].Name)
	require.Equal(t, "job", result.Inputs[0].Source)
	require.Equal(t, "missing_required", result.Errors[0].Code)
	job.Options = jsontext.Value(`{"inputs":42}`)
	require.Equal(t, "declaration_invalid", ResolveInputs(job, nil).Errors[0].Code)
	require.Error(t, ValidateInputs(job.Options, "", nil))
}

func TestInputStringProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		limit := hegel.Draw(ht, hegel.Integers[int64](1, 2048))
		multiline := hegel.Draw(ht, hegel.Booleans())
		want := int64(len(value)) <= limit
		if !multiline {
			for _, r := range value {
				if unicode.IsControl(r) {
					want = false
				}
			}
		}
		if validInputValue(inputDeclaration{Type: "string", MaxLength: &limit, Multiline: multiline}, value) != want {
			ht.Fatalf("string %q, limit %d, multiline %v", value, limit, multiline)
		}
	}, hegel.WithTestCases(1000))
}
func TestInputIntegerProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		a := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		b := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		if a > b {
			a, b = b, a
		}
		if validInputValue(inputDeclaration{Type: "int", Min: &a, Max: &b}, n) != (n >= a && n <= b) {
			ht.Fatalf("int %d outside %d..%d", n, a, b)
		}
	}, hegel.WithTestCases(1000))
}
func TestInputBoolProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b := hegel.Draw(ht, hegel.Booleans())
		if !validInputValue(inputDeclaration{Type: "bool"}, b) {
			ht.Fatalf("bool %v", b)
		}
		s := hegel.Draw(ht, hegel.Text())
		if validInputValue(inputDeclaration{Type: "bool"}, s) {
			ht.Fatalf("string %q accepted as bool", s)
		}
	}, hegel.WithTestCases(1000))
}
func TestInputEnumProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		v := hegel.Draw(ht, hegel.Text())
		member := hegel.Draw(ht, hegel.Text())
		if validInputValue(inputDeclaration{Type: "enum", Values: []string{member}}, v) != (v == member) {
			ht.Fatalf("enum %q member %q", v, member)
		}
	}, hegel.WithTestCases(1000))
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

func TestInputResolutionReportsEveryInvalidDeclaration(t *testing.T) {
	job := JobDefinition{Action: Action{Prompt: "Review"}, Options: jsontext.Value(`{"inputs":{"first":{"type":"bad"},"second":{"type":"bad"}}}`)}
	result := ResolveInputs(job, nil)
	require.Len(t, result.Errors, 2)
	require.Equal(t, "first", result.Errors[0].Name)
	require.Equal(t, "second", result.Errors[1].Name)
}
