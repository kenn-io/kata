package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// An unpinned direct hub records itself as its project's producer, while a
// pin owned by another authority keeps it from selecting itself.
func TestSelectLocalProducerUnpinnedHub(t *testing.T) {
	client, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "example-model", Dims: 2})
	require.NoError(t, err)
	other, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "other-model", Dims: 2})
	require.NoError(t, err)
	recipeOf := func(c *embedding.Client) embedding.ArtifactIdentity {
		r, err := c.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		return r
	}
	cases := []struct {
		name        string
		bind        bool
		disabled    bool
		pinAuth     string // "" none, "self", "other"
		seed        *embedding.ArtifactIdentity
		wantAllowed bool
		wantRecord  bool
		wantEvents  int
	}{
		{name: "unpinned hub records self", bind: true, wantAllowed: true, wantRecord: true, wantEvents: 1},
		{name: "unpinned hub re-records changed recipe", bind: true, seed: new(recipeOf(other)), wantAllowed: true, wantRecord: true, wantEvents: 1},
		{name: "pin owned by another authority", bind: true, pinAuth: "other", wantAllowed: false},
		{name: "pin owned by self", bind: true, pinAuth: "self", wantAllowed: true, wantRecord: true, wantEvents: 1},
		{name: "paused unpinned hub records nothing", bind: true, disabled: true, wantAllowed: true},
		{name: "unbound project", bind: false, wantAllowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				project, err := store.CreateProject(ctx, "hub-project")
				require.NoError(t, err)
				var binding db.FederationBinding
				if tc.bind {
					binding, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: !tc.disabled})
					require.NoError(t, err)
				}
				if tc.pinAuth != "" {
					authority := store.InstanceUID()
					if tc.pinAuth == "other" {
						authority, err = uid.New()
						require.NoError(t, err)
					}
					public, _, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: authority, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				}
				var seeded []byte
				if tc.seed != nil {
					seed := db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: *tc.seed}
					seeded, err = json.Marshal(seed)
					require.NoError(t, err)
					_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "system", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: seeded}})
					require.NoError(t, err)
				}
				project, err = store.ProjectByID(ctx, project.ID)
				require.NoError(t, err)
				before := project.Metadata
				var events []db.Event
				recipe := recipeOf(client)
				var allowed bool
				if tc.bind {
					allowed, err = daemon.SelectLocalProducerForTest(ctx, store, project, binding, recipe, func(e db.Event) { events = append(events, e) })
				} else {
					// An unbound project never reaches selection; the gate allows it.
					allowed, err = daemon.ProjectGenerationAllowedForTest(ctx, store, project.UID, recipe)
				}
				require.NoError(t, err)
				require.Equal(t, tc.wantAllowed, allowed)
				after, err := store.ProjectByID(ctx, project.ID)
				require.NoError(t, err)
				require.Len(t, events, tc.wantEvents)
				for _, e := range events {
					require.Equal(t, "project.metadata_updated", e.Type)
				}
				if !tc.wantRecord {
					require.Equal(t, before, after.Metadata)
					return
				}
				got, err := db.ProjectEmbeddingProducerFromMetadata(after.Metadata)
				require.NoError(t, err)
				require.NotNil(t, got)
				require.Equal(t, store.InstanceUID(), got.ProducerInstanceUID)
				require.Equal(t, recipe, got.Recipe)
			})
		})
	}
}

// idleHubEmbedder encodes without a network and counts encoder calls. With
// portable set it also exposes the artifact recipe, like the real client.
type idleHubEmbedder struct {
	client *embedding.Client
	calls  int
}

func (e *idleHubEmbedder) Generation() kitvec.Generation { return e.client.Generation() }

func (e *idleHubEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		e.calls++
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
}

// portableIdleHubEmbedder adds the recipe accessor the reconciler probes for.
type portableIdleHubEmbedder struct{ *idleHubEmbedder }

func (e portableIdleHubEmbedder) ArtifactIdentity(projectUID, issueUID, producerUID string) (embedding.ArtifactIdentity, error) {
	return e.client.ArtifactIdentity(projectUID, issueUID, producerUID)
}

type idleHubReconcilerEmbedder interface {
	EncodeFunc() kitvec.EncodeFunc
	Generation() kitvec.Generation
}

func newIdleHubEmbedder(t *testing.T, model string, portable bool) (*idleHubEmbedder, idleHubReconcilerEmbedder, embedding.ArtifactIdentity) {
	t.Helper()
	client, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: model, Dims: 2})
	require.NoError(t, err)
	e := &idleHubEmbedder{client: client}
	recipe, err := client.ArtifactIdentity("", "", "")
	require.NoError(t, err)
	if portable {
		return e, portableIdleHubEmbedder{e}, recipe
	}
	return e, e, recipe
}

func idleHubIndex(t *testing.T, store db.Storage) *vector.Index {
	t.Helper()
	ctx := t.Context()
	var ix *vector.Index
	var err error
	if pg, ok := store.(*pgstore.Store); ok {
		ix, err = vector.OpenPostgres(ctx, pg.DB)
		require.NoError(t, err)
		release, err := ix.AcquireReconcilerLease(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, release()) })
	} else {
		ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
		require.NoError(t, err)
	}
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	return ix
}

func hubBinding(t *testing.T, store db.Storage, project db.Project, enabled bool) {
	t.Helper()
	_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: enabled})
	require.NoError(t, err)
}

func patchProducer(t *testing.T, store db.Storage, project db.Project, raw []byte) {
	t.Helper()
	_, err := store.PatchProjectMetadata(t.Context(), db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "system", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
	require.NoError(t, err)
}

// writeRawProducer stores producer JSON the validated patch path would reject,
// like a selection retained from an older recipe version.
func writeRawProducer(t *testing.T, store db.Storage, project db.Project, raw string) {
	t.Helper()
	metadata := `{"` + db.ProjectEmbeddingMetadataKey + `":` + raw + `}`
	var err error
	if pg, ok := store.(*pgstore.Store); ok {
		_, err = pg.ExecContext(t.Context(), `UPDATE projects SET metadata=$1, revision=revision+1 WHERE id=$2`, metadata, project.ID)
	} else {
		_, err = store.(*sqlitestore.Store).ExecContext(t.Context(), `UPDATE projects SET metadata=?, revision=revision+1 WHERE id=?`, metadata, project.ID)
	}
	require.NoError(t, err)
}

func recordedProducer(t *testing.T, store db.Storage, project db.Project) (*db.ProjectEmbeddingProducer, db.Project) {
	t.Helper()
	got, err := store.ProjectByID(t.Context(), project.ID)
	require.NoError(t, err)
	producer, err := db.ProjectEmbeddingProducerFromMetadata(got.Metadata)
	require.NoError(t, err)
	return producer, got
}

// A hub whose content was filled before it was bound still announces itself on
// the next pass, without encoding anything.
func TestIdleHubSelectsItselfWithoutNewContent(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "hub-project")
		require.NoError(t, err)
		for range 3 {
			_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "t", Body: "b", Author: "member"})
			require.NoError(t, err)
		}
		ix := idleHubIndex(t, store)
		counter, emb, recipe := newIdleHubEmbedder(t, "example-model", true)
		require.NoError(t, daemon.ReconcileOnceForTest(ctx, store, ix, emb))
		require.Positive(t, counter.calls)
		filled := counter.calls

		hubBinding(t, store, project, true)
		require.NoError(t, daemon.ReconcileOnceForTest(ctx, store, ix, emb))

		require.Equal(t, filled, counter.calls, "an idle pass must not encode")
		got, _ := recordedProducer(t, store, project)
		require.NotNil(t, got)
		require.Equal(t, store.InstanceUID(), got.ProducerInstanceUID)
		require.Equal(t, recipe, got.Recipe)
	})
}

// A selection that already matches the configured recipe is left untouched.
func TestIdleHubSelectionSkipsMatchingSelection(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "hub-project")
		require.NoError(t, err)
		hubBinding(t, store, project, true)
		_, emb, recipe := newIdleHubEmbedder(t, "example-model", true)
		raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: recipe})
		require.NoError(t, err)
		patchProducer(t, store, project, raw)
		_, before := recordedProducer(t, store, project)

		require.NoError(t, daemon.ReconcileOnceForTest(ctx, store, idleHubIndex(t, store), emb))

		_, after := recordedProducer(t, store, project)
		require.Equal(t, before.Revision, after.Revision)
		require.Equal(t, before.Metadata, after.Metadata)
	})
}

// conflictOnceStore changes project metadata between the idle pass reading a
// project and writing its selection.
type conflictOnceStore struct {
	db.Storage
	armed    bool
	injected bool
}

func (s *conflictOnceStore) ListFederationBindings(ctx context.Context) ([]db.FederationBinding, error) {
	bindings, err := s.Storage.ListFederationBindings(ctx)
	s.armed = !s.injected
	return bindings, err
}

func (s *conflictOnceStore) ProjectByID(ctx context.Context, id int64) (db.Project, error) {
	project, err := s.Storage.ProjectByID(ctx, id)
	if err != nil || !s.armed {
		return project, err
	}
	s.armed, s.injected = false, true
	_, err = s.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: id, Actor: "member", Patch: map[string]jsontext.Value{"example_key": jsontext.Value(`"value"`)}})
	return project, err
}

func TestIdleHubSelectionConflictRetries(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "hub-project")
		require.NoError(t, err)
		hubBinding(t, store, project, true)
		_, emb, recipe := newIdleHubEmbedder(t, "example-model", true)
		ix := idleHubIndex(t, store)
		wrapped := &conflictOnceStore{Storage: store}

		require.NoError(t, daemon.ReconcileOnceForTest(ctx, wrapped, ix, emb))
		require.True(t, wrapped.injected)
		got, _ := recordedProducer(t, store, project)
		require.Nil(t, got, "the conflicted pass must not record")

		require.NoError(t, daemon.ReconcileOnceForTest(ctx, wrapped, ix, emb))
		got, _ = recordedProducer(t, store, project)
		require.NotNil(t, got)
		require.Equal(t, recipe, got.Recipe)
	})
}

// Without a portable recipe the reconciler has nothing to announce.
func TestIdleHubSelectionWithoutPortableRecipe(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "hub-project")
		require.NoError(t, err)
		hubBinding(t, store, project, true)
		_, before := recordedProducer(t, store, project)
		_, emb, _ := newIdleHubEmbedder(t, "example-model", false)

		require.NoError(t, daemon.ReconcileOnceForTest(ctx, store, idleHubIndex(t, store), emb))

		got, after := recordedProducer(t, store, project)
		require.Nil(t, got)
		require.Equal(t, before.Revision, after.Revision)
	})
}

// The idle pass selects only when this root owns, or may take over, the choice.
func TestIdleHubSelectionRows(t *testing.T) {
	other, err := uid.New()
	require.NoError(t, err)
	type row struct {
		name       string
		disabled   bool
		archive    bool
		seed       func(self string, old embedding.ArtifactIdentity) string // producer JSON; "" for none
		raw        bool                                                     // write seed without validation
		wantRecord bool                                                     // selection names self with the configured recipe
	}
	validJSON := func(instance string, recipe embedding.ArtifactIdentity) string {
		raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: instance, Recipe: recipe})
		require.NoError(t, err)
		return string(raw)
	}
	rows := []row{
		{name: "absent selection records self", wantRecord: true},
		{name: "self with stale recipe re-records", seed: func(self string, old embedding.ArtifactIdentity) string { return validJSON(self, old) }, wantRecord: true},
		{name: "unreadable selection naming self is replaced", seed: func(self string, _ embedding.ArtifactIdentity) string {
			return `{"producer_instance_uid":"` + self + `","recipe":{}}`
		}, raw: true, wantRecord: true},
		{name: "unreadable selection naming another instance is kept", seed: func(string, embedding.ArtifactIdentity) string {
			return `{"producer_instance_uid":"` + other + `","recipe":{}}`
		}, raw: true},
		{name: "another producer is kept", seed: func(_ string, old embedding.ArtifactIdentity) string { return validJSON(other, old) }},
		{name: "disabled binding is skipped", disabled: true},
		{name: "archived project is skipped", archive: true},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
				ctx := t.Context()
				project, err := store.CreateProject(ctx, "hub-project")
				require.NoError(t, err)
				hubBinding(t, store, project, !tc.disabled)
				_, _, oldRecipe := newIdleHubEmbedder(t, "old-model", true)
				if tc.seed != nil {
					seed := tc.seed(store.InstanceUID(), oldRecipe)
					if tc.raw {
						writeRawProducer(t, store, project, seed)
					} else {
						patchProducer(t, store, project, []byte(seed))
					}
				}
				if tc.archive {
					_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "member", Force: true})
					require.NoError(t, err)
				}
				before := projectIncludingArchived(t, store, project)
				_, emb, recipe := newIdleHubEmbedder(t, "example-model", true)

				require.NoError(t, daemon.ReconcileOnceForTest(ctx, store, idleHubIndex(t, store), emb))

				after := projectIncludingArchived(t, store, project)
				if !tc.wantRecord {
					require.Equal(t, before.Metadata, after.Metadata)
					return
				}
				got, err := db.ProjectEmbeddingProducerFromMetadata(after.Metadata)
				require.NoError(t, err)
				require.NotNil(t, got)
				require.Equal(t, store.InstanceUID(), got.ProducerInstanceUID)
				require.Equal(t, recipe, got.Recipe)
			})
		})
	}
}

func projectIncludingArchived(t *testing.T, store db.Storage, project db.Project) db.Project {
	t.Helper()
	all, err := store.ListProjectsIncludingArchived(t.Context())
	require.NoError(t, err)
	for _, p := range all {
		if p.ID == project.ID {
			return p
		}
	}
	require.FailNow(t, "project not found")
	return db.Project{}
}
