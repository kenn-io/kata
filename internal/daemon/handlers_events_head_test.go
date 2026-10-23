package daemon_test

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestProjectEventHeadAuthorizedCommittedBoundary(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "", "spoke-project")
	_, event := mkIssueWithEvent(t, env, pid, "Committed issue")
	var head struct {
		AfterID     int64  `json:"after_id"`
		InstanceUID string `json:"instance_uid"`
	}
	envGetJSON(t, env, "/api/v1/projects/"+strconv.FormatInt(pid, 10)+"/events/head", &head)
	require.GreaterOrEqual(t, head.AfterID, event.ID)
	require.NotEmpty(t, head.InstanceUID)
	mkIssue(t, env, pid, "Later issue")
	var page struct {
		Events []struct {
			EventID int64 `json:"event_id"`
		}
		NextAfterID int64 `json:"next_after_id"`
	}
	envGetJSON(t, env, "/api/v1/projects/"+strconv.FormatInt(pid, 10)+"/events?after_id="+strconv.FormatInt(head.AfterID, 10), &page)
	require.Len(t, page.Events, 1, "a captured head must not skip the subsequent committed event")
	for _, id := range []string{"0", "999999"} {
		response, _ := envGetRaw(t, env, "/api/v1/projects/"+id+"/events/head")
		require.NotEqual(t, 200, response.StatusCode)
	}
}

func TestProjectEventHeadRejectsForbiddenScope(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		for _, project := range []db.Project{f.private, f.public} {
			status, _, _ := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/events/head", project.ID), "nonmember", nil, nil)
			pollStatus, _, _ := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/events", project.ID), "nonmember", nil, nil)
			if project.UID == f.private.UID {
				require.Equal(t, 404, status, "a denied project scope must not expose a head")
			} else {
				require.Equal(t, 200, status, "an authorized project remains readable")
			}
			require.Equal(t, pollStatus, status, "head preserves event polling denial semantics")
		}
		status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/events/head", f.private.ID), "member", nil, nil)
		require.Equalf(t, 200, status, "%s", body)
	})
}

func TestProjectEventHeadIncludesCommittedPurgeBoundary(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "", "spoke-project")
	issue := mkIssue(t, env, pid, "Removed work")
	purged, err := env.DB.PurgeIssue(t.Context(), issue.ID, "example-user", nil)
	require.NoError(t, err)
	require.NotNil(t, purged.PurgeResetAfterEventID)
	maxEvent, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Greater(t, *purged.PurgeResetAfterEventID, maxEvent)
	var head struct {
		AfterID int64 `json:"after_id"`
	}
	envGetJSON(t, env, fmt.Sprintf("/api/v1/projects/%d/events/head", pid), &head)
	require.GreaterOrEqual(t, head.AfterID, *purged.PurgeResetAfterEventID)
	for range 3 {
		var page struct {
			ResetRequired bool  `json:"reset_required"`
			NextAfterID   int64 `json:"next_after_id"`
		}
		envGetJSON(t, env, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", pid, head.AfterID), &page)
		require.False(t, page.ResetRequired, "a committed head must already include its purge boundary")
		require.Equal(t, head.AfterID, page.NextAfterID)
	}
}
