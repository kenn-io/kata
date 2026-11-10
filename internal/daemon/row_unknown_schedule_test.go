package daemon_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRowsRejectUnknownScheduleRules(t *testing.T) {
	f := newRowDatesFixture(t)
	_, err := f.env.DB.ExecContext(t.Context(), `UPDATE issues SET metadata = ? WHERE id = ?`, `{"scheduled_on":{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}}`, f.nodate.ID)
	require.NoError(t, err)
	for _, path := range []string{
		fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID),
		"/api/v1/issues",
		fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.project.ID, f.nodate.ShortID),
	} {
		t.Run(path, func(t *testing.T) {
			status, raw := cronHTTP(t, f.env, http.MethodGet, path, nil)
			require.NotEqual(t, http.StatusOK, status, string(raw))
			require.Contains(t, string(raw), "unsupported schedule tzdata version")
		})
	}
}
