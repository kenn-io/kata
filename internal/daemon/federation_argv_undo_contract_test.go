package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type refusalAfterRekeyStore struct {
	db.Storage
	beforeRefusal func()
}

func (s refusalAfterRekeyStore) AdoptProjectIntoFederation(_ context.Context, _ db.AdoptProjectIntoFederationParams) (db.AdoptProjectIntoFederationResult, error) {
	s.beforeRefusal()
	return db.AdoptProjectIntoFederationResult{}, db.ErrUnsupportedEventFeatures
}

type contextCredentialStore struct{ *replicaCredentialStore }

func (s contextCredentialStore) RekeyFederationCredential(ctx context.Context, p config.FederationCredentialRekey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.replicaCredentialStore.RekeyFederationCredential(ctx, p)
}

func (s contextCredentialStore) RekeyFederationCredentialWithUndo(ctx context.Context, p config.FederationCredentialRekey) (*config.FederationCredentialRekeyUndo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.replicaCredentialStore.RekeyFederationCredentialWithUndo(ctx, p)
}

func (s contextCredentialStore) UndoFederationCredentialRekey(ctx context.Context, undo config.FederationCredentialRekeyUndo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.replicaCredentialStore.UndoFederationCredentialRekey(ctx, undo)
}

func TestAttachEmptyWorkflowRefusalPreservesEqualTargetCredential(t *testing.T) {
	ctx := t.Context()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	_, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "argv", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "run", Kind: "argv", Argv: []string{"printf", ""}}}}})
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	params := replicaServiceParams()
	params.ProjectName, params.AttachEmpty = project.Name, true
	params.Credential.Capabilities = "pull,push"
	credentials.put(project.UID, params.Credential)
	credentials.put(replicaHubProjectUID, params.Credential)
	_, err = daemon.EnsureFederationReplica(ctx, store, credentials, nil, params)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	for _, uid := range []string{project.UID, replicaHubProjectUID} {
		got, found, err := credentials.FederationCredential(ctx, uid)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, params.Credential, got)
	}
	require.Zero(t, credentials.rekeyCalls)
}

func TestRefusedAdoptionUndoPreservesCredentialState(t *testing.T) {
	for _, scenario := range []string{"absent target", "equal target", "source changed", "target changed", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
			require.NoError(t, err)
			raw := newReplicaCredentialStore()
			credentials := contextCredentialStore{raw}
			params := replicaServiceParams()
			params.ProjectName, params.AdoptExisting = project.Name, true
			params.Credential.Capabilities = "pull,push"
			original := params.Credential
			raw.put(project.UID, original)
			if scenario == "equal target" {
				raw.put(replicaHubProjectUID, original)
			}
			changed := original
			changed.Token = "concurrent-token"
			wrapped := refusalAfterRekeyStore{Storage: store, beforeRefusal: func() {
				switch scenario {
				case "source changed":
					raw.put(project.UID, changed)
				case "target changed":
					raw.put(replicaHubProjectUID, changed)
				case "cancelled":
					cancel()
				}
			}}
			_, err = daemon.EnsureFederationReplica(ctx, wrapped, credentials, nil, params)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			if scenario == "source changed" || scenario == "target changed" {
				require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
			}
			// Read using a live context to inspect state even when the caller cancelled.
			readCtx := context.WithoutCancel(ctx)
			source, sourceFound, err := credentials.FederationCredential(readCtx, project.UID)
			require.NoError(t, err)
			target, targetFound, err := credentials.FederationCredential(readCtx, replicaHubProjectUID)
			require.NoError(t, err)
			switch scenario {
			case "source changed":
				require.True(t, sourceFound)
				require.Equal(t, changed, source)
				require.True(t, targetFound)
				require.Equal(t, original, target)
			case "target changed":
				require.False(t, sourceFound)
				require.True(t, targetFound)
				require.Equal(t, changed, target)
			default:
				require.True(t, sourceFound)
				require.Equal(t, original, source)
				require.Equal(t, scenario == "equal target", targetFound)
				if targetFound {
					require.Equal(t, original, target)
				}
			}
		})
	}
}

// An owner backup may retain a future workflow event without a projection that
// this version can represent. Adoption must undo the credential move if the
// strict retained-history check refuses that event.
func TestAdoptionRefusalRestoresCredentialsForFutureWorkflowHistory(t *testing.T) {
	ctx := t.Context()
	store := openReplicaServiceStore(t)
	const created = "2026-07-15T12:00:00.000Z"
	projectRecord := &db.ProjectExport{ID: 5, UID: replicaLocalProjectUID, Name: "spoke-project", CreatedAt: created, Metadata: jsontext.Value(`{}`), Revision: 1}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(db.CronDefinitionEvent{
		UID: "01HZNQ7VFPK1XGD8R5MABCD4EB", ProjectUID: projectRecord.UID,
		Name: "future", Author: "worker", CreatedAt: now, UpdatedAt: now,
		Definition: jsontext.Value(`{"version":2,"steps":[]}`),
	})
	require.NoError(t, err)
	event := &db.EventExport{
		ID: 10, UID: "01HZNQ7VFPK1XGD8R5MABCD4EC", OriginInstanceUID: store.InstanceUID(),
		ProjectID: projectRecord.ID, ProjectUID: projectRecord.UID, ProjectName: projectRecord.Name,
		Type: "cron.workflow.created", Actor: "worker", Payload: payload,
		HLCPhysicalMS: 1784102400000, CreatedAt: created,
	}
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{
		UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID,
		ProjectName: event.ProjectName, Type: event.Type, Actor: event.Actor,
		HLCPhysicalMS: event.HLCPhysicalMS, CreatedAt: event.CreatedAt, Payload: event.Payload,
	})
	require.NoError(t, err)
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{projectRecord, event}, db.ImportOptions{}))
	project, err := store.ProjectByUID(ctx, projectRecord.UID)
	require.NoError(t, err)
	before, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Equal(t, event.ContentHash, before[0].ContentHash)
	_, err = store.FederationBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)

	credentials := newReplicaCredentialStore()
	params := replicaServiceParams()
	params.ProjectName, params.AdoptExisting = project.Name, true
	params.Credential.Capabilities = "pull,push"
	credentials.put(project.UID, params.Credential)
	credentials.put(replicaHubProjectUID, params.Credential)
	_, err = daemon.EnsureFederationReplica(ctx, store, credentials, nil, params)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	require.Equal(t, 1, credentials.rekeyCalls)

	current, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, project, current)
	after, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = store.FederationBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	for _, uid := range []string{project.UID, replicaHubProjectUID} {
		got, found, err := credentials.FederationCredential(ctx, uid)
		require.NoError(t, err)
		require.True(t, found, "credential for %s", uid)
		require.Equal(t, params.Credential, got)
	}
}
