package federation_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/pkg/federationprovider"
)

type intentChangeFixture struct {
	store       db.Storage
	credentials config.FederationManagedCredentialStore
	hub         *fakeHub
	catalog     config.CatalogDaemonConfig
	mapping     config.FederationProjectConfig
	log         string
}

func newIntentChangeFixture(t *testing.T, intent federationprovider.Intent) *intentChangeFixture {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
	t.Setenv("KATA_TEST_PROVIDER_INTENT_CHANGE", "1")
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
	t.Setenv("KATA_TEST_PROVIDER_RELEASE_DECISION", "released")
	log := filepath.Join(t.TempDir(), "provider-requests.jsonl")
	t.Setenv("KATA_TEST_PROVIDER_LOG", log)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"project_id": 42, "project_uid": hubProjectUID, "project_name": "hub-project",
			"replay_horizon_event_id": 9, "baseline_through_event_id": 12,
		}))
	}))
	t.Cleanup(server.Close)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	executable, err := os.Executable()
	require.NoError(t, err)
	catalog := config.CatalogDaemonConfig{Name: "team-hub", URL: server.URL + "/tasks"}
	return &intentChangeFixture{
		store: openReconcileStore(t), credentials: config.DefaultFederationCredentialStore(), hub: newFakeHub(),
		catalog: catalog, log: log,
		mapping: config.FederationProjectConfig{
			Hub: catalog.Name, SpokeProject: "spoke-project", HubProject: "hub-project", Intent: intent,
			CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
		},
	}
}

func (f *intentChangeFixture) reconcile(t *testing.T, intent federationprovider.Intent) error {
	t.Helper()
	f.mapping.Intent = intent
	return federation.ReconcileMapping(t.Context(), f.store, f.credentials, f.hub, f.catalog, f.mapping, nil)
}

func (f *intentChangeFixture) saved(t *testing.T) config.FederationManagedCredentialReservation {
	t.Helper()
	saved, found, err := f.credentials.FindManagedFederationCredential(t.Context(), f.mapping.SpokeProject)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, saved.Credential.Provider)
	return saved
}

func (f *intentChangeFixture) binding(t *testing.T) (db.Project, db.FederationBinding) {
	t.Helper()
	project, err := f.store.ProjectByName(t.Context(), f.mapping.SpokeProject)
	require.NoError(t, err)
	binding, err := f.store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	return project, binding
}

func (f *intentChangeFixture) requests(t *testing.T) []federationprovider.Request {
	t.Helper()
	file, err := os.Open(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	var out []federationprovider.Request
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var request federationprovider.Request
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
		out = append(out, request)
	}
	require.NoError(t, scanner.Err())
	return out
}

func TestReconcileProviderUpgradesReadOnlyReplicaInPlace(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentReadOnly)
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	before := f.saved(t)
	project, binding := f.binding(t)
	require.False(t, binding.PushEnabled)
	require.NoError(t, f.store.AdvanceFederationPullCursor(t.Context(), project.ID, 30))

	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))

	after := f.saved(t)
	upgradedProject, upgraded := f.binding(t)
	assert.Equal(t, project.ID, upgradedProject.ID, "the local project is kept")
	assert.Equal(t, hubProjectUID, upgradedProject.UID)
	assert.True(t, upgraded.PushEnabled, "collaborate capabilities turn push on")
	assert.Equal(t, int64(30), upgraded.PullCursorEventID, "the binding keeps its pull progress")
	assert.Equal(t, federationprovider.IntentCollaborate, after.Credential.Provider.Intent)
	assert.Equal(t, federationprovider.StatusReady, after.Credential.Provider.Status)
	assert.NotEqual(t, before.Credential.Provider.RequestID, after.Credential.Provider.RequestID)
	assert.False(t, before.Credential.Token == after.Credential.Token, "the new request approves a new token")
	assert.Equal(t, "claim,pull,push", after.Credential.Capabilities)
	assert.NotEqual(t, before.Credential.Actor, after.Credential.Actor, "the new approval names a new actor")
	assert.Equal(t, after.Credential.Actor, upgraded.Actor, "the new actor replaces the bound actor")
	assert.Equal(t, binding.Actor, after.Credential.Provider.PreviousActor)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "First local task", Author: "someone"})
	require.NoError(t, err)
	assert.Equal(t, after.Credential.Actor, issue.Author, "local changes are authored as the new actor")

	requests := f.requests(t)
	require.Len(t, requests, 3)
	original := before.Credential.Provider
	assert.Equal(t, "authorize", requests[0].Operation)
	assert.Equal(t, "release", requests[1].Operation)
	assert.Equal(t, original.RequestID, requests[1].RequestID)
	assert.Equal(t, federationprovider.IntentReadOnly, requests[1].Intent, "release names the request it ends")
	assert.Equal(t, "authorize", requests[2].Operation)
	assert.Equal(t, after.Credential.Provider.RequestID, requests[2].RequestID)
	assert.Equal(t, federationprovider.IntentCollaborate, requests[2].Intent)
	assert.Equal(t, original.LocalProjectUID, requests[2].LocalProjectUID, "the new request names the same local project")
	assert.Equal(t, original.SpokeInstanceUID, requests[2].SpokeInstanceUID)
	assert.Equal(t, requests[0].Project, requests[2].Project)

	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	assert.Len(t, f.requests(t), 3, "a ready request is reused after restart")
	assert.Zero(t, f.hub.resolveCalls)
	assert.Empty(t, f.hub.enrollmentCalls)
	_, restarted := f.binding(t)
	assert.Equal(t, after.Credential.Actor, restarted.Actor)
}

func TestReconcileProviderDowngradeWaitsForLocalWork(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentCollaborate)
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	before := f.saved(t)
	project, binding := f.binding(t)
	require.True(t, binding.PushEnabled)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Unpushed local task", Author: "someone"})
	require.NoError(t, err)

	assert.Equal(t, binding.Actor, issue.Author)
	err = f.reconcile(t, federationprovider.IntentReadOnly)
	require.ErrorIs(t, err, federation.ErrBindingConflict, "an unaccepted local change blocks the downgrade")
	assert.Len(t, f.requests(t), 1, "a blocked downgrade does not contact the provider")
	_, blocked := f.binding(t)
	assert.True(t, blocked.PushEnabled, "the replica keeps collaborating")
	assert.True(t, before.Credential.Equal(f.saved(t).Credential), "the approved request is kept")

	accepted, err := f.store.MaxLocalOriginEventID(t.Context(), project.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.AdvanceFederationPushCursor(t.Context(), project.ID, accepted))
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))

	after := f.saved(t)
	downgradedProject, downgraded := f.binding(t)
	assert.Equal(t, project.ID, downgradedProject.ID)
	assert.False(t, downgraded.PushEnabled, "read-only capabilities turn push off")
	assert.Equal(t, accepted, downgraded.PushCursorEventID)
	_, err = f.store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err, "the downgrade keeps local tasks")
	assert.Equal(t, federationprovider.IntentReadOnly, after.Credential.Provider.Intent)
	assert.Equal(t, "pull", after.Credential.Capabilities)
	assert.NotEqual(t, binding.Actor, after.Credential.Actor)
	assert.Equal(t, after.Credential.Actor, downgraded.Actor, "the new actor replaces the bound actor")
	kept, err := f.store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.Actor, kept.Author, "accepted changes keep their author")
	requests := f.requests(t)
	require.Len(t, requests, 3)
	assert.Equal(t, []string{"authorize", "release", "authorize"}, []string{requests[0].Operation, requests[1].Operation, requests[2].Operation})
	assert.Equal(t, before.Credential.Provider.RequestID, requests[1].RequestID)
	assert.Equal(t, federationprovider.IntentReadOnly, requests[2].Intent)
}

func TestReconcileProviderDeniedIntentChangeKeepsReplica(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentReadOnly)
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	project, _ := f.binding(t)

	t.Setenv("KATA_TEST_PROVIDER_DECISION", "denied")
	err := f.reconcile(t, federationprovider.IntentCollaborate)
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrConfigurationConflict)
	denied := f.saved(t)
	assert.Equal(t, federationprovider.StatusDenied, denied.Credential.Provider.Status)
	keptProject, kept := f.binding(t)
	assert.Equal(t, project.ID, keptProject.ID, "a denied request keeps the local project")
	assert.Equal(t, hubProjectUID, kept.HubProjectUID, "a denied request keeps the binding")
	assert.False(t, kept.PushEnabled)

	// Restoring the earlier intent asks again for that access.
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	restored := f.saved(t)
	assert.Equal(t, federationprovider.IntentReadOnly, restored.Credential.Provider.Intent)
	assert.Equal(t, federationprovider.StatusReady, restored.Credential.Provider.Status)
	requests := f.requests(t)
	require.Len(t, requests, 5)
	assert.Equal(t, "release", requests[3].Operation)
	assert.Equal(t, denied.Credential.Provider.RequestID, requests[3].RequestID, "the denied request is released before the next one")
	assert.Equal(t, restored.Credential.Provider.RequestID, requests[4].RequestID)
}

func TestReconcileProviderIntentChangeRefusesAnotherHubProject(t *testing.T) {
	f := newIntentChangeFixture(t, federationprovider.IntentReadOnly)
	require.NoError(t, f.reconcile(t, federationprovider.IntentReadOnly))
	project, before := f.binding(t)

	t.Setenv("KATA_TEST_PROVIDER_PROJECT_UID", recreatedProjectUID)
	err := f.reconcile(t, federationprovider.IntentCollaborate)
	require.ErrorIs(t, err, federation.ErrBindingConflict)
	pending := f.saved(t)
	assert.Empty(t, pending.Credential.Provider.Status, "approval for another project is not saved")
	assert.Equal(t, hubProjectUID, pending.Credential.Provider.HubProjectUID)
	keptProject, kept := f.binding(t)
	assert.Equal(t, project, keptProject)
	assert.Equal(t, before, kept)

	t.Setenv("KATA_TEST_PROVIDER_PROJECT_UID", "")
	require.NoError(t, f.reconcile(t, federationprovider.IntentCollaborate))
	assert.Equal(t, pending.Credential.Provider.RequestID, f.saved(t).Credential.Provider.RequestID, "the retry reuses the saved request")
	_, upgraded := f.binding(t)
	assert.True(t, upgraded.PushEnabled)
	assert.Equal(t, f.saved(t).Credential.Actor, upgraded.Actor)
}
