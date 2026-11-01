package jsonl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/jsonl"
)

func decodeLines(ctx context.Context, lines ...string) ([]jsonl.Envelope, error) {
	return jsonl.NewDecoder(strings.NewReader(buildJSONL(lines...))).ReadAll(ctx)
}

func TestDecoderRequiresExportVersionFirst(t *testing.T) {
	_, err := decodeLines(t.Context(), `{"kind":"project","data":{"id":1}}`)

	require.Error(t, err)
	assert.ErrorIs(t, err, jsonl.ErrMissingExportVersion)
	assert.Contains(t, err.Error(), "line 1")
}

func TestDecoderRejectsUnknownKind(t *testing.T) {
	_, err := decodeLines(t.Context(),
		validExportVersion,
		`{"kind":"bogus","data":{"id":1}}`,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, jsonl.ErrUnknownKind)
	assert.Contains(t, err.Error(), "line 2")
}

func TestDecoderRejectsOutOfOrderKind(t *testing.T) {
	_, err := decodeLines(t.Context(),
		validExportVersion,
		`{"kind":"link","data":{"id":1}}`,
		`{"kind":"issue","data":{"id":1}}`,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, jsonl.ErrKindOrderViolation)
	assert.Contains(t, err.Error(), "line 3")
}

func TestDecoderReportsInvalidJSONLine(t *testing.T) {
	_, err := decodeLines(t.Context(),
		validExportVersion,
		`{"kind":"project","data":`,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2")
}

func TestDecoderReadsOrderedEnvelopes(t *testing.T) {
	got, err := decodeLines(t.Context(),
		`{"kind":"meta","data":{"key":"export_version","value":"1"},"ignored":true}`,
		`{"kind":"meta","data":{"key":"schema_version","value":"1"}}`,
		`{"kind":"project","data":{"id":1}}`,
	)

	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, jsonl.KindMeta, got[0].Kind)
	assert.Equal(t, jsonl.KindProject, got[2].Kind)
}

func TestEncoderWritesCompactJSONLines(t *testing.T) {
	var out strings.Builder
	enc := jsonl.NewEncoder(&out)

	err := enc.Write(jsonl.Envelope{
		Kind: jsonl.KindMeta,
		Data: []byte(`{"key":"export_version","value":"1"}`),
	})

	require.NoError(t, err)
	assert.Equal(t, validExportVersion+"\n", out.String())
}

func TestDecoderReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := decodeLines(ctx, validExportVersion)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}
