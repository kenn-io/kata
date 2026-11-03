package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func rewriteSignedCommentCreationUIDForTest(ctx context.Context, t *testing.T, store db.Storage, backend Backend, projectUID, canonicalUID string, event db.Event, receipt db.AttributionReceipt, private ed25519.PrivateKey) (string, db.AttributionReceipt) {
	t.Helper()
	lowerUID := strings.ToLower(canonicalUID)
	remote := db.RemoteEventFromStored(event)
	oldValue := []byte(`"comment_uid":"` + canonicalUID + `"`)
	newValue := []byte(`"comment_uid":"` + lowerUID + `"`)
	newPayload := strings.Replace(string(remote.Payload), string(oldValue), string(newValue), 1)
	require.NotEqual(t, string(remote.Payload), newPayload, "the signed creation event must contain its canonical comment UID")
	remote.Payload = jsontext.Value(newPayload)
	hash, err := db.RemoteEventContentHash(remote)
	require.NoError(t, err)
	receipt.ContentHash = hash
	receipt, err = db.SignRootReceipt(receipt, private)
	require.NoError(t, err)
	receiptJSON, err := json.Marshal(receipt)
	require.NoError(t, err)

	executor, ok := store.(commentIdentityTestExecutor)
	require.True(t, ok, "storage test adapter must expose SQL for legacy spelling setup")
	queries := [3]string{
		`UPDATE events SET payload=?,content_hash=? WHERE uid=?`,
		`UPDATE federation_event_provenance SET content_hash=?,receipt=? WHERE project_uid=? AND event_uid=?`,
		`UPDATE federation_entity_provenance SET entity_uid=? WHERE project_uid=? AND kind='comment' AND entity_uid=?`,
	}
	if backend.Name == "postgres" {
		queries = [3]string{
			`UPDATE events SET payload=$1,content_hash=$2 WHERE uid=$3`,
			`UPDATE federation_event_provenance SET content_hash=$1,receipt=$2 WHERE project_uid=$3 AND event_uid=$4`,
			`UPDATE federation_entity_provenance SET entity_uid=$1 WHERE project_uid=$2 AND kind='comment' AND entity_uid=$3`,
		}
	}
	updates := [][]any{
		{newPayload, hash, event.UID},
		{hash, string(receiptJSON), projectUID, event.UID},
		{lowerUID, projectUID, canonicalUID},
	}
	for i, query := range queries {
		result, err := executor.ExecContext(ctx, query, updates[i]...)
		require.NoError(t, err)
		affected, err := result.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), affected)
	}
	return lowerUID, receipt
}

func validateCommentBackupWithCanonicalizedRow(ctx context.Context, t *testing.T, store db.Storage, projectID int64) {
	t.Helper()
	filter := db.ExportFilter{ProjectID: &projectID}
	records := make([]db.ImportRecord, 0)
	for record, err := range store.ExportProjects(ctx, filter) {
		require.NoError(t, err)
		value := record
		records = append(records, &value)
	}
	for record, err := range store.ExportIssues(ctx, filter) {
		require.NoError(t, err)
		value := record
		records = append(records, &value)
	}
	for record, err := range store.ExportComments(ctx, filter) {
		require.NoError(t, err)
		value := record
		records = append(records, &value)
	}
	for record, err := range store.ExportEvents(ctx, filter) {
		require.NoError(t, err)
		value := record
		records = append(records, &value)
	}
	for record, err := range store.ExportAttribution(ctx, filter) {
		require.NoError(t, err)
		records = append(records, record)
	}
	require.NoError(t, db.ValidateImportReplay(records, db.ImportOptions{}),
		"a complete backup must retain signed creation references when comment rows were normalized")
}
