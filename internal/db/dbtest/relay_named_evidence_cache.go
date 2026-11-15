package dbtest

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayNamedEvidenceCache verifies cached history validation follows project name resolution changes.
func RunRelayNamedEvidenceCache(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "named-evidence")
	project := fixture.project
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Evidence target", Author: "member"})
	require.NoError(t, err)
	closed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Legacy close", Author: "member"})
	require.NoError(t, err)
	closeEvent := newRemoteEvent(t, project, &closed.UID, "issue.closed", "member", store.InstanceUID(), 600,
		jsontext.Value(fmt.Sprintf(`{"reason":"duplicate","evidence":[{"type":"duplicate-of","issue_ref":%q}]}`, project.Name+"#"+target.ShortID)))
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{closeEvent})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion,
	})
	require.NoError(t, err)
	bootstrap := store.(db.RelayResetBootstrapStore)
	check := func(want bool) {
		t.Helper()
		cached, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		fresh, err := backend.OpenSame(t, store).(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		require.Equal(t, want, fresh, "fresh validation resolves the stored project name")
		require.Equal(t, fresh, cached, "cached validation must follow name resolution changes")
	}
	check(false)
	_, err = store.RenameProject(ctx, project.ID, "renamed-evidence")
	require.NoError(t, err)
	check(true)
	other, err := store.CreateProject(ctx, project.Name)
	require.NoError(t, err)
	foreignUID, err := uid.New()
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Foreign evidence", Author: "member", UID: foreignUID[:10] + target.UID[10:], ShortIDOverride: target.ShortID})
	require.NoError(t, err)
	check(true)
	_, err = store.RenameProject(ctx, other.ID, "other-evidence")
	require.NoError(t, err)
	_, err = store.RenameProject(ctx, project.ID, project.Name)
	require.NoError(t, err)
	check(false)
}
