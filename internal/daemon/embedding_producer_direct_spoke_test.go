package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

// A direct spoke defers generation exactly when its binding is enabled and the
// project's recorded producer is another instance. Every other row keeps the
// existing result.
func TestDirectSpokeGenerationPolicy(t *testing.T) {
	client, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "example-model", Dims: 2})
	require.NoError(t, err)
	other, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "other-model", Dims: 2})
	require.NoError(t, err)
	identity := func(c *embedding.Client) embedding.ArtifactIdentity {
		r, err := c.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		return r
	}
	const (
		kindNone     = "none"
		kindDirect   = "direct"
		kindDisabled = "direct-disabled"

		prodAbsent     = "absent"
		prodSelf       = "self"
		prodRemoteSame = "remote-same-recipe"
		prodRemoteDiff = "remote-other-recipe"
		prodUnreadable = "unreadable"
	)
	cases := []struct {
		kind, producer string
		want           bool
		wantError      bool
	}{}
	for _, kind := range []string{kindNone, kindDirect, kindDisabled} {
		for _, producer := range []string{prodAbsent, prodSelf, prodRemoteSame, prodRemoteDiff, prodUnreadable} {
			want := true
			if kind == kindDirect && (producer == prodRemoteSame || producer == prodRemoteDiff) {
				want = false
			}
			cases = append(cases, struct {
				kind, producer string
				want           bool
				wantError      bool
			}{kind, producer, want, kind == kindDirect && producer == prodRemoteDiff})
		}
	}
	for _, tc := range cases {
		t.Run(tc.kind+"/"+tc.producer, func(t *testing.T) {
			embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				project, err := store.CreateProject(ctx, "spoke-project")
				require.NoError(t, err)
				remote, err := uid.New()
				require.NoError(t, err)
				local := identity(client)
				var raw jsontext.Value
				switch tc.producer {
				case prodSelf:
					raw, err = json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: local})
				case prodRemoteSame:
					raw, err = json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: remote, Recipe: local})
				case prodRemoteDiff:
					raw, err = json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: remote, Recipe: identity(other)})
				}
				require.NoError(t, err)
				if tc.producer == prodUnreadable {
					writeRawProducer(t, store, project, `{"producer_instance_uid":"`+remote+`","recipe":{}}`)
				} else if raw != nil {
					_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "system", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
					require.NoError(t, err)
				}
				if tc.kind != kindNone {
					_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
						ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
						HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: tc.kind != kindDisabled, PushEnabled: true,
					})
					require.NoError(t, err)
				}
				allowed, health, err := daemon.ProjectGenerationAllowedWithConfigForTest(ctx, store, project.UID, local, daemon.ReconcilerConfig{})
				require.NoError(t, err)
				require.Equal(t, tc.want, allowed)
				failure, recorded := health.ProducerErrors[project.UID]
				if !tc.wantError {
					// Relay rows clear the error on success; either way nothing is recorded.
					require.False(t, recorded)
					return
				}
				require.True(t, recorded)
				require.Equal(t, "producer recipe differs from this daemon's embedding recipe", failure.Message)
				selected, err := db.ProjectEmbeddingProducerFromMetadata(mustProject(t, store, project.ID).Metadata)
				require.NoError(t, err)
				require.Equal(t, *selected, failure.Producer)
			})
		})
	}
}

func mustProject(t *testing.T, store db.Storage, id int64) db.Project {
	t.Helper()
	project, err := store.ProjectByID(t.Context(), id)
	require.NoError(t, err)
	return project
}

// A recorded recipe mismatch is cleared once the remote producer's recipe
// matches this daemon's again; a fetch failure is kept.
func TestDirectSpokeRecipeMismatchClearsOnMatch(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		client, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		other, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "other-model", Dims: 2})
		require.NoError(t, err)
		local, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		otherRecipe, err := other.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		project, err := store.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		remote, err := uid.New()
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
			HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true,
		})
		require.NoError(t, err)
		selectRecipe := func(recipe embedding.ArtifactIdentity) {
			raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: remote, Recipe: recipe})
			require.NoError(t, err)
			writeRawProducer(t, store, project, string(raw))
		}
		r := daemon.NewReconcilerForTest(store)
		check := func() daemon.ReconcilerHealth {
			allowed, err := r.ProjectGenerationAllowedForTest(ctx, project.UID, local)
			require.NoError(t, err)
			require.False(t, allowed)
			return r.Health()
		}

		selectRecipe(otherRecipe)
		require.Contains(t, check().ProducerErrors, project.UID)

		selectRecipe(local)
		require.NotContains(t, check().ProducerErrors, project.UID, "the producer now matches")

		fetchFailure := daemon.ProducerFailure{Message: "hub fetch failed", Producer: db.ProjectEmbeddingProducer{ProducerInstanceUID: remote, Recipe: local}}
		r.SetProducerErrorForTest(project.UID, fetchFailure)
		require.Equal(t, fetchFailure, check().ProducerErrors[project.UID], "a fetch failure survives a matching check")
	})
}

// Pausing federation hands generation back to this daemon, so a fetch failure
// recorded while the binding was enabled must not linger in status.
func TestDirectSpokePausedBindingClearsFetchFailure(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		client, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		local, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		project, err := store.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		remote, err := uid.New()
		require.NoError(t, err)
		binding := db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
			HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true,
		}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: remote, Recipe: local})
		require.NoError(t, err)
		writeRawProducer(t, store, project, string(raw))
		r := daemon.NewReconcilerForTest(store)
		r.SetProducerErrorForTest(project.UID, daemon.ProducerFailure{Message: "hub fetch failed"})

		allowed, err := r.ProjectGenerationAllowedForTest(ctx, project.UID, local)
		require.NoError(t, err)
		require.False(t, allowed)
		require.Contains(t, r.Health().ProducerErrors, project.UID)

		binding.Enabled = false
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		allowed, err = r.ProjectGenerationAllowedForTest(ctx, project.UID, local)
		require.NoError(t, err)
		require.True(t, allowed, "a paused binding resumes local generation")
		require.NotContains(t, r.Health().ProducerErrors, project.UID)
	})
}
