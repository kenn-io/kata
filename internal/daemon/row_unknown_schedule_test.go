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
		name, scheduled      string
		unsupported, replace bool
	}{
		{"unsupported rules", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}`, true, true},
		{"unsupported members", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":true}`, true, true},
		{"opaque legacy value", `{"legacy":"opaque"}`, false, true},
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
					if tc.unsupported && path == show {
						assertAPIError(t, status, raw, http.StatusBadRequest, "unsupported_schedule")
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
					if tc.unsupported {
						require.NotEmpty(t, row["schedule_error"])
					} else {
						require.NotContains(t, row, "schedule_error")
					}
					require.Equal(t, "2026-11-02", row["deadline_on_date"])
				})
			}
			if !tc.replace {
				return
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

func TestUnknownScheduleDoesNotHideOtherIssues(t *testing.T) {
	f := newRowDatesFixture(t)
	records, err := dbtest.CollectImportRecords(t.Context(), f.env.DB, db.ExportFilter{ProjectID: &f.project.ID})
	require.NoError(t, err)
	for _, record := range records {
		if issue, ok := record.(*db.IssueExport); ok && issue.UID == f.nodate.UID {
			issue.Metadata = jsontext.Value(`{"scheduled_on":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}}`)
		}
	}
	require.NoError(t, f.env.DB.ImportReplay(t.Context(), records, db.ImportOptions{}))
	for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID), "/api/v1/issues"} {
		t.Run(path, func(t *testing.T) {
			status, raw := cronHTTP(t, f.env, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var body struct {
				Issues []struct {
					UID string `json:"uid"`
				} `json:"issues"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			found := false
			for _, issue := range body.Issues {
				found = found || issue.UID == f.dateOnly.UID
			}
			require.True(t, found, "a healthy issue must remain visible")
		})
	}
}

func TestUISnapshotShowsHealthyIssuesWhenScheduleRulesAreUnknown(t *testing.T) {
	f := newRowDatesFixture(t)
	records, err := dbtest.CollectImportRecords(t.Context(), f.env.DB, db.ExportFilter{ProjectID: &f.project.ID})
	require.NoError(t, err)
	for _, record := range records {
		if issue, ok := record.(*db.IssueExport); ok && issue.UID == f.nodate.UID {
			issue.Metadata = jsontext.Value(`{"scheduled_on":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}}`)
		}
	}
	require.NoError(t, f.env.DB.ImportReplay(t.Context(), records, db.ImportOptions{}))
	status, raw := cronHTTP(t, f.env, http.MethodGet, "/api/v1/ui/snapshot?view=all-open&time_zone=UTC", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var body struct {
		Collection []struct {
			UID           string                    `json:"uid"`
			ScheduleError string                    `json:"schedule_error"`
			Metadata      map[string]jsontext.Value `json:"metadata"`
		} `json:"collection"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	var healthy, flagged bool
	for _, issue := range body.Collection {
		healthy = healthy || issue.UID == f.dateOnly.UID
		flagged = flagged || issue.UID == f.nodate.UID && issue.ScheduleError != ""
		if issue.UID == f.nodate.UID && len(issue.Metadata["scheduled_on"]) > 0 {
			var legacyDate string
			require.NoError(t, json.Unmarshal(issue.Metadata["scheduled_on"], &legacyDate), "old browsers must receive a string or no date")
		}
	}
	require.True(t, healthy, "a healthy issue must remain visible")
	require.True(t, flagged, "the unreadable schedule must be identified")
}

func TestShowReportsUnknownScheduleAsClientError(t *testing.T) {
	f := newRowDatesFixture(t)
	records, err := dbtest.CollectImportRecords(t.Context(), f.env.DB, db.ExportFilter{ProjectID: &f.project.ID})
	require.NoError(t, err)
	for _, record := range records {
		if issue, ok := record.(*db.IssueExport); ok && issue.UID == f.nodate.UID {
			issue.Metadata = jsontext.Value(`{"scheduled_on":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}}`)
		}
	}
	require.NoError(t, f.env.DB.ImportReplay(t.Context(), records, db.ImportOptions{}))
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.project.ID, f.nodate.ShortID)
	status, raw := cronHTTP(t, f.env, http.MethodGet, path, nil)
	assertAPIError(t, status, raw, http.StatusBadRequest, "unsupported_schedule")
	require.Contains(t, string(raw), "unsupported schedule tzdata version")
}
