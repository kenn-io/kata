package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestFederationStatusEmbeddingPresentation(t *testing.T) {
	status := api.FederationProjectStatus{ProjectName: "shared-project", Role: "spoke", Embedding: &api.FederationEmbeddingStatus{
		State: "reused", ArtifactLimit: 32, Limited: true,
		Producer:  &db.ProjectEmbeddingProducer{ProducerInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", Recipe: embedding.ArtifactIdentity{Model: "example-model", Dimensions: 2}},
		Artifacts: []api.FederationEmbeddingArtifactStatus{{State: "generated"}, {State: "reused"}, {State: "incompatible"}, {State: "stored_unindexed"}},
	}}
	for _, mode := range []string{"human", "agent"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			body := api.FederationStatusBody{Statuses: []api.FederationProjectStatus{status}}
			if mode == "human" {
				require.NoError(t, printFederationStatus(cmd, body))
				require.Contains(t, out.String(), "embedding: reused")
				require.Contains(t, out.String(), "embedding producer: 01HZNQ7VFPK1XGD8R5MABCD4EA")
				require.Contains(t, out.String(), "embedding recipe: example-model / 2 dimensions")
				require.Contains(t, out.String(), "4 retained / 32 limit (limited)")
			} else {
				require.NoError(t, printFederationStatusAgent(cmd, body))
				require.Contains(t, out.String(), "embedding_project=shared-project")
				require.Contains(t, out.String(), "state=reused")
				require.Contains(t, out.String(), "producer=01HZNQ7VFPK1XGD8R5MABCD4EA")
				require.Contains(t, out.String(), "retained=4 limit=32 limited=true")
			}
			for _, count := range []string{"generated=1", "reused=1", "incompatible=1", "stored_unindexed=1"} {
				require.Contains(t, out.String(), count)
			}
		})
	}
}
