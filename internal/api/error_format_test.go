// Internal tests for Kata's Huma error conversion.
//
//nolint:revive // package-name lint flagged externally; internal test needs the package name
package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
)

func humaErr(loc, msg string) error {
	return &huma.ErrorDetail{Location: loc, Message: msg}
}

// TestFoldDetailsIntoMessage covers hammer-test finding #11: a
// validation failure used to surface "validation failed" with no
// detail because InstallErrorFormatter dropped huma's per-field
// errs slice. Now folds up to 3 details into the message in
// "field: reason" form so close --reason banana, list --status
// nonsense, etc. give the user actionable feedback.
func TestFoldDetailsIntoMessage(t *testing.T) {
	tests := []struct {
		name     string
		errs     []error
		expected string   // exact match when non-empty
		contains []string // substrings that must appear in output
	}{
		{
			name:     "no details returns base message",
			errs:     nil,
			expected: "validation failed",
		},
		{
			name:     "ErrorDetailer with location surfaces field name",
			errs:     []error{humaErr("body.reason", "expected one of done, wontfix, duplicate")},
			expected: "validation failed: reason: expected one of done, wontfix, duplicate",
		},
		{
			name: "path. and query. prefixes also stripped",
			errs: []error{
				humaErr("query.status", "expected enum value"),
				humaErr("path.id", "must be integer"),
			},
			contains: []string{"status: expected enum value", "id: must be integer"},
		},
		{
			name:     "plain error falls back to .Error()",
			errs:     []error{errors.New("custom")},
			expected: "validation failed: custom",
		},
		{
			name: "more than three details gets and-N-more suffix",
			errs: []error{
				humaErr("body.a", "x"),
				humaErr("body.b", "x"),
				humaErr("body.c", "x"),
				humaErr("body.d", "x"),
				humaErr("body.e", "x"),
			},
			contains: []string{"(and 2 more)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := foldDetailsIntoMessage("validation failed", tt.errs)
			if tt.expected != "" {
				assert.Equal(t, tt.expected, got)
			}
			for _, sub := range tt.contains {
				assert.Contains(t, got, sub)
			}
		})
	}
}

func TestAPIErrorFromHuma_FoldsDetails(t *testing.T) {
	apiErr := apiErrorFromHuma(&huma.ErrorModel{
		Status: http.StatusUnprocessableEntity,
		Detail: "validation failed",
		Errors: []*huma.ErrorDetail{
			{Location: "body.reason", Message: "must be one of done, wontfix"},
		},
	})
	assert.Equal(t, 400, apiErr.Status)
	assert.Equal(t, "validation", apiErr.Code)
	assert.Contains(t, apiErr.Message, "reason: must be one of done, wontfix",
		"the per-field detail must reach the wire envelope")
}
