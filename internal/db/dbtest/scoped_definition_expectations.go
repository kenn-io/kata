package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// RunScopedDefinitionPreparation uses genuine dormant native definitions and
// the real root preparer. No definition event, issuer row or token is seeded.
// Source emission and root token consumption are separate tests/owner joins.
func RunScopedDefinitionPreparation(t *testing.T, s db.Storage, kind string) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	write := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[0].UID), f.Parent.Actor)
	ctx := scopedPreparationContext(t, f)
	ctx = db.WithTransactionFence(ctx, db.ActorTokenTransactionFence(f.Parent))
	ctx = db.WithAdditionalTransactionFence(ctx, s.ProjectAccessTransactionFence(f.Parent.Actor, []string{f.Projects[0].UID, f.Projects[1].UID}))
	native := s.(db.CronDefinitions)
	var current db.CronDefinition
	put := func(name string, deleted bool) {
		t.Helper()
		if kind == "job" {
			job, _, err := native.PutCronJob(write, db.PutCronJob{UID: current.UID, ProjectID: f.Projects[0].ID, Name: name, Actor: f.Parent.Actor, ExpectedEventUID: current.DefinitionEventUID, Deleted: deleted, Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: false, Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect the current work"}, Issue: &cron.IssuePolicy{Kind: "existing", UID: f.Issue.UID}, Overlap: "forbid", Catchup: "skip"}})
			require.NoError(t, err)
			current = job.CronDefinition
		} else {
			workflow, _, err := native.PutCronWorkflow(write, db.PutCronWorkflow{UID: current.UID, ProjectID: f.Projects[0].ID, Name: name, Actor: f.Parent.Actor, ExpectedEventUID: current.DefinitionEventUID, Deleted: deleted, Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "prompt", Prompt: "Inspect the current work"}}}})
			require.NoError(t, err)
			current = workflow.CronDefinition
		}
	}
	put("Definition A", false)
	command := map[string]string{"job": "CronJob", "workflow": "CronWorkflow"}[kind]
	preparation := db.ScopedMutationPreparation{Kind: "replace" + command, Subject: db.ScopedMutationSubject{Kind: kind, ProjectUID: f.Projects[0].UID, DefinitionUID: current.UID}, Predicate: "definition", Peers: []db.ScopedEndpoint{}}
	prepare := func() db.ScopedMutationPredicate {
		t.Helper()
		token, err := s.(scopedStablePreparationStore).PrepareScopedMutation(ctx, preparation)
		require.NoError(t, err)
		predicate := scopedPreparedPredicate(t, s, token)
		require.Equal(t, preparation, predicate.Preparation)
		return predicate
	}
	beforeCursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	first := prepare()
	storageKind := kind + ".definition"
	golden := `{"Current":{"DefinitionEventUID":` + string(scopedOwnerBytes(t, current.DefinitionEventUID)) + `,"Deleted":false,"ProjectDeleted":false},"Domain":"kata.scoped.root-predicate.v1","Kind":` + string(scopedOwnerBytes(t, storageKind)) + `}`
	require.Equal(t, db.ScopedOfferedDigest([]byte(golden)), first.CanonicalDigest, "complete independent native winning-version predicate envelope")
	digest, err := db.ScopedDefinitionPredicateDigest(storageKind, current, false)
	require.NoError(t, err)
	require.Equal(t, first.CanonicalDigest, digest)
	remapped := current
	remapped.ID, remapped.ProjectID, remapped.Revision = current.ID+100, current.ProjectID+100, current.Revision+100
	remapped.Name, remapped.Author = "Local presentation alias", "local-presentation"
	remapped.CreatedAt, remapped.UpdatedAt = current.CreatedAt.AddDate(0, 1, 0), current.UpdatedAt.AddDate(0, 1, 0)
	remapped.DefinitionHLC.PhysicalMS++
	remappedDigest, err := db.ScopedDefinitionPredicateDigest(storageKind, remapped, false)
	require.NoError(t, err)
	require.Equal(t, digest, remappedDigest, "local IDs/revision/aliases/timestamps/HLC are not the native version value")
	projectDeletedDigest, err := db.ScopedDefinitionPredicateDigest(storageKind, current, true)
	require.NoError(t, err)
	require.NotEqual(t, digest, projectDeletedDigest)
	again := prepare()
	require.Equal(t, first, again, "repeated preparation does not mutate the native predicate value/incarnation")
	afterCursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, beforeCursor, afterCursor, "issuing an opaque expectation emits no canonical event")
	scopedAssertPredicateCurrent(t, s, first, false)
	priorUID := current.DefinitionEventUID
	put("Definition A", false)
	require.NotEqual(t, priorUID, current.DefinitionEventUID, "native same-document Put is a new winning event, not an invented no-op")
	scopedAssertPredicateCurrent(t, s, first, true)
	second := prepare()
	put("Definition B", false)
	put("Definition A", false)
	scopedAssertPredicateCurrent(t, s, second, true)
	third := prepare()
	require.NotEqual(t, second.CanonicalDigest, third.CanonicalDigest, "winning event UID prevents document ABA")
	preparation.Kind = "archive" + command
	archive := prepare()
	put("Definition A", true)
	scopedAssertPredicateCurrent(t, s, archive, true)
	preparation.Kind = "restore" + command
	restore := prepare()
	put("Definition A", false)
	scopedAssertPredicateCurrent(t, s, restore, true)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}
