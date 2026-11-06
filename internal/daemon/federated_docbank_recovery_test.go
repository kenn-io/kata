package daemon_test

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/uid"
)

// The origin UID remains absolute when a project loses or disables its route.
// A matching version in the local vault must never substitute for that origin.
func TestFederatedDocbankRecoveryForeignOriginNeverUsesLocalVault(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled_binding_%t", disabled), func(t *testing.T) {
			env, vault := attachmentEnv(t)
			project, issue := seedProjectAndIssue(t, env)
			origin, err := uid.New()
			require.NoError(t, err)
			attachmentID, err := uid.New()
			require.NoError(t, err)
			version := uuid.MustParse(linkVersion)
			vault.Seed("/kata/matching.pdf", docbank.Node{ID: 42, Name: "matching.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("local bytes")})
			raw := jsontext.Value(fmt.Sprintf(`{"name":"matching.pdf","media_type":"application/pdf","size":11,"docbank_version":%q,"origin":%q}`, version.String(), origin))
			_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment." + attachmentID: raw}})
			require.NoError(t, err)
			if disabled {
				_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: false})
				require.NoError(t, err)
			}
			status, body := env.Get(t, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/%s/content", project.ID, issue.UID, attachmentID))
			require.Contains(t, []int{http.StatusGone, http.StatusServiceUnavailable}, status, string(body))
			require.Empty(t, vault.Requests(), "a foreign origin must not consult the local vault")
		})
	}
}
