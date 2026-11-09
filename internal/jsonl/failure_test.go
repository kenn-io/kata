package jsonl_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/jsonl"
)

func TestImportPropagatesDecoderFailures(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "truncated JSON",
			input:   buildJSONL(validExportVersion, `{"kind":"project","data":`),
			wantErr: "line 2",
		},
		{
			name:    "missing export version",
			input:   buildJSONL(`{"kind":"project","data":{"id":1}}`),
			wantErr: "missing export_version",
		},
		{
			name: "kind order violation",
			input: buildJSONL(
				validExportVersion,
				`{"kind":"event","data":{"id":1}}`,
				`{"kind":"issue","data":{"id":1}}`,
			),
			wantErr: "kind order violation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := openImportTargetDB(t)

			err := jsonl.Import(t.Context(), strings.NewReader(tt.input), target)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
