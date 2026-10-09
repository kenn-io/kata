package db_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestProvenanceReplayValidatesPinnedSignatures(t *testing.T) {
	seed := sha256.Sum256([]byte("fixture-root"))
	private := ed25519.NewKeyFromSeed(seed[:])
	public := private.Public().(ed25519.PublicKey)
	project := &db.ProjectExport{ID: 42, UID: "00000000000000000000000001", Name: "shared-project"}
	pin := &db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	receipt, e := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: "00000000000000000000000003", ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AuthorityUID: pin.AuthorityUID, AccountableActor: "member", SourceActor: "assistant", IngressInstanceUID: "00000000000000000000000004", AcceptedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID}, private)
	require.NoError(t, e)
	ref := &db.EntityProvenance{ProjectUID: project.UID, Kind: "issue", EntityUID: "00000000000000000000000005", EventUID: receipt.EventUID}
	issue := &db.IssueExport{ID: 1, ProjectID: project.ID, UID: ref.EntityUID, Author: "assistant"}
	records := []db.ImportRecord{project, issue, pin, &receipt, ref}
	require.NoError(t, db.ValidateImportReplay(records, db.ImportOptions{}), "a complete owner backup must retain its signed proofs even after event compaction")
	bad := receipt
	bad.AccountableActor = "impostor"
	require.Error(t, db.ValidateImportReplay([]db.ImportRecord{project, pin, &bad}, db.ImportOptions{}), "signature tampering rejects before target clearing")
	require.Error(t, db.ValidateImportReplay([]db.ImportRecord{project, &receipt}, db.ImportOptions{}), "never trust receipt without enrolled/restored pin")
	require.Error(t, db.ValidateImportReplay(append(records, pin), db.ImportOptions{}), "duplicate pins cannot make authority ambiguous")
	other := *ref
	other.EventUID = "00000000000000000000000006"
	require.Error(t, db.ValidateImportReplay([]db.ImportRecord{project, issue, pin, &receipt, &other}, db.ImportOptions{}), "creation ref requires exact receipt")
	other = *ref
	other.EntityUID = "00000000000000000000000007"
	require.Error(t, db.ValidateImportReplay([]db.ImportRecord{project, issue, pin, &receipt, &other}, db.ImportOptions{}), "creation ref must identify a restored entity in its project")
	// This valid dependency-complete snapshot must use the enrolled relay reset
	// protocol; an ordinary project merge cannot establish root authority.
	_, err := db.PrepareProjectMergeRecords(records, db.ProjectMergeOffsets{TargetProjectID: 100}, nil)
	require.ErrorContains(t, err, "trusted provenance")
}
