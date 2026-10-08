package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

func FuzzNotifyRateHistoryHiddenMessage(f *testing.F) {
	f.Add("hidden target context")
	f.Add(strings.Repeat("hidden target context ", 20))
	f.Add(strings.Repeat("x", notification.MessageMaxBytes))
	f.Fuzz(func(t *testing.T, message string) {
		if strings.TrimSpace(message) == "" || len(message) > notification.MessageMaxBytes || !utf8.ValidString(message) {
			return
		}
		env, p, source, target, _ := scopedReplyFixture(t, true)
		raw, err := json.Marshal(map[string]any{"from": "worker", "message": message, "re": target.UID, "broadcast": true})
		require.NoError(t, err)
		_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "worker", Patch: map[string]jsontext.Value{"notify.cmVhZGVy": raw}})
		require.NoError(t, err)
		response, body := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, source.ShortID), map[string]any{"broadcast": true, "message": "New finding"}, map[string]string{"Authorization": "Bearer worker-token"})
		require.Equal(t, 429, response.StatusCode, string(body))
		var result struct {
			Error struct {
				Data struct {
					Prefix string `json:"last_message_prefix"`
					Retry  int    `json:"retry_after_seconds"`
					Window string `json:"window"`
				} `json:"data"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &result))
		require.Empty(t, result.Error.Data.Prefix, "hidden history cannot leak through rate diagnostics")
		require.Positive(t, result.Error.Data.Retry)
		require.NotEmpty(t, result.Error.Data.Window)
	})
}
