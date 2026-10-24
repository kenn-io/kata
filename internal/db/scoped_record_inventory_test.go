package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedRecordInventoryBaselineAndEffects(t *testing.T) {
	checkpoint, _, _, _ := verificationCheckpoint(t)
	checkpoint.Cut.Sequence = 7
	checkpoint.Collections.Projects = []ScopedProjectState{{UID: verificationID(1), Name: "baseline"}}
	reference := ScopedReference{Kind: "parent", Slot: "parent", Subject: ScopedEndpoint{verificationID(1), verificationID(40)}, Revision: 2, Signatures: []ScopedSignature{{ProjectUID: verificationID(1), KeyID: "nested", Signature: "retained"}}}
	checkpoint.Collections.References = []ScopedReference{reference}
	baseline, err := ScopedCheckpointRecordEffects(checkpoint)
	require.NoError(t, err)
	require.Len(t, baseline, 2)
	for _, effect := range baseline {
		require.Equal(t, int64(7), effect.Revision)
		require.Equal(t, "replace", effect.Action)
		if effect.Reference != nil {
			require.Equal(t, reference, *effect.Reference)
		}
	}
	effect := ScopedEffect{Collection: "projects", Key: verificationID(1), Revision: 8, Action: "replace", Project: &ScopedProjectState{UID: verificationID(1), Name: "latest"}}
	batch := ScopedBatch{View: checkpoint.View, Cut: ScopedCut{Sequence: 8, Digest: verificationDigest("batch"), CaptureUID: verificationID(72)}, PreviousDigest: checkpoint.Cut.Digest, Effects: []ScopedEffect{effect}}
	inventory, err := ScopedRecordInventory(checkpoint, []ScopedBatch{batch})
	require.NoError(t, err)
	require.Len(t, inventory, 2)
	require.Equal(t, effect, inventory[0])
	require.Equal(t, "baseline", checkpoint.Collections.Projects[0].Name)
	batch.Cut.Sequence = 9
	_, err = ScopedRecordInventory(checkpoint, []ScopedBatch{batch})
	require.Error(t, err, "missing signed position cannot reconstruct a latest inventory")
}

func TestScopedRecordInventoryOldTombstoneEvidence(t *testing.T) {
	checkpoint, _, _, _ := verificationCheckpoint(t)
	checkpoint.Cut.Sequence = 7
	remove := ScopedEffect{Collection: "issues", Key: verificationID(1) + ":" + verificationID(40), Revision: 2, Action: "remove"}
	old := ScopedBatch{View: checkpoint.View, Cut: ScopedCut{Sequence: 2, Digest: verificationDigest("old"), CaptureUID: verificationID(70)}, PreviousDigest: verificationDigest("prior"), Effects: []ScopedEffect{remove}}
	inventory, err := ScopedRecordInventory(checkpoint, []ScopedBatch{old})
	require.NoError(t, err)
	require.Equal(t, []ScopedEffect{remove}, inventory)
	checkpoint.Collections.Issues = []ScopedIssueState{{ProjectUID: verificationID(1), UID: verificationID(40)}}
	inventory, err = ScopedRecordInventory(checkpoint, []ScopedBatch{old})
	require.NoError(t, err)
	require.Len(t, inventory, 1)
	require.Equal(t, "replace", inventory[0].Action, "old remove cannot override current signed presence")
}
func verificationDigest(s string) string { return ScopedOfferedDigest([]byte(s)) }

func TestScopedRecordInventoryCollectionMapper(t *testing.T) {
	checkpoint, _, _, _ := verificationCheckpoint(t)
	c := checkpoint.Collections
	c.Projects = []ScopedProjectState{{UID: verificationID(1), Name: "visible"}}
	effects, err := ScopedCollectionRecordEffects(c, 4)
	require.NoError(t, err)
	require.Equal(t, []ScopedEffect{{Collection: "projects", Key: verificationID(1), Action: "replace", Revision: 4, Project: &c.Projects[0]}}, effects)
	_, err = ScopedCollectionRecordEffects(c, 0)
	require.Error(t, err)
	c.Projects = nil
	_, err = ScopedCollectionRecordEffects(c, 4)
	require.Error(t, err, "missing required collection remains invalid without checkpoint context")
}
