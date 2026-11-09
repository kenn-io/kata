package db_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

// Contract: every provider decodes its existing configuration unchanged when
// the binding also carries an explicit connection identity.
func TestIssueSyncConnectionMetadataAcrossProviders(t *testing.T) {
	const objectID = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name   string
		config string
		decode func(jsontext.Value) error
	}{
		{"github", `{"host":"github.com","owner":"example-owner","repo":"example-repo","repo_id":1}`, func(raw jsontext.Value) error { _, err := githubsync.DecodeConfig(raw); return err }},
		{"notion", `{"data_source_id":"` + objectID + `","database_id":"` + objectID + `","title_property_id":"title","status_property_id":"status","assignee_property_id":"people","done_status_ids":["done"]}`, func(raw jsontext.Value) error { _, err := notionsync.DecodeConfig(raw); return err }},
		{"linear", `{"workspace_id":"` + objectID + `","team_id":"` + objectID + `"}`, func(raw jsontext.Value) error { _, err := linearsync.DecodeConfig(raw); return err }},
		{"plane", `{"api_origin":"https://api.plane.so","web_origin":"https://app.plane.so","workspace":"example-workspace","project_id":"` + objectID + `"}`, func(raw jsontext.Value) error { _, err := planesync.DecodeConfig(raw); return err }},
		{"ticktick", `{"project_id":"example-project"}`, func(raw jsontext.Value) error { _, err := tickticksync.DecodeConfig(raw); return err }},
		{"todoist", `{"api_origin":"https://api.todoist.com","account_id":"1234567","project_id":"example-project","history_since":"2026-09-01T00:00:00Z"}`, func(raw jsontext.Value) error { _, err := todoistsync.DecodeConfig(raw); return err }},
		{"twenty", `{"api_origin":"https://twenty.example","web_origin":"https://ui.example","workspace_id":"` + objectID + `"}`, func(raw jsontext.Value) error { _, err := twentysync.DecodeConfig(raw); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := jsontext.Value(tc.config)
			require.NoError(t, tc.decode(raw), "baseline fixture must be valid")
			selected, err := db.SetIssueSyncConnectionConfig(raw, db.IssueSyncConnection{
				OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
				ConnectionUID:    "01ARZ3NDEKTSV4RRFFQ69G5FAW",
				Generation:       "first",
			})
			require.NoError(t, err)
			require.NoError(t, tc.decode(selected))
		})
	}
}
