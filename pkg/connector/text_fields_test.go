package connector

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestPortableTextFieldWireValidation(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		valid      bool
	}{
		{"exact text", `{"kind":"string","value":"  café\n東京  "}`, true},
		{"empty text", `{"kind":"string"}`, true},
		{"set", `{"kind":"string_set","values":[""," A ","café","東京"]}`, true},
		{"empty set", `{"kind":"string_set"}`, true},
		{"unsorted", `{"kind":"string_set","values":["z","a"]}`, false},
		{"duplicate", `{"kind":"string_set","values":["a","a"]}`, false},
		{"set scalar", `{"kind":"string_set","value":"a"}`, false},
		{"set zone", `{"kind":"string_set","timezone":"UTC"}`, false},
		{"text set", `{"kind":"string","values":["a"]}`, false},
		{"text zone", `{"kind":"string","timezone":"UTC"}`, false},
		{"text NUL", `{"kind":"string","value":"\u0000"}`, false},
		{"set NUL", `{"kind":"string_set","values":["\u0000"]}`, false},
		{"date set", `{"kind":"date","value":"2026-08-21","values":["a"]}`, false},
		{"null set", `{"kind":"null","values":["a"]}`, false},
		{"unknown", `{"kind":"boolean","value":"true"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value FieldValue
			require.NoError(t, json.Unmarshal([]byte(tc.wire), &value))
			err := ValidateFieldValue(value)
			if tc.valid {
				require.NoError(t, err)
				encoded, e := json.Marshal(value)
				require.NoError(t, e)
				assert.JSONEq(t, tc.wire, string(encoded))
			} else {
				require.Error(t, err)
			}
		})
	}
	require.Error(t, ValidateFieldValue(FieldValue{Kind: "string", Value: string([]byte{0xff})}))
}

func TestServeOneAcceptsPortableTextFields(t *testing.T) {
	for _, value := range []string{`{"kind":"string","value":"  café\n東京  "}`, `{"kind":"string"}`, `{"kind":"string_set","values":["","a","東京"]}`, `{"kind":"string_set"}`} {
		handler := &writeFieldsCallHandler{}
		var output bytes.Buffer
		err := ServeOne(t.Context(), bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"text","method":"write_fields","instance":"example-instance","settings":{},"params":{"root_key":"root-1","fields":{"field-1":`+value+`}}}`), &output, handler)
		require.NoError(t, err)
		require.True(t, handler.called)
	}
}

func TestCanonicalizeFieldValueCopiesSet(t *testing.T) {
	original := FieldValue{Kind: "string_set", Values: []string{"東京", "a", "a", "", " A "}}
	got, err := CanonicalizeFieldValue(original)
	require.NoError(t, err)
	assert.Equal(t, []string{"", " A ", "a", "東京"}, got.Values)
	assert.Equal(t, []string{"東京", "a", "a", "", " A "}, original.Values)
	again, err := CanonicalizeFieldValue(got)
	require.NoError(t, err)
	assert.Equal(t, got, again)
	got.Values[0] = "changed"
	assert.Equal(t, "東京", original.Values[0])
	assert.Equal(t, "", again.Values[0])
	_, err = CanonicalizeFieldValue(FieldValue{Kind: "string_set", Values: []string{string([]byte{0xff})}})
	require.Error(t, err)
	_, err = CanonicalizeFieldValue(FieldValue{Kind: "string_set", Values: []string{"a\x00"}})
	require.Error(t, err)
}

func TestScalarFieldsRejectEmptySetPayload(t *testing.T) {
	for _, value := range []FieldValue{{Kind: "string", Values: []string{}}, {Kind: "null", Values: []string{}}, {Kind: "date", Value: "2026-08-21", Values: []string{}}} {
		require.Error(t, ValidateFieldValue(value))
	}
}

func TestServeOneRejectsMalformedPortableFields(t *testing.T) {
	for _, value := range []string{`{"kind":"string_set","values":["z","a"]}`, `{"kind":"string_set","values":["a","a"]}`, `{"kind":"string_set","value":"a"}`, `{"kind":"string","values":["a"]}`, `{"kind":"string","timezone":"UTC"}`, `{"kind":"string","value":"\u0000"}`, `{"kind":"unknown"}`, `{"kind":"string_set","values":["\u0000"]}`} {
		for _, field := range []string{"fields", "expected"} {
			payload := `"fields":{"field-1":{"kind":"string"}},"expected":{"field-1":` + value + `}`
			if field == "fields" {
				payload = `"fields":{"field-1":` + value + `}`
			}
			handler := &writeFieldsCallHandler{}
			var output bytes.Buffer
			err := ServeOne(t.Context(), bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"bad-text","method":"write_fields","instance":"example-instance","settings":{},"params":{"root_key":"root-1",`+payload+`}}`), &output, handler)
			require.Error(t, err)
			require.False(t, handler.called)
			require.Empty(t, output.Bytes())
		}
	}
}

// Arbitrary bytes exercise the acceptance and rejection directions of the
// portable text contract, including invalid UTF-8 and NUL.
func TestPortableTextValidationProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := string(rapid.SliceOf(rapid.Byte()).Draw(t, "text bytes"))
		valid := utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
		for _, value := range []FieldValue{{Kind: "string", Value: text}, {Kind: "string_set", Values: []string{text}}} {
			err := ValidateFieldValue(value)
			if (err == nil) != valid {
				t.Fatalf("validation(%#v) = %v, want valid=%v", value, err, valid)
			}
		}
	})
}

func TestCanonicalizeSetMembershipProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := rapid.SliceOf(rapid.String()).Draw(t, "set members")
		members := map[string]bool{}
		valid := true
		for _, text := range input {
			members[text] = true
			if strings.ContainsRune(text, '\x00') {
				valid = false
			}
		}
		got, err := CanonicalizeFieldValue(FieldValue{Kind: "string_set", Values: input})
		if !valid {
			if err == nil {
				t.Fatal("accepted NUL set member")
			}
			return
		}
		if err != nil {
			t.Fatalf("rejected text set: %v", err)
		}
		if len(got.Values) != len(members) {
			t.Fatalf("got %d members, want %d", len(got.Values), len(members))
		}
		for i, text := range got.Values {
			if !members[text] || (i > 0 && got.Values[i-1] >= text) {
				t.Fatalf("noncanonical or altered set: %#v", got.Values)
			}
		}
	})
}

func TestCanonicalizeSetIdempotenceProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		value := FieldValue{Kind: "string_set", Values: rapid.SliceOf(rapid.String()).Draw(t, "set members")}
		first, err := CanonicalizeFieldValue(value)
		if err != nil {
			for _, text := range value.Values {
				if strings.ContainsRune(text, '\x00') {
					return
				}
			}
			t.Fatalf("rejected valid set before idempotence check: %v", err)
		}
		second, err := CanonicalizeFieldValue(first)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("canonicalization changed canonical value: first=%#v second=%#v err=%v", first, second, err)
		}
	})
}
