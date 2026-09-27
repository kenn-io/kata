package jsonutil

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var numberLiteralCases = []struct {
	name, literal string
}{
	{"integer above float64 precision", "9007199254740993"},
	{"negative integer below int64", "-9223372036854775809"},
	{"fraction scale", "10.00"},
	{"exponent spelling", "1E+003"},
	{"negative zero", "-0"},
	{"negative zero scale", "-0.00"},
	{"large exponent", "1e1000"},
	{"small exponent", "1e-1000"},
	{"zero", "0"},
}

func TestPreserveNumberLiterals_Representation(t *testing.T) {
	for _, tc := range numberLiteralCases {
		t.Run(tc.name, func(t *testing.T) {
			checkNumberLiteral(t, tc.literal)
		})
	}
}

func checkNumberLiteral(t *testing.T, literal string) {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal([]byte(literal), &value, PreserveNumberLiterals()))
	raw, ok := value.(jsontext.Value)
	require.True(t, ok, "numeric value must retain its raw token type, got %T", value)
	assert.Equal(t, literal, string(raw))
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	assert.Equal(t, literal, string(encoded))
}

func TestPreserveNumberLiterals_Fallback(t *testing.T) {
	var value any
	require.NoError(t, json.Unmarshal([]byte(`{"text":"10","yes":true,"no":false,"empty":null,"nested":[9007199254740993,"text",true,null,{"negative":-0.00}]}`), &value, PreserveNumberLiterals()))
	assert.Equal(t, map[string]any{
		"text": "10", "yes": true, "no": false, "empty": nil,
		"nested": []any{jsontext.Value("9007199254740993"), "text", true, nil, map[string]any{"negative": jsontext.Value("-0.00")}},
	}, value)
	var typed struct {
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"count":12}`), &typed, PreserveNumberLiterals()))
	assert.Equal(t, 12, typed.Count)
}

func TestPreserveNumberLiterals_InvalidJSON(t *testing.T) {
	for _, data := range []string{`{`, `[1,]`, `0e`, `1 2`} {
		t.Run(data, func(t *testing.T) {
			var value any
			require.Error(t, json.Unmarshal([]byte(data), &value, PreserveNumberLiterals()))
		})
	}
}

func TestPreserveNumberLiterals_StreamingPolicy(t *testing.T) {
	decoder := jsontext.NewDecoder(strings.NewReader("9007199254740993 true null"))
	var number, boolean, null any
	require.NoError(t, json.UnmarshalDecode(decoder, &number, PreserveNumberLiterals()))
	require.NoError(t, json.UnmarshalDecode(decoder, &boolean, PreserveNumberLiterals()))
	require.NoError(t, json.UnmarshalDecode(decoder, &null, PreserveNumberLiterals()))
	assert.Equal(t, jsontext.Value("9007199254740993"), number)
	assert.Equal(t, true, boolean)
	assert.Nil(t, null)
	var trailing any
	require.ErrorIs(t, json.UnmarshalDecode(decoder, &trailing, PreserveNumberLiterals()), io.EOF)
}

func TestPreserveNumberLiterals_OwnsInputTokens(t *testing.T) {
	data := []byte(`[9007199254740993,10.0,-0]`)
	var values []any
	require.NoError(t, json.Unmarshal(data, &values, PreserveNumberLiterals()))
	for index := range data {
		data[index] = ' '
	}
	assert.Equal(t, []any{jsontext.Value("9007199254740993"), jsontext.Value("10.0"), jsontext.Value("-0")}, values)
}

func TestPreserveNumberLiterals_OwnsDecoderTokens(t *testing.T) {
	input := `[9007199254740993,10.0,-0]` + "\n" + strings.Repeat(`{"padding":"`+strings.Repeat("x", 4096)+`"}`+"\n", 8)
	decoder := jsontext.NewDecoder(strings.NewReader(input))
	var values []any
	require.NoError(t, json.UnmarshalDecode(decoder, &values, PreserveNumberLiterals()))
	for {
		var next any
		err := json.UnmarshalDecode(decoder, &next, PreserveNumberLiterals())
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	assert.Equal(t, []any{jsontext.Value("9007199254740993"), jsontext.Value("10.0"), jsontext.Value("-0")}, values)
}

func FuzzPreserveNumberLiterals(f *testing.F) {
	for _, tc := range numberLiteralCases {
		f.Add(tc.literal)
	}
	f.Fuzz(func(t *testing.T, input string) {
		token := strings.TrimSpace(input)
		if len(token) == 0 || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) || !jsontext.Value(token).IsValid() {
			t.Skip()
		}
		checkNumberLiteral(t, token)
	})
}
