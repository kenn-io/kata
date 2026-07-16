package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestRoundtripRichDatabaseAcrossPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	fixture := buildRichJSONLFixture(t)
	sourceJSONL := exportToBuffer(ctx, t, fixture.DB)

	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	postgresStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, postgresStore.Close()) })
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(sourceJSONL.Bytes()), postgresStore))

	var postgresJSONL bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, postgresStore, &postgresJSONL,
		jsonl.ExportOptions{IncludeDeleted: true}))
	assert.Equal(t, replayWireRecordsWithoutSequences(t, sourceJSONL.Bytes()),
		replayWireRecordsWithoutSequences(t, postgresJSONL.Bytes()))

	roundTripped := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(postgresJSONL.Bytes()), roundTripped))
	assertRoundtripTableCounts(t, fixture.DB, roundTripped)
	assertSearchResultsMatch(t, fixture.DB, roundTripped, fixture.Project.ID, "orchid")
	assertSearchResultsMatch(t, fixture.DB, roundTripped, fixture.Project.ID, "watermelon")
	assertSearchResultsMatch(t, fixture.DB, roundTripped, fixture.Project.ID, "soft")
}

func replayWireRecordsWithoutSequences(t *testing.T, data []byte) []string {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var records []string
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var envelope struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, json.Unmarshal(line, &envelope))
		if envelope.Kind != db.ImportKindSQLiteSequence {
			records = append(records, string(line))
		}
	}
	require.NoError(t, scanner.Err())
	return records
}
