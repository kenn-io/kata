package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunFederationAdoptionFence tests the durable pause through actual native writes.
func RunFederationAdoptionFence(t *testing.T, store db.Storage) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "source", Author: "member"})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "peer", Author: "member"})
	require.NoError(t, err)
	link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: peer.ID, ToIssueID: issue.ID, Type: "blocks", Author: "member"})
	require.NoError(t, err)
	syncBinding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: source.ID, Provider: "github", SourceKey: "github:example/repo", RemoteID: "example/repo", DisplayName: "example/repo", Config: []byte(`{}`), IntervalSeconds: 60})
	require.NoError(t, err)
	_, err = store.DisableIssueSyncBinding(ctx, source.ID)
	require.NoError(t, err)
	third, err := store.CreateProject(ctx, "third-project")
	require.NoError(t, err)
	parent, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: peer.ID, ToIssueID: issue.ID, Type: "parent", Author: "member"})
	require.NoError(t, err)
	replacement, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "replacement", Author: "member"})
	require.NoError(t, err)
	_, err = store.AddLabel(ctx, issue.ID, "existing", "member")
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	adoption := store.(db.FederationAdoptionStore)
	op := db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: store.InstanceUID(), ProjectUID: source.UID, ProjectID: source.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", CatalogName: "hub", Account: "member", ProjectName: source.Name}
	op, err = adoption.BeginFederationAdoption(ctx, op)
	require.NoError(t, err)
	require.Equal(t, "preparing", op.Phase)
	read, err := adoption.FederationAdoption(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, op, read)
	beforeEvents, err := store.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	beforeLabels, err := store.LabelsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	beforeLinks, err := store.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	operations := map[string]func(context.Context) error{
		"project metadata": func(c context.Context) error {
			_, e := store.PatchProjectMetadata(c, db.PatchProjectMetadataIn{ProjectID: source.ID, Actor: "member", Patch: map[string]jsontext.Value{"example": []byte(`true`)}})
			return e
		},
		"issue metadata": func(c context.Context) error {
			_, e := store.PatchIssueMetadata(c, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"example": []byte(`true`)}})
			return e
		},
		"issue edit": func(c context.Context) error {
			_, e := store.EditIssueAtomic(c, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "member", Title: new("blocked")})
			return e
		},
		"issue close": func(c context.Context) error {
			_, _, _, e := store.CloseIssue(c, issue.ID, "done", "member", "completed", nil)
			return e
		},
		"issue delete": func(c context.Context) error {
			_, _, _, e := store.SoftDeleteIssue(c, issue.ID, "member")
			return e
		},
		"initial peer link": func(c context.Context) error {
			_, _, e := store.CreateIssue(c, db.CreateIssueParams{ProjectID: other.ID, Title: "blocked peer", Author: "member", Links: []db.InitialLink{{Type: "blocks", ToNumber: issue.ID}}})
			return e
		},
		"rewrite linked peer": func(c context.Context) error {
			_, e := store.RewriteAuthorIdentity(c, db.RewriteAuthorIdentityParams{ProjectID: other.ID, From: "member", To: "renamed", Actor: "member"})
			return e
		},
		"import": func(c context.Context) error {
			_, _, e := store.ImportBatch(c, db.ImportBatchParams{ProjectID: source.ID, Source: "tracker", Actor: "member", Items: []db.ImportItem{{ExternalID: "first", Title: "blocked", Status: "open", Author: "member", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}}})
			return e
		},
		"legacy enrollment": func(c context.Context) error {
			_, e := store.CreateFederationEnrollment(c, db.CreateFederationEnrollmentParams{ProjectID: &source.ID, SpokeInstanceUID: "00000000000000000000000015", Token: "test-enrollment", Capabilities: "pull", Actor: "member"})
			return e
		},
		"legacy reset":                  func(c context.Context) error { return store.ResetFederatedProject(c, source.ID, 0, 0) },
		"legacy remote materialization": func(c context.Context) error { return store.MaterializeFederatedProject(c, source.ID) },
		"claim admission": func(c context.Context) error {
			_, e := store.AcquireClaim(c, db.AcquireClaimParams{ProjectID: source.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{Holder: "member", HolderInstanceUID: store.InstanceUID()}, ClaimKind: "hard"})
			return e
		},
		"integration refresh": func(c context.Context) error {
			_, e := store.RefreshIssueSyncBinding(c, db.IssueSyncBindingUpdateParams{BindingID: syncBinding.ID, DisplayName: "updated", Config: []byte(`{}`)})
			return e
		},
		"raw label insert": func(c context.Context) error {
			_, e := store.AddLabel(c, issue.ID, "blocked", "member")
			return e
		},
		"raw label delete": func(c context.Context) error { return store.RemoveLabel(c, issue.ID, "existing") },
		"archive linked peer": func(c context.Context) error {
			_, _, e := store.RemoveProject(c, db.RemoveProjectParams{ProjectID: other.ID, Actor: "member", Force: true})
			return e
		},
		"merge linked peer": func(c context.Context) error {
			_, e := store.MergeProjects(c, db.MergeProjectsParams{SourceProjectID: other.ID, TargetProjectID: third.ID, Actor: "member"})
			return e
		},
		"move linked peer": func(c context.Context) error {
			_, e := store.MoveIssueProject(c, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: other.ID, ToProjectID: third.ID, IfMatchRev: peer.Revision, Actor: "member"})
			return e
		},
		"link event insert": func(c context.Context) error {
			_, _, e := store.CreateLinkAndEvent(c, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventIssueID: peer.ID, EventType: "issue.linked", Actor: "member"})
			return e
		},
		"link event delete": func(c context.Context) error {
			_, e := store.DeleteLinkAndEvent(c, link, db.LinkEventParams{EventIssueID: peer.ID, EventType: "issue.unlinked", Actor: "member"})
			return e
		},
		"parent replacement": func(c context.Context) error {
			_, e := store.(interface {
				ReplaceParentAndEvents(context.Context, db.ReplaceParentAndEventsParams) (db.ReplaceParentAndEventsResult, error)
			}).ReplaceParentAndEvents(c, db.ReplaceParentAndEventsParams{ExpectedParentLinkID: parent.ID, ExpectedParentIssueID: issue.ID, Link: db.CreateLinkParams{FromIssueID: peer.ID, ToIssueID: replacement.ID, Type: "parent", Author: "member"}, UnlinkEvent: db.LinkEventParams{EventIssueID: peer.ID, EventType: "issue.unlinked", Actor: "member"}, LinkEvent: db.LinkEventParams{EventIssueID: peer.ID, EventType: "issue.linked", Actor: "member"}})
			return e
		},
		"root authority admission": func(c context.Context) error {
			return store.PinRootAuthority(c, db.RootKeyPin{ProjectUID: source.UID, AuthorityUID: "00000000000000000000000012", KeyID: db.RootPublicKeyID(public), PublicKey: public})
		},
		"cron definition": func(c context.Context) error {
			_, _, e := store.PutCronJob(c, db.PutCronJob{ProjectID: source.ID, Name: "blocked", Actor: "member", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}})
			return e
		},
		"cron observation": func(c context.Context) error {
			_, e := store.ObserveCronRun(c, db.ObserveCronRun{ProjectID: source.ID, UID: "00000000000000000000000017", JobUID: new("00000000000000000000000018"), DefinitionEventUID: new("00000000000000000000000019"), Actor: "member", Status: "running", Summary: cron.Summary{Version: 1}})
			return e
		},
		"issue": func(c context.Context) error {
			_, _, e := store.CreateIssue(c, db.CreateIssueParams{ProjectID: source.ID, Title: "blocked", Author: "member"})
			return e
		},
		"comment": func(c context.Context) error {
			_, _, e := store.CreateComment(c, db.CreateCommentParams{IssueID: issue.ID, Author: "member", Body: "blocked"})
			return e
		},
		"rename": func(c context.Context) error {
			_, e := store.RenameProject(c, source.ID, "changed")
			return e
		},
		"archive": func(c context.Context) error {
			_, _, e := store.RemoveProject(c, db.RemoveProjectParams{ProjectID: source.ID, Actor: "member", Force: true})
			return e
		},
		"merge source": func(c context.Context) error {
			_, e := store.MergeProjects(c, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: other.ID, Actor: "member"})
			return e
		},
		"merge target": func(c context.Context) error {
			_, e := store.MergeProjects(c, db.MergeProjectsParams{SourceProjectID: other.ID, TargetProjectID: source.ID, Actor: "member"})
			return e
		},
		"move out": func(c context.Context) error {
			_, e := store.MoveIssueProject(c, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: other.ID, IfMatchRev: issue.Revision, Actor: "member"})
			return e
		},
		"move in": func(c context.Context) error {
			_, e := store.MoveIssueProject(c, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: other.ID, ToProjectID: source.ID, IfMatchRev: peer.Revision, Actor: "member"})
			return e
		},
		"rewrite": func(c context.Context) error {
			_, e := store.RewriteAuthorIdentity(c, db.RewriteAuthorIdentityParams{ProjectID: source.ID, From: "member", To: "renamed", Actor: "member"})
			return e
		},
		"link insert to": func(c context.Context) error {
			_, e := store.CreateLink(c, db.CreateLinkParams{FromIssueID: peer.ID, ToIssueID: issue.ID, Type: "blocks", Author: "member"})
			return e
		},
		"link insert from": func(c context.Context) error {
			_, e := store.CreateLink(c, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "member"})
			return e
		},
		"link delete": func(c context.Context) error { return store.DeleteLinkByID(c, link.ID) },
		"atomic peer link": func(c context.Context) error {
			_, e := store.EditIssueAtomic(c, db.EditIssueAtomicParams{IssueID: peer.ID, Actor: "member", AddBlocks: []int64{issue.ID}})
			return e
		},
		"atomic peer unlink": func(c context.Context) error {
			_, e := store.EditIssueAtomic(c, db.EditIssueAtomicParams{IssueID: peer.ID, Actor: "member", RemoveBlocks: []int64{issue.ID}})
			return e
		},
		"federation enable": func(c context.Context) error {
			_, e := store.EnableProjectFederation(c, source.ID, "member")
			return e
		},
		"federation leave": func(c context.Context) error {
			_, e := store.LeaveFederationReplica(c, source.ID)
			return e
		},
		"integration admission": func(c context.Context) error {
			_, e := store.UpsertIssueSyncBinding(c, db.UpsertIssueSyncBindingParams{ProjectID: source.ID, Provider: "github", SourceKey: "github:example/repo", RemoteID: "example/repo", DisplayName: "example/repo", Config: []byte(`{}`), IntervalSeconds: 60})
			return e
		},
		"external admission": func(c context.Context) error {
			_, _, e := store.CreateExternalRootBinding(c, db.CreateExternalRootBindingParams{ProjectID: source.ID, IssueID: issue.ID, ConnectorInstance: "example", ExternalRootKey: "root", ExternalAccountKey: "example-account", ReceiveCommentsAfter: time.Now().UTC(), Actor: "member"})
			return e
		},
		"recurrence": func(c context.Context) error {
			_, _, e := store.CreateRecurrence(c, db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "member", Rule: "FREQ=DAILY", DTStart: "2030-01-01", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "scheduled"}})
			return e
		},
	}
	for name, mutate := range operations {
		if !t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, mutate(ctx), db.ErrFederationAdoptionPaused)
			retained, e := adoption.FederationAdoption(ctx, source.ID)
			require.NoError(t, e)
			require.Equal(t, op, retained)
			afterEvents, e := store.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
			require.NoError(t, e)
			require.True(t, reflect.DeepEqual(beforeEvents, afterEvents), "retained history changed")
			retainedIssue, e := store.IssueByID(ctx, issue.ID)
			require.NoError(t, e)
			require.Equal(t, issue, retainedIssue)
			labels, e := store.LabelsByIssue(ctx, issue.ID)
			require.NoError(t, e)
			require.Equal(t, beforeLabels, labels)
			links, e := store.LinksByIssue(ctx, issue.ID)
			require.NoError(t, e)
			require.Equal(t, beforeLinks, links)
			project, e := store.ProjectByID(ctx, source.ID)
			require.NoError(t, e)
			require.Equal(t, source, project)
		}) {
			t.FailNow()
		}
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "allowed", Author: "member"})
	require.NoError(t, err)
	// Even an attachment context never grants ordinary mutation authority.
	_, _, err = store.CreateComment(db.WithFederationAdoptionAttachment(ctx, op), db.CreateCommentParams{IssueID: issue.ID, Author: "member", Body: "blocked"})
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	require.NoError(t, adoption.CancelUnusedFederationAdoption(ctx, op))
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "member", Body: "resumed"})
	require.NoError(t, err)
}

// RunFederationAdoptionFinalization pins exact activated credentials and native
// binding/epoch matching. Ordinary transaction fences cannot release a pause.
func RunFederationAdoptionFinalization(t *testing.T, store db.Storage) {
	ctx := t.Context()
	s := store.(db.FederationAdoptionStore)
	project, err := store.CreateProject(ctx, "finalization-project")
	require.NoError(t, err)
	retainedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "preserved source", Author: "member"})
	require.NoError(t, err)
	op, err := s.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: store.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", Account: "member", CatalogName: "hub", ProjectName: project.Name})
	require.NoError(t, err)
	next := op
	next.Phase = "frozen"
	next.Digest = "opaque-digest"
	next.EnvelopePath = "opaque-path"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
	op = next
	next.Phase = "transmitting"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
	op = next
	next.Phase = "published"
	next.HubProjectID = 42
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
	op = next
	require.Error(t, s.CancelUnusedFederationAdoption(ctx, op), "known publication cannot be cancelled")
	bad := op
	bad.Phase = "rejected"
	bad.HubProjectID = 0
	require.Error(t, s.AdvanceFederationAdoption(ctx, op, bad), "known publication cannot become a rejection")
	next.BindingUID = "00000000000000000000000013"
	next.ResetEpoch = 1
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
	op = next
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: op.TargetOrigin, HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", PushEnabled: true, Enabled: true}
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	attachment := db.WithFederationAdoptionAttachment(ctx, op)
	_, err = store.UpsertFederationBinding(attachment, binding)
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(attachment, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: op.TargetInstanceUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	binding, err = store.SetRelayBindingConfig(attachment, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: op.BindingUID, UpstreamInstanceUID: op.TargetInstanceUID, AuthorityUID: op.TargetInstanceUID, HubPath: []string{op.TargetInstanceUID, store.InstanceUID()}, LocalActor: "member", ResetEpoch: 1})
	require.NoError(t, err)
	next.Phase = "attached"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
	op = next
	released, err := s.FinalizeFederationAdoption(ctx, op, binding)
	require.Error(t, err)
	require.False(t, released)
	generic := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return nil })
	released, err = s.FinalizeFederationAdoption(generic, op, binding)
	require.Error(t, err)
	require.False(t, released)
	calls := 0
	validate := func(_ context.Context, _ db.Transaction, current db.FederationAdoptionOperation, live db.FederationBinding) error {
		calls++
		require.Equal(t, op, current)
		require.Equal(t, binding, live)
		return nil
	}
	for _, change := range []func(*db.FederationAdoptionOperation){func(o *db.FederationAdoptionOperation) { o.OperationUID = "00000000000000000000000014" }, func(o *db.FederationAdoptionOperation) { o.ResetEpoch++ }, func(o *db.FederationAdoptionOperation) { o.BindingUID = "00000000000000000000000015" }} {
		wrong := op
		change(&wrong)
		proof := db.WithFederationAdoptionFinalization(ctx, wrong, binding, validate)
		released, err = s.FinalizeFederationAdoption(proof, op, binding)
		require.Error(t, err)
		require.False(t, released)
	}
	require.Zero(t, calls)
	denied := db.WithFederationAdoptionFinalization(ctx, op, binding, func(context.Context, db.Transaction, db.FederationAdoptionOperation, db.FederationBinding) error {
		return db.ErrNotFound
	})
	released, err = s.FinalizeFederationAdoption(denied, op, binding)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.False(t, released)
	retained, err := s.FederationAdoption(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, op, retained)
	proof := db.WithFederationAdoptionFinalization(ctx, op, binding, validate)
	native := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	// Native seeding retained the original local event. Hop acknowledgement
	// alone still leaves a root receipt owed.
	released, err = s.FinalizeFederationAdoption(proof, op, binding)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	require.False(t, released)
	_, err = native.ExecContext(ctx, `UPDATE federation_relay_outbox SET acknowledged=1 WHERE project_uid=$1`, project.UID)
	require.NoError(t, err)
	released, err = s.FinalizeFederationAdoption(proof, op, binding)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	require.False(t, released)
	// Remove only the test's seeded delivery to isolate the next native blocker.
	_, err = native.ExecContext(ctx, `DELETE FROM federation_relay_outbox WHERE project_uid=$1`, project.UID)
	require.NoError(t, err)
	_, err = native.ExecContext(ctx, `INSERT INTO pending_claim_requests(request_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,claim_kind,requested_at) VALUES($1,$2,$3,$4,'member',$5,'hard',$6)`, "00000000000000000000000016", project.ID, retainedIssue.ID, retainedIssue.UID, store.InstanceUID(), time.Now().UTC())
	require.NoError(t, err)
	released, err = s.FinalizeFederationAdoption(proof, op, binding)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	require.False(t, released)
	_, err = native.ExecContext(ctx, `DELETE FROM pending_claim_requests WHERE project_id=$1`, project.ID)
	require.NoError(t, err)
	// Quarantine is ordinary sync recovery state and must retain the exact marker.
	_, err = store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{ProjectID: project.ID, Direction: "pull", FirstEventID: 1, LastEventID: 1, Error: "rejected batch", CreatedAt: time.Now().UTC(), RelayBindingUID: op.BindingUID, RelayResetEpoch: op.ResetEpoch})
	require.NoError(t, err)
	released, err = s.FinalizeFederationAdoption(proof, op, binding)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByQuarantine)
	require.False(t, released)
	_, err = native.ExecContext(ctx, `DELETE FROM federation_quarantine WHERE project_id=$1`, project.ID)
	require.NoError(t, err)
	// Model the authenticated reset's committed native binding, before its
	// activated credential is refreshed. Only an exact live credential proof
	// may carry the operation forward to that new epoch.
	config := *binding.RelayConfig
	config.ResetEpoch++
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	_, err = native.ExecContext(ctx, `UPDATE federation_bindings SET relay_config=$1 WHERE project_id=$2`, string(raw), project.ID)
	require.NoError(t, err)
	live, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	advanced := op
	advanced.ResetEpoch = config.ResetEpoch
	require.Error(t, s.AdvanceFederationAdoption(ctx, op, advanced))
	require.Error(t, s.AdvanceFederationAdoption(generic, op, advanced))
	staleProof := db.WithFederationAdoptionFinalization(ctx, op, live, validate)
	require.Error(t, s.AdvanceFederationAdoption(staleProof, op, advanced))
	epochProof := db.WithFederationAdoptionFinalization(ctx, advanced, live, func(_ context.Context, _ db.Transaction, operation db.FederationAdoptionOperation, b db.FederationBinding) error {
		require.Equal(t, advanced, operation)
		require.Equal(t, live, b)
		return nil
	})
	require.NoError(t, s.AdvanceFederationAdoption(epochProof, op, advanced))
	op = advanced
	binding = live
	proof = db.WithFederationAdoptionFinalization(ctx, op, binding, validate)
	calls = 0
	released, err = s.FinalizeFederationAdoption(proof, op, binding)
	require.NoError(t, err)
	require.True(t, released)
	require.Equal(t, 1, calls)
	released, err = s.FinalizeFederationAdoption(ctx, op, binding)
	require.NoError(t, err)
	require.False(t, released)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "resumed", Author: "member"})
	require.NoError(t, err)
}

// RunFederationAdoptionCAS checks complete descriptor comparison and safe phases.
func RunFederationAdoptionCAS(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "operation-project")
	require.NoError(t, err)
	s := store.(db.FederationAdoptionStore)
	op, err := s.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: store.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", Account: "member", CatalogName: "hub", ProjectName: project.Name})
	require.NoError(t, err)
	same, err := s.BeginFederationAdoption(ctx, op)
	require.NoError(t, err)
	require.Equal(t, op, same)
	frozen := op
	frozen.Phase = "frozen"
	frozen.EnvelopePath = "opaque-path"
	frozen.Digest = "opaque-digest"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, frozen))
	require.Error(t, s.AdvanceFederationAdoption(ctx, op, frozen), "complete expected descriptor is compared")
	require.NoError(t, s.AdvanceFederationAdoption(ctx, frozen, frozen))
	for i := 0; i < reflect.TypeOf(frozen).NumField(); i++ {
		candidate := frozen
		field := reflect.ValueOf(&candidate).Elem().Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(field.String() + "x")
		case reflect.Int64:
			field.SetInt(field.Int() + 1)
		}
		require.Error(t, s.AdvanceFederationAdoption(ctx, frozen, candidate), reflect.TypeOf(frozen).Field(i).Name)
		retained, e := s.FederationAdoption(ctx, project.ID)
		require.NoError(t, e)
		require.Equal(t, frozen, retained)
	}

	sent := frozen
	sent.Phase = "transmitting"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, frozen, sent))
	require.Error(t, s.CancelUnusedFederationAdoption(ctx, sent))
	rejected := sent
	rejected.Phase = "rejected"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, sent, rejected))
	require.NoError(t, s.CancelUnusedFederationAdoption(ctx, rejected))
	_, err = s.FederationAdoption(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

// RunFederationAdoptionPersistence closes all handles before resuming the pause.
func RunFederationAdoptionPersistence(t *testing.T, store db.Storage, reopen func() db.Storage) {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "persistent-project")
	require.NoError(t, err)
	op, err := store.(db.FederationAdoptionStore).BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: store.InstanceUID(), ProjectUID: p.UID, ProjectID: p.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", CatalogName: "hub", Account: "member", LocalAccount: "local-member", ProjectName: p.Name})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	again := reopen()
	t.Cleanup(func() { _ = again.Close() })
	retained, err := again.(db.FederationAdoptionStore).FederationAdoption(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, op, retained)
	_, _, err = again.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "blocked after restart", Author: "member"})
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	_, err = again.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
}

// RunFederationAdoptionEligibility rejects mutable source authority in Begin.
func RunFederationAdoptionEligibility(t *testing.T, store db.Storage) {
	ctx := t.Context()
	s := store.(db.FederationAdoptionStore)
	for _, kind := range []string{"archive", "integration", "claim", "recurrence", "federation", "root-pin", "retired-root-pin"} {
		t.Run(kind, func(t *testing.T) {
			p, err := store.CreateProject(ctx, kind+"-project")
			require.NoError(t, err)
			switch kind {
			case "archive":
				_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "member"})
			case "integration":
				_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "github", SourceKey: "github:example/repo", RemoteID: "example/repo", DisplayName: "example/repo", Config: []byte(`{}`), IntervalSeconds: 60})
			case "claim":
				issue, _, e := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "claimed", Author: "member"})
				require.NoError(t, e)
				_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: p.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{Holder: "member", HolderInstanceUID: store.InstanceUID()}, ClaimKind: "hard"})
			case "recurrence":
				_, _, err = store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: p.ID, Actor: "member", Rule: "FREQ=DAILY", DTStart: "2030-01-01", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "scheduled"}})
			case "federation":
				_, err = store.EnableProjectFederation(ctx, p.ID, "member")
			case "root-pin", "retired-root-pin":
				public, _, e := ed25519.GenerateKey(nil)
				require.NoError(t, e)
				err = store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: "00000000000000000000000012", KeyID: db.RootPublicKeyID(public), PublicKey: public})
				require.NoError(t, err)
				if kind == "retired-root-pin" {
					_, err = store.(interface {
						ExecContext(context.Context, string, ...any) (sql.Result, error)
					}).ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=$1`, p.UID)
				}
			}
			require.NoError(t, err)
			_, err = s.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: "00000000000000000000000011", SourceInstanceUID: store.InstanceUID(), ProjectUID: p.UID, ProjectID: p.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", CatalogName: "hub", Account: "member", ProjectName: p.Name})
			require.ErrorIs(t, err, db.ErrFederationAdoptionConflict)
			_, err = s.FederationAdoption(ctx, p.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

// adoptionAttachedFixture uses only native setup APIs, before a signed reset.
func adoptionAttachedFixture(t *testing.T, store db.Storage) (*rootResetFixture, db.FederationAdoptionOperation, db.FederationBinding) {
	t.Helper()
	ctx := t.Context()
	fixtureUID, err := uid.New()
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "recovery-project-"+fixtureUID)
	require.NoError(t, err)
	s := store.(db.FederationAdoptionStore)
	op, err := s.BeginFederationAdoption(ctx, db.FederationAdoptionOperation{OperationUID: fixtureUID, SourceInstanceUID: store.InstanceUID(), ProjectUID: project.UID, ProjectID: project.ID, TargetInstanceUID: "00000000000000000000000012", TargetOrigin: "https://hub.example", CatalogName: "hub", Account: "member", ProjectName: project.Name})
	require.NoError(t, err)
	for _, phase := range []string{"frozen", "transmitting", "published", "published"} {
		next := op
		next.Phase = phase
		next.EnvelopePath, next.Digest = "opaque-path", "opaque-digest"
		if phase == "published" {
			next.HubProjectID = 42
			if op.Phase == "published" {
				next.BindingUID, next.ResetEpoch = bindingUID, 1
			}
		}
		require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
		op = next
	}
	attachment := db.WithFederationAdoptionAttachment(ctx, op)
	_, err = store.UpsertFederationBinding(attachment, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: op.TargetOrigin, HubProjectID: op.HubProjectID, HubProjectUID: project.UID, Actor: op.Account, Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: op.TargetInstanceUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(attachment, pin))
	binding, err := store.SetRelayBindingConfig(attachment, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: op.BindingUID, UpstreamInstanceUID: op.TargetInstanceUID, AuthorityUID: op.TargetInstanceUID, HubPath: []string{op.TargetInstanceUID, store.InstanceUID()}, LocalActor: "member", ResetEpoch: 1})
	require.NoError(t, err)
	next := op
	next.Phase = "attached"
	require.NoError(t, s.AdvanceFederationAdoption(ctx, op, next))
	return &rootResetFixture{t: t, project: project, pin: pin, private: private, rootUID: op.TargetInstanceUID, relayUID: store.InstanceUID(), binding: op.BindingUID}, next, binding
}

// RunFederationAdoptionResetRecovery drains authenticated new-epoch sync while
// the durable operation still requires its activated-credential epoch proof.
func RunFederationAdoptionResetRecovery(t *testing.T, store db.Storage) {
	ctx := t.Context()
	root, op, _ := adoptionAttachedFixture(t, store)
	s := store.(db.FederationAdoptionStore)
	kept := root.created("Checkpoint issue", 300, 1)
	manifest, snapshot, translation := root.checkpoint(2, 2, kept)
	require.NoError(t, store.(db.RelayResetStore).InstallRelayReset(ctx, op.BindingUID, manifest, snapshot, translation))
	live, err := store.FederationBindingByProject(ctx, op.ProjectID)
	require.NoError(t, err)
	require.Equal(t, int64(2), live.RelayConfig.ResetEpoch)
	retained, err := s.FederationAdoption(ctx, op.ProjectID)
	require.NoError(t, err)
	require.Equal(t, op, retained, "signed reset cannot claim activated credentials for the operation")
	late := root.created("New epoch issue", 301, 2)
	root.deliverIssue(ctx, store, late, 2, 1)
	require.NoError(t, store.MaterializeFederatedProject(ctx, op.ProjectID))
	issue, err := store.IssueByUID(ctx, late.entity.EntityUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "New epoch issue", issue.Title)
	require.Equal(t, "verified", issue.Verification)
	// A current binding epoch alone is not reset proof. Retained proof must
	// still name this exact project, hop, receiver and signed checkpoint.
	native := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	key := db.RelayResetMetadataPrefix + op.ProjectUID
	var raw string
	require.NoError(t, native.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw))
	for _, kind := range []string{"missing", "signature", "epoch", "binding", "sender", "receiver", "project", "snapshot"} {
		if !t.Run(kind, func(t *testing.T) {
			var checkpoint db.RelayResetCheckpoint
			require.NoError(t, json.Unmarshal([]byte(raw), &checkpoint))
			switch kind {
			case "signature":
				checkpoint.Manifest.Signature[0] ^= 1
			case "epoch":
				checkpoint.Translation.Authority.Epoch++
			case "binding":
				checkpoint.Translation.Authority.BindingUID = "00000000000000000000000014"
			case "sender":
				checkpoint.Translation.Authority.SenderInstanceUID = "00000000000000000000000014"
			case "receiver":
				checkpoint.Translation.Authority.ReceiverInstanceUID = "00000000000000000000000014"
			case "project":
				checkpoint.Translation.Authority.ProjectUID = "00000000000000000000000014"
			case "snapshot":
				checkpoint.Snapshot.Events = []byte(`[]`)
			}
			if kind == "missing" {
				_, err = native.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, key)
			} else {
				changed, e := json.Marshal(checkpoint)
				require.NoError(t, e)
				_, err = native.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(changed), key)
			}
			require.NoError(t, err)
			require.ErrorIs(t, store.MaterializeFederatedProject(ctx, op.ProjectID), db.ErrFederationAdoptionPaused)
			_, err = native.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, raw)
			require.NoError(t, err)
		}) {
			t.FailNow()
		}
	}
	// Recovery must also work between reset installation and the final
	// operation epoch advance, when its exact-expected config is newer.
	revoked := *live.RelayConfig
	revoked.UpstreamRevoked = true
	_, err = store.SetRelayBindingConfig(ctx, op.ProjectID, revoked)
	require.NoError(t, err)
	recovered := revoked
	recovered.UpstreamRevoked = false
	live, err = store.SetRelayBindingConfig(ctx, op.ProjectID, recovered, revoked)
	require.NoError(t, err)
	retained, err = s.FederationAdoption(ctx, op.ProjectID)
	require.NoError(t, err)
	require.Equal(t, op, retained)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: op.ProjectID, Title: "ordinary blocked", Author: "member"})
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	advanced := op
	advanced.ResetEpoch = 2
	require.Error(t, s.AdvanceFederationAdoption(ctx, op, advanced))
	released, err := s.FinalizeFederationAdoption(ctx, op, live)
	require.Error(t, err)
	require.False(t, released)
	generic := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return nil })
	require.Error(t, s.AdvanceFederationAdoption(generic, op, advanced))
	proof := db.WithFederationAdoptionFinalization(ctx, advanced, live, func(_ context.Context, _ db.Transaction, current db.FederationAdoptionOperation, binding db.FederationBinding) error {
		require.Equal(t, advanced, current)
		require.Equal(t, live, binding)
		return nil
	})
	require.NoError(t, s.AdvanceFederationAdoption(proof, op, advanced))
	released, err = s.FinalizeFederationAdoption(proof, advanced, live)
	require.NoError(t, err)
	require.True(t, released)
}

// RunFederationAdoptionRevocationRecovery preserves the native exact-expected
// upstream recovery path without granting general binding reconfiguration.
func RunFederationAdoptionRevocationRecovery(t *testing.T, store db.Storage) {
	for _, name := range []string{"observe", "recover"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, op, binding := adoptionAttachedFixture(t, store)
			revoked := *binding.RelayConfig
			revoked.UpstreamRevoked = true
			markCtx := ctx
			if name == "recover" {
				markCtx = db.WithFederationAdoptionAttachment(ctx, op)
			}
			binding, err := store.SetRelayBindingConfig(markCtx, op.ProjectID, revoked)
			require.NoError(t, err, "observing upstream revocation must retain the pause")
			if name == "recover" {
				recovered := revoked
				recovered.UpstreamRevoked = false
				binding, err = store.SetRelayBindingConfig(ctx, op.ProjectID, recovered, revoked)
				require.NoError(t, err, "the existing exact-expected recovery call must remain usable")
				require.False(t, binding.RelayConfig.UpstreamRevoked)
				_, err = store.SetRelayBindingConfig(ctx, op.ProjectID, recovered, revoked)
				require.ErrorIs(t, err, db.ErrRemoteEventConflict, "stale recovery cannot overwrite current state")
				changed := recovered
				changed.ServeDownstream = true
				_, err = store.SetRelayBindingConfig(ctx, op.ProjectID, changed, recovered)
				require.ErrorIs(t, err, db.ErrFederationAdoptionPaused, "exact-expected recovery cannot change other configuration")
			}
			retained, err := store.(db.FederationAdoptionStore).FederationAdoption(ctx, op.ProjectID)
			require.NoError(t, err)
			require.Equal(t, op, retained)
			_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: op.ProjectID, Title: "still paused", Author: "member"})
			require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
		})
	}
}
