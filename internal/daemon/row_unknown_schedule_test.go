package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/metadata"
)

func TestRowsHandleStoredScheduleObjects(t *testing.T) {
	for _, tc := range []struct {
		name, scheduled string
		unsupported     bool
	}{
		{"unsupported rules", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}`, true},
		{"unsupported members", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":true}`, true},
		{"opaque legacy value", `{"legacy":"opaque"}`, false},
		{"opaque numeric value", `{"value":123}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRowDatesFixture(t)
			records, err := dbtest.CollectImportRecords(t.Context(), f.env.DB, db.ExportFilter{ProjectID: &f.project.ID})
			require.NoError(t, err)
			for _, record := range records {
				if issue, ok := record.(*db.IssueExport); ok && issue.UID == f.nodate.UID {
					issue.Metadata = jsontext.Value(`{"scheduled_on":` + tc.scheduled + `,"deadline_on":"2026-11-02"}`)
				}
			}
			require.NoError(t, f.env.DB.ImportReplay(t.Context(), records, db.ImportOptions{}))
			projectList := fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID)
			show := projectList + "/" + f.nodate.ShortID
			for _, path := range []string{projectList, "/api/v1/issues", show} {
				t.Run(path, func(t *testing.T) {
					status, raw := cronHTTP(t, f.env, http.MethodGet, path, nil)
					if tc.unsupported {
						assertAPIError(t, status, raw, http.StatusInternalServerError, "internal")
						return
					}
					require.Equal(t, http.StatusOK, status, string(raw))
					var body struct {
						Issue  map[string]any   `json:"issue"`
						Issues []map[string]any `json:"issues"`
					}
					require.NoError(t, json.Unmarshal(raw, &body))
					row := body.Issue
					if path == show && tc.name == "opaque legacy value" {
						require.Contains(t, string(raw), `"legacy":"opaque"`)
					} else if path != show {
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
			t.Run("replace scheduled_on", func(t *testing.T) {
				before, err := f.env.DB.IssueByID(t.Context(), f.nodate.ID)
				require.NoError(t, err)
				status, raw := cronHTTP(t, f.env, http.MethodPost, show+"/metadata", map[string]any{"actor": "tester", "patch": map[string]string{"scheduled_on": "2026-11-13"}})
				after, err := f.env.DB.IssueByID(t.Context(), f.nodate.ID)
				require.NoError(t, err)
				if tc.unsupported {
					require.Equal(t, http.StatusBadRequest, status, string(raw))
					require.Equal(t, before, after)
					return
				}
				require.Equal(t, http.StatusOK, status, string(raw))
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(after.Metadata), &fields))
				saved, err := metadata.DecodeSavedSchedule(fields["scheduled_on"])
				require.NoError(t, err)
				require.NotNil(t, saved)
				require.Equal(t, "America/New_York", saved.Timezone)
				require.Equal(t, "2026-11-13", saved.Value)
			})
		})
	}
}

func TestMetadataRejectsReplacingUnsupportedScheduleObject(t *testing.T) {
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		for _, tc := range []struct{ name, stored string }{
			{"future rules", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}`},
			{"future member", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":true}`},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				f := newRowDatesFixture(t)
				records, err := dbtest.CollectImportRecords(t.Context(), f.env.DB, db.ExportFilter{ProjectID: &f.project.ID})
				require.NoError(t, err)
				for _, record := range records {
					if issue, ok := record.(*db.IssueExport); ok && issue.UID == f.nodate.UID {
						issue.Metadata = jsontext.Value(`{"` + field + `":` + tc.stored + `}`)
					}
				}
				require.NoError(t, f.env.DB.ImportReplay(t.Context(), records, db.ImportOptions{}))
				before, err := f.env.DB.IssueByID(t.Context(), f.nodate.ID)
				require.NoError(t, err)
				path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", f.project.ID, f.nodate.ShortID)
				replacement := map[string]any{"value": "2026-11-13", "timezone": "UTC", "tzdata_version": metadata.ScheduleTZDataVersion}
				status, raw := cronHTTP(t, f.env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]any{field: replacement}})
				require.Equal(t, http.StatusBadRequest, status, string(raw))
				after, err := f.env.DB.IssueByID(t.Context(), f.nodate.ID)
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	}
}
