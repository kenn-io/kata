package daemon_test

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestScopedNotificationPointersHiddenAcrossReads(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, true)
	_, e := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "coordinator", Patch: map[string]jsontext.Value{"notify.d29ya2Vy": jsontext.Value(fmt.Sprintf(`{"from":"lead","message":"private","re":%q,"kind":"reply"}`, target.UID))}})
	require.NoError(t, e)
	base := fmt.Sprintf("/api/v1/projects/%d/issues/%s", p.ID, source.ShortID)
	for _, path := range []string{base, base + "/metadata", fmt.Sprintf("/api/v1/projects/%d/issues", p.ID), fmt.Sprintf("/api/v1/projects/%d/events", p.ID), "/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + source.UID} {
		t.Run(path, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer worker-token"})
			require.Equal(t, 200, resp.StatusCode, string(body))
			require.NotContains(t, string(body), target.UID)
			require.NotContains(t, string(body), "private")
		})
	}
}
