package dbtest

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkAdoptionPreservesProjectAccessPolicy(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	access, ok := store.(db.ProjectAccessStorage)
	require.True(t, ok)
	project, err := store.CreateProject(ctx, "adoption-access-project")
	if err != nil {
		return err
	}
	team, _, err := access.CreateTeam(ctx, "adoption-access-team", "admin")
	if err != nil {
		return err
	}
	policy, err := access.ProjectAccessPolicy(ctx, project.UID)
	if err != nil {
		return err
	}
	policy.Visibility = "teams"
	policy.TeamUIDs = []string{team.UID}
	policy, _, err = access.SetProjectAccessPolicy(ctx, policy, "admin")
	if err != nil {
		return err
	}
	accessRevision, err := access.ProjectAccessRevision(ctx)
	if err != nil {
		return err
	}
	hubUID, err := uid.New()
	if err != nil {
		return err
	}
	result, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: hubUID, ReplayHorizonEventID: 1, Actor: "admin",
	})
	if err != nil {
		return err
	}
	updatedAccessRevision, err := access.ProjectAccessRevision(ctx)
	if err != nil {
		return err
	}
	assert.Greater(t, updatedAccessRevision, accessRevision,
		"the adopted project identity must invalidate cached access decisions")
	assert.Equal(t, hubUID, result.Project.UID)
	got, err := access.ProjectAccessPolicy(ctx, hubUID)
	if err != nil {
		return err
	}
	assert.Equal(t, policy.Visibility, got.Visibility)
	assert.Equal(t, policy.Revision, got.Revision)
	assert.Equal(t, policy.TeamUIDs, got.TeamUIDs,
		"the project policy and team grants must follow its adopted UID")
	_, err = access.ProjectAccessPolicy(ctx, project.UID)
	assert.ErrorIs(t, err, db.ErrNotFound, "the replaced UID must not retain an orphan policy")
	return nil
}

func checkProjectScopedRelayEnrollmentExportRoundTrip(
	t *testing.T,
	store db.Storage,
	backend Backend,
) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "relay-export-project")
	if err != nil {
		return err
	}
	if _, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	}); err != nil {
		return err
	}
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	if err := store.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: store.InstanceUID(),
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}); err != nil {
		return err
	}
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "relay-export-parent-token", Actor: "member", AdminActor: "admin",
	})
	if err != nil {
		return err
	}
	spokeUID, err := uid.New()
	if err != nil {
		return err
	}
	created, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: spokeUID,
		ProtocolVersion: db.RelayProtocolVersion, Token: "relay-export-downstream-token",
	})
	if err != nil {
		return err
	}
	require.NotNil(t, created.Enrollment.ParentTokenID)
	assert.Equal(t, parent.ID, *created.Enrollment.ParentTokenID)

	filter := db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}
	var scoped []db.FederationEnrollmentExport
	for enrollment, err := range store.ExportFederationEnrollments(ctx, filter) {
		if err != nil {
			return err
		}
		scoped = append(scoped, enrollment)
	}
	assert.Empty(t, scoped, "project-scoped exports omit hub-local downstream credentials")
	var full []db.FederationEnrollmentExport
	for enrollment, err := range store.ExportFederationEnrollments(ctx, db.ExportFilter{}) {
		if err != nil {
			return err
		}
		full = append(full, enrollment)
	}
	require.Len(t, full, 1, "instance backups retain relay enrollment state")
	assert.Equal(t, created.Enrollment.ID, full[0].ID)

	records, err := CollectImportRecords(ctx, store, filter)
	if err != nil {
		return err
	}
	for _, record := range records {
		_, isEnrollment := record.(*db.FederationEnrollmentExport)
		assert.False(t, isEnrollment,
			"the project replay must not carry an enrollment with a local token parent")
	}
	target := backend.Open(t)
	require.NotNil(t, target)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	if err := target.ImportReplay(ctx, records, db.ImportOptions{}); err != nil {
		return err
	}
	if _, err := target.ProjectByUID(ctx, project.UID); err != nil {
		return err
	}
	for enrollment, err := range target.ExportFederationEnrollments(ctx, db.ExportFilter{}) {
		if err != nil {
			return err
		}
		assert.NotEqual(t, created.Enrollment.ID, enrollment.ID)
	}
	return nil
}
