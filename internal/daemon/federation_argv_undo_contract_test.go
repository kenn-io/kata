package daemon_test

import (
	"context"
	"testing"

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
