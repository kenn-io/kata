package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRowsHandleStoredScheduleObjects(t *testing.T) {
	for _, tc := range []struct {
		name, scheduled string
		unsupported     bool
	}{
		{"unsupported rules", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}`, true},
		{"opaque legacy value", `{"legacy":"opaque"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRowDatesFixture(t)
			// TestImportAcceptsOpaqueScheduleMetadata covers admission of these
			// restore shapes. Seed the restored projection to exercise its readers.
			_, err := f.env.DB.ExecContext(t.Context(), `UPDATE issues SET metadata = ? WHERE id = ?`,
				`{"scheduled_on":`+tc.scheduled+`,"deadline_on":"2026-11-02"}`, f.nodate.ID)
			require.NoError(t, err)
			projectList := fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID)
			show := projectList + "/" + f.nodate.ShortID
			for _, path := range []string{projectList, "/api/v1/issues", show} {
				t.Run(path, func(t *testing.T) {
					status, raw := cronHTTP(t, f.env, http.MethodGet, path, nil)
					if tc.unsupported {
						require.NotEqual(t, http.StatusOK, status, string(raw))
						require.Contains(t, string(raw), "unsupported schedule tzdata version")
						return
					}
					require.Equal(t, http.StatusOK, status, string(raw))
					var body struct {
						Issue  map[string]any   `json:"issue"`
						Issues []map[string]any `json:"issues"`
					}
					require.NoError(t, json.Unmarshal(raw, &body))
					row := body.Issue
					if path == show {
						require.Contains(t, string(raw), `"legacy":"opaque"`)
					} else {
						var sibling map[string]any
						for _, issue := range body.Issues {
							switch issue["uid"] {
							case f.nodate.UID:
								row = issue
							case f.dateOnly.UID:
								sibling = issue
							}
						}
						require.Equal(t, "2026-10-29", sibling["scheduled_on_date"])
					}
					require.Equal(t, f.nodate.UID, row["uid"])
					require.NotContains(t, row, "scheduled_on_date")
					require.Equal(t, "2026-11-02", row["deadline_on_date"])
				})
			}
		})
	}
}
