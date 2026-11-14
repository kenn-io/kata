package pgstore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestFederationFoldSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dst, err := store.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	src, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	const subject = "00000000000000000000000100"
	_, err = store.ExecContext(t.Context(), `INSERT INTO issues(uid,short_id,project_id,title,author) VALUES($1,'00000100',$2,'Moved','tester')`, subject, dst.ID)
	require.NoError(t, err)
	for _, e := range []struct {
		n            int
		project      int64
		envelope     any
		typ, payload string
	}{
		{1, src.ID, subject, "issue.updated", `{"issue_uid":"` + subject + `"}`},
		{2, src.ID, subject, "issue.updated", `{"uid":"` + subject + `"}`},
		{3, src.ID, nil, "project.updated", `{"issue_uid":"` + subject + `"}`},
		{4, dst.ID, subject, "issue.moved", fmt.Sprintf(`{"issue_uid":%q,"from_project_uid":%q,"from_short_id":"00000100","to_project_uid":%q,"to_short_id":"00000100","updated_at":"2026-05-23T12:00:00.000Z"}`, subject, src.UID, dst.UID)},
		{5, dst.ID, nil, "project.updated", `{}`},
	} {
		_, err = store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash) VALUES($1,$2,$3,'test-project',$4,$5,'tester',$6,$7,0,repeat('a',64))`, fmt.Sprintf("%026d", e.n), store.InstanceUID(), e.project, e.envelope, e.typ, e.payload, e.n)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name  string
		types []string
		want  []string
	}{
		{"all", nil, []string{"1", "2", "4", "5"}},
		{"typed", []string{"issue.updated"}, []string{"1", "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, args := pgstore.FederationFoldEventsQueryForTest(dst.ID, tc.types)
			rows, err := store.QueryContext(t.Context(), query, args...)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			var got []string
			for rows.Next() {
				var uid string
				var origin, project, typ, actor, payload, created string
				var issue, related sql.NullString
				var physical, counter int64
				require.NoError(t, rows.Scan(&uid, &origin, &project, &issue, &related, &typ, &actor, &payload, &physical, &counter, &created))
				if strings.HasPrefix(uid, "0000000000000000000000000") {
					got = append(got, strings.TrimLeft(uid, "0"))
				}
			}
			require.NoError(t, rows.Err())
			require.Equal(t, tc.want, got)
		})
	}
}
