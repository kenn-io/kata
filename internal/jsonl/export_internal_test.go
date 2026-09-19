package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestExportForCutoverPreservesAssignment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		ttl     time.Duration
	}{
		{name: "v28_permanent", version: 28},
		{name: "v29_permanent", version: 29},
		{name: "v29_timed", version: 29, ttl: 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			project, err := source.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: "Assigned issue", Author: "tester",
			})
			require.NoError(t, err)
			_, err = source.ClaimOwner(ctx, db.ClaimOwnerParams{
				IssueID: issue.ID, Actor: "worker-a", TTL: tc.ttl,
				Now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			if tc.version < 29 {
				// Exercise the old projection against a database without the column.
				_, err = source.ExecContext(ctx, `DROP INDEX idx_issues_assignment_expires_on`)
				require.NoError(t, err)
				_, err = source.ExecContext(ctx, `ALTER TABLE issues DROP COLUMN assignment_expires_on`)
				require.NoError(t, err)
			}
			_, err = source.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'schema_version'`,
				fmt.Sprint(tc.version))
			require.NoError(t, err)

			var out bytes.Buffer
			require.NoError(t, exportForCutover(ctx, source, &out, ExportOptions{IncludeDeleted: true}))
			target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, Import(ctx, &out, target))
			restored, err := target.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotNil(t, restored.Owner)
			assert.Equal(t, "worker-a", *restored.Owner)
			if tc.ttl == 0 {
				assert.Nil(t, restored.AssignmentExpiresOn)
			} else {
				require.NotNil(t, restored.AssignmentExpiresOn)
				assert.Equal(t, "2026-09-19T12:30:00Z", restored.AssignmentExpiresOn.UTC().Format(time.RFC3339))
			}
		})
	}
}

func TestExportSnapshotV14FederationEnrollmentPreservesAdoptionMarker(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `
		INSERT INTO federation_enrollments(
			token_hash, spoke_instance_uid, project_id, capabilities, bound_actor,
			allow_adoption_snapshot_authors
		)
		VALUES(?, ?, ?, 'pull,push', 'tester', 1)`,
		strings.Repeat("c", 64), "01HZZZZZZZZZZZZZZZZZZZZZ04", project.ID)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, exportSnapshot(ctx, source, &out, ExportOptions{}))

	assert.Contains(t, out.String(), `"allow_adoption_snapshot_authors":true`)
	assert.Contains(t, out.String(), `"bound_actor":"tester"`)
}

func TestWriteRecordUsesDeterministicJSON(t *testing.T) {
	for range 100 {
		var out bytes.Buffer
		require.NoError(t, writeRecord(NewEncoder(&out), KindMeta, map[string]any{"z": 1, "a": 2}))
		assert.Equal(t, "{\"kind\":\"meta\",\"data\":{\"a\":2,\"z\":1}}\n", out.String())
	}
}

func TestMarshalLegacyGitHubSyncConfigUsesDeterministicJSON(t *testing.T) {
	for range 100 {
		got := mustMarshalGitHubSyncConfig("github.example", "owner", "repo", 7)
		assert.Equal(t, `{"host":"github.example","owner":"owner","repo":"repo","repo_id":7}`, string(got))
	}
}

func TestExportSnapshotCarriesExternalRootStateWithoutLiveClaim(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Portable root", Author: "tester",
	})
	require.NoError(t, err)
	frontier := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	binding, _, err := source.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "connector-one",
		ExternalRootKey: "root-one", ExternalAccountKey: "account-one",
		Actor: "tester", ReceiveCommentsAfter: frontier,
	})
	require.NoError(t, err)
	mapping, err := source.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "field-one", ExternalFieldName: "Schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "revision-one",
	})
	require.NoError(t, err)
	const claimToken = "live-claim-must-not-export"
	binding, claimed, err := source.ClaimExternalRootBinding(
		ctx, binding.ID, claimToken, frontier.Add(time.Hour), frontier,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	_, _, err = source.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
		BindingID: binding.ID, MappingID: mapping.ID, ClaimToken: claimToken,
		Baseline: jsontext.Value(`"2026-08-20"`), At: frontier.Add(time.Hour),
		Actor: "tester",
	})
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, exportSnapshot(ctx, source, &out, ExportOptions{}))

	assert.Contains(t, out.String(), `"kind":"external_field_mapping"`)
	assert.Contains(t, out.String(), `"kind":"external_root_binding"`)
	assert.Contains(t, out.String(), `"kind":"external_field_state"`)
	assert.NotContains(t, out.String(), claimToken)
	assert.NotContains(t, out.String(), `"claim_token"`)
}

func TestExportSnapshotProjectIncludesActiveMappingBeforeStateExists(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Portable root", Author: "tester",
	})
	require.NoError(t, err)
	_, _, err = source.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "connector-one",
		ExternalRootKey: "root-one", ExternalAccountKey: "account-one",
		Actor: "tester", ReceiveCommentsAfter: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	_, err = source.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "field-one", ExternalFieldName: "Schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "revision-one",
	})
	require.NoError(t, err)
	_, err = source.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-unbound", KataField: "deadline_on",
		ExternalFieldID: "field-unbound", ExternalFieldName: "Deadline",
		AcceptedKinds: []string{"instant"}, Nullable: true, Writable: true,
		SchemaRevision: "revision-unbound",
	})
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, exportSnapshot(ctx, source, &out, ExportOptions{
		ProjectID: project.ID, IncludeDeleted: true,
	}))
	envelopes, err := NewDecoder(bytes.NewReader(out.Bytes())).ReadAll(ctx)
	require.NoError(t, err)
	var mappings []db.ExternalFieldMappingExport
	for _, envelope := range envelopes {
		if envelope.Kind != KindExternalFieldMapping {
			continue
		}
		var mapping db.ExternalFieldMappingExport
		require.NoError(t, json.Unmarshal(envelope.Data, &mapping))
		mappings = append(mappings, mapping)
	}
	require.Len(t, mappings, 1)
	assert.Equal(t, "connector-one", mappings[0].ConnectorInstance)
	assert.Equal(t, "field-one", mappings[0].ExternalFieldID)
}
