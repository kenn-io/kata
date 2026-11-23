package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

func TestExpiredAssignmentHandoffPatch(t *testing.T) {
	for _, tc := range []struct {
		name, status, metadata string
		want                   bool
	}{
		{"open ok", "open", `{"work.attention":"ok"}`, true},
		{"closed ok", "closed", `{"work.attention":"ok"}`, false},
		{"stuck", "open", `{"work.attention":"stuck"}`, false},
		{"needs human", "open", `{"work.attention":"needs-human"}`, false},
		{"absent", "open", `{}`, false},
		{"null", "open", `{"work.attention":null}`, false},
		{"number", "open", `{"work.attention":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch, ok := db.ExpiredAssignmentHandoffPatch(tc.status, db.JSONBlob(tc.metadata))
			assert.Equal(t, tc.want, ok)
			if tc.want {
				assert.JSONEq(t, `"needs-human"`, string(patch["work.attention"]))
				assert.JSONEq(t, `"claim expired without hand-off"`, string(patch["work.attention_msg"]))
			} else {
				assert.Nil(t, patch)
			}
		})
	}
}
