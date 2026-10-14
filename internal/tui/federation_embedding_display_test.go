package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestFederationDetailShowsEmbeddingReuseAndProducer(t *testing.T) {
	defer snapshotInit(t)()
	status := federationStatusFixture("shared-project", "spoke")
	status.Embedding = &api.FederationEmbeddingStatus{
		State: "reused", ArtifactLimit: 32, Limited: true,
		Producer:  &db.ProjectEmbeddingProducer{ProducerInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", Recipe: embedding.ArtifactIdentity{Model: "example-model", Dimensions: 2}},
		Artifacts: []api.FederationEmbeddingArtifactStatus{{State: "generated"}, {State: "reused"}, {State: "incompatible"}, {State: "stored_unindexed"}},
	}
	model := setupFederationViewWithStatuses(status)
	model.federation.mode = federationModeDetail
	model.federation.cursor = 0
	model.width, model.height = 100, 80
	page := stripANSI(renderFederation(model))
	for _, want := range []string{"embedding: reused", "embedding producer: 01HZNQ7VFPK1XGD8R5MABCD4EA", "embedding recipe: example-model / 2 dimensions", "generated=1", "reused=1", "incompatible=1", "stored_unindexed=1", "4 retained / 32 limit (limited)"} {
		require.Contains(t, page, want)
	}
	t.Logf("Federation detail:\n%s", page)
}

func TestFederationEmbeddingLinesWrapAndKeepUnavailableProducerExplicit(t *testing.T) {
	status := &api.FederationEmbeddingStatus{State: "waiting", ArtifactLimit: 32,
		Producer:  &db.ProjectEmbeddingProducer{ProducerInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", Recipe: embedding.ArtifactIdentity{Model: "example-model", Dimensions: 4001}},
		Artifacts: []api.FederationEmbeddingArtifactStatus{{State: "stored_unindexed"}},
	}
	for _, width := range []int{80, 32} {
		lines := federationEmbeddingLines(status, width)
		for _, line := range lines {
			require.LessOrEqual(t, runewidth.StringWidth(line), width)
		}
		require.Contains(t, strings.Join(lines, ""), "stored_unindexed=1")
		t.Logf("%d-column embedding status:\n%s", width, strings.Join(lines, "\n"))
	}
	lines := federationEmbeddingLines(&api.FederationEmbeddingStatus{State: "unconfigured", ArtifactLimit: 32}, 80)
	require.Contains(t, lines, "embedding producer: none")
	require.NotContains(t, strings.Join(lines, "\n"), "(limited)")
}
