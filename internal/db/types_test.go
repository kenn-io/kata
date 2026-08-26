package db_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

func TestIssue_HasShortIDFieldAndNoNumber(t *testing.T) {
	typ := reflect.TypeFor[db.Issue]()
	_, hasShortID := typ.FieldByName("ShortID")
	_, hasNumber := typ.FieldByName("Number")
	assert.True(t, hasShortID, "Issue.ShortID should exist")
	assert.False(t, hasNumber, "Issue.Number should be removed")
}

// TestJSONStringArrayRoundTripsVerbatim pins that the array-shaped storage
// blob behaves exactly like JSONBlob on the wire: the stored bytes go out
// verbatim (not as a JSON-encoded string), and JSON null comes back as the
// empty value so unmarshal/marshal is idempotent.
func TestJSONStringArrayRoundTripsVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  db.JSONStringArray
		want    string
		wantVal db.JSONStringArray
	}{
		{name: "labels", stored: `["p2","recurring"]`, want: `["p2","recurring"]`, wantVal: `["p2","recurring"]`},
		{name: "empty array", stored: `[]`, want: `[]`, wantVal: `[]`},
		{name: "zero value", stored: "", want: `null`, wantVal: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.stored)
			assert.NoError(t, err)
			assert.Equal(t, tc.want, string(raw))

			var back db.JSONStringArray
			assert.NoError(t, json.Unmarshal(raw, &back))
			assert.Equal(t, tc.wantVal, back)
		})
	}
}

// TestRecurrenceTemplateLabelsMarshalAsArray pins that the recurrence row
// publishes template_labels as a JSON array and template_metadata as a JSON
// object — the two shapes the daemon used to reconstruct by property name.
func TestRecurrenceTemplateLabelsMarshalAsArray(t *testing.T) {
	raw, err := json.Marshal(db.Recurrence{
		TemplateLabels:   `["p2","recurring"]`,
		TemplateMetadata: `{"area":"example-workspace"}`,
	})
	assert.NoError(t, err)

	var decoded struct {
		TemplateLabels   []string       `json:"template_labels"`
		TemplateMetadata map[string]any `json:"template_metadata"`
	}
	assert.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, []string{"p2", "recurring"}, decoded.TemplateLabels)
	assert.Equal(t, map[string]any{"area": "example-workspace"}, decoded.TemplateMetadata)
}
