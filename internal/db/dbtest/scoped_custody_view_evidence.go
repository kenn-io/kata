package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Actual native retention must bind its selected view, not reconstruct one
// from an operation's project subset. No execution, holder or acceptance proof
// is supplied by this complete normalized-input custody fixture.
func RunScopedSourceCustodyViewEvidence(t *testing.T, open func(*testing.T) db.Storage, custodyBytes func(context.Context, db.Storage) (int64, error)) {
	t.Helper()
	for _, scenario := range []string{"newborn-exact-view-and-cost", "sibling-retry-refused", "legacy-retry-no-upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			root := open(t)
			f := NewScopedCaptureFixture(t, root)
			full := f.Params.View
			narrowParams := f.Params
			narrowParams.View.Key.ViewUID = newScopedCustodyEvidenceUID(t)
			narrowParams.CatalogEntryUID = newScopedCustodyEvidenceUID(t)
			narrowParams.View.Projects = append([]db.ScopedProjectPin(nil), full.Projects[:1]...)
			grant, err := root.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), narrowParams)
			require.NoError(t, err, "both siblings must be genuinely native admitted")
			require.Equal(t, f.Grant.Enrollment.ID, grant.Enrollment.ID)
			require.Equal(t, full.RouteDigest, narrowParams.View.RouteDigest)
			principal := db.CustodyPrincipal{AuthorityUID: full.AuthorityUID, BindingUID: f.Grant.Enrollment.RelayBindingUID, Epoch: f.Grant.Enrollment.RelayResetEpoch, AccountableActor: f.Parent.Actor, RouteDigest: full.RouteDigest}
			access := func(key db.ScopedViewKey) context.Context {
				ctx := db.WithScopedSourceAccess(t.Context(), db.ScopedSourceAccess{View: key, Principal: principal})
				ctx = db.WithTransactionFence(ctx, db.ActorTokenTransactionFence(f.Parent))
				return db.WithAdditionalTransactionFence(ctx, root.ProjectAccessTransactionFence(f.Parent.Actor, []string{full.Projects[0].ProjectUID}))
			}
			op := db.SourceOperation{Version: 3, UID: newScopedCustodyEvidenceUID(t), SourceInstanceUID: full.DestinationUID, CommandKind: "importIssues", Principal: principal, ImportIntent: &db.ScopedImportIntent{ProjectUID: full.Projects[0].ProjectUID, Source: "example-import", Items: []db.ScopedImportItem{}}}
			op.Digest, err = db.SourceOperationDigest(op)
			require.NoError(t, err)
			wire, err := db.EncodeScopedSourceSubmission(db.SourceSubmission{Operation: op})
			require.NoError(t, err)
			submission, err := db.DecodeScopedSourceSubmission(wire)
			require.NoError(t, err)
			selected := narrowParams.View.Key
			if scenario != "newborn-exact-view-and-cost" {
				selected = full.Key
			}
			native := scopedCustodyNative(t, root)
			status, err := native.RetainSourceOperation(access(selected), principal, submission)
			require.NoError(t, err)
			require.Equal(t, "staged", status.State)
			after := scopedAuthoritySnapshot(t, root)
			if scenario == "sibling-retry-refused" {
				_, err := native.RetainSourceOperation(access(narrowParams.View.Key), principal, submission)
				require.Error(t, err, "same route, principal and member subset do not authorize changing the admitted view")
				retried := scopedAuthoritySnapshot(t, root)
				require.Equal(t, scopedMultiProjectSequences(after), scopedMultiProjectSequences(retried), "EVERY sequence FIRST")
				require.Equal(t, after, retried, "ENTIRE owner is unchanged by sibling substitution")
				return
			}
			var custody *db.ScopedSourceCustodyExport
			var descriptorDigest string
			for _, record := range after {
				switch row := record.(type) {
				case *db.ScopedSourceCustodyExport:
					if row.OperationUID == op.UID && row.Direction == "inbound" {
						require.Nil(t, custody)
						custody = row
					}
				case *db.ScopedGenerationExport:
					if row.ViewUID == selected.ViewUID && row.Generation == selected.Generation {
						descriptorDigest = row.DescriptorDigest
						require.Equal(t, db.ScopedOfferedDigest(row.DescriptorBytes), descriptorDigest)
					}
				}
			}
			require.NotNil(t, custody)
			require.NotEmpty(t, descriptorDigest)
			if scenario == "legacy-retry-no-upgrade" {
				// A trusted owner backup in the historical plain-principal format.
				// Its operation and complete data came from the native retain above.
				plain, err := json.Marshal(principal)
				require.NoError(t, err)
				custody.PrincipalBytes = plain
				require.NoError(t, db.ValidateImportRecords(after))
				require.NoError(t, root.ImportReplay(t.Context(), after, db.ImportOptions{}))
				before := scopedAuthoritySnapshot(t, root)
				retry, err := native.RetainSourceOperation(access(selected), principal, submission)
				require.NoError(t, err, "explicit live revalidation remains distinct from automatic view recovery")
				require.Equal(t, status, retry)
				retried := scopedAuthoritySnapshot(t, root)
				require.Equal(t, scopedMultiProjectSequences(before), scopedMultiProjectSequences(retried), "EVERY sequence FIRST on legacy retry")
				require.Equal(t, before, retried, "legacy bytes are never backfilled")
				return
			}
			var evidence struct {
				Version          int                 `json:"version"`
				Principal        db.CustodyPrincipal `json:"principal"`
				View             db.ScopedViewKey    `json:"view"`
				DescriptorDigest string              `json:"descriptor_digest"`
			}
			require.NoError(t, json.Unmarshal(custody.PrincipalBytes, &evidence))
			require.Equal(t, 1, evidence.Version)
			require.Equal(t, principal, evidence.Principal)
			require.Equal(t, selected, evidence.View, "record actual native requested narrow sibling, not first matching full view")
			require.Equal(t, descriptorDigest, evidence.DescriptorDigest)
			var expected int64
			for _, record := range after {
				if _, ok := record.(*db.ScopedSourceCustodyExport); ok {
					expected += db.ScopedRetainedBytes(record)
				}
			}
			charged, err := custodyBytes(t.Context(), root)
			require.NoError(t, err)
			require.Equal(t, expected, charged, "same SQL accountant charges all actual retained evidence bytes")
			require.NoError(t, db.ValidateImportRecords(after))
			backup := open(t)
			require.NoError(t, backup.ImportReplay(t.Context(), after, db.ImportOptions{}))
			require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, backup)))
		})
	}
}

func newScopedCustodyEvidenceUID(t *testing.T) string {
	t.Helper()
	value, err := uid.New()
	require.NoError(t, err)
	return value
}

func scopedMultiProjectSequences(records []db.ImportRecord) []db.SequenceExport {
	var rows []db.SequenceExport
	for _, record := range records {
		if sequence, ok := record.(*db.SequenceExport); ok {
			rows = append(rows, *sequence)
		}
	}
	return rows
}
