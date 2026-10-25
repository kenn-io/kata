package dbtest

import (
	"encoding/json/jsontext"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A valid restored private counter may reach its representable limit. The
// next effective write must refuse with its native transaction intact.
func RunScopedOpaqueGuardIncarnationCap(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, test := range []struct {
		name        string
		incarnation int64
		refuse      bool
	}{
		{"cap_minus_one", math.MaxInt64 - 1, false},
		{"cap", math.MaxInt64, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := open(t)
			f := NewScopedCaptureFixture(t, s)
			write := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Issue.ProjectUID), "member")
			initial, err := s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"B"`)}})
			require.NoError(t, err)
			require.True(t, initial.Changed)
			native, ok := s.(scopedPreparationStore)
			require.True(t, ok)
			ctx := scopedPreparationContext(t, f)
			command := db.ScopedCommand{Kind: "patchIssueMetadata", Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Metadata: &db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "source-label", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"next"`)}, Guard: &db.MetadataPatchGuard{Key: "guard", IfValue: jsontext.Value(`"B"`)}}}
			token, err := native.PrepareMutation(ctx, command)
			require.NoError(t, err)
			predicate := scopedPreparedPredicate(t, s, token)
			kind, err := db.ScopedPredicateStorageKind(predicate.Preparation)
			require.NoError(t, err)

			// Change only the existing typed private row in a COMPLETE owner
			// export. Earlier tokens remain valid historical issuer evidence.
			records := scopedAuthoritySnapshot(t, s)
			matches := 0
			for n, record := range records {
				if row, ok := record.(*db.ScopedPredicateIncarnationExport); ok && row.ProjectUID == f.Issue.ProjectUID && row.SubjectUID == f.Issue.UID && row.PredicateKind == kind {
					copy := *row
					copy.Incarnation = test.incarnation
					records[n] = &copy
					matches++
				}
			}
			require.Equal(t, 1, matches)
			require.NoError(t, db.ValidateImportRecords(records), "the complete owner with this positive int64 incarnation is valid")
			require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
			issue, err := s.IssueByUID(write, f.Issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			project, err := s.ProjectByUID(write, f.Issue.ProjectUID)
			require.NoError(t, err)
			require.Equal(t, project.UID, issue.ProjectUID)
			command.Metadata.IssueID = issue.ID
			command.Subject = db.ScopedEndpoint{ProjectUID: project.UID, IssueUID: issue.UID}
			token, err = native.PrepareMutation(ctx, command)
			require.NoError(t, err, "restored native authority and actual GuardB must admit preparation")
			predicate = scopedPreparedPredicate(t, s, token)
			require.Equal(t, test.incarnation, predicate.Incarnation)
			noop, err := s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"B"`)}})
			require.NoError(t, err, "a same-value native patch is legal even at the cap")
			require.False(t, noop.Changed)
			scopedAssertPredicateCurrent(t, s, predicate, false)
			beforeIssue, err := s.IssueByUID(write, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.JSONEq(t, `{"guard":"B"}`, string(beforeIssue.Metadata))
			before := scopedAuthoritySnapshot(t, s)
			require.NoError(t, db.ValidateImportRecords(before))
			out, err := s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"C"`)}})
			if !test.refuse {
				require.NoError(t, err, "cap-1 permits one effective native change")
				require.True(t, out.Changed)
				current, err := s.IssueByUID(write, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.JSONEq(t, `{"guard":"C"}`, string(current.Metadata))
				after := scopedAuthoritySnapshot(t, s)
				require.NoError(t, db.ValidateImportRecords(after))
				found := false
				for _, record := range after {
					if row, ok := record.(*db.ScopedPredicateIncarnationExport); ok && row.ProjectUID == project.UID && row.SubjectUID == issue.UID && row.PredicateKind == kind {
						found = true
						require.Equal(t, int64(math.MaxInt64), row.Incarnation)
						require.NotEqual(t, predicate.CanonicalDigest, row.CanonicalDigest)
					}
				}
				require.True(t, found)
				return
			}
			require.Error(t, err, "an effective native write must refuse before the private incarnation overflows")
			require.False(t, out.Changed)
			afterIssue, err := s.IssueByUID(write, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, beforeIssue, afterIssue)
			after := scopedAuthoritySnapshot(t, s)
			require.NoError(t, db.ValidateImportRecords(after), "refusal preserves a valid complete canonical/private owner graph")
			// PostgreSQL event nextval is nontransactional. Only its allocator
			// high-water may advance; every domain row and other sequence must
			// compare exactly, including events, private predicates and work.
			domainRows := func(records []db.ImportRecord) ([]db.ImportRecord, int64) {
				var rows []db.ImportRecord
				var eventSequence int64
				for _, record := range records {
					if sequence, ok := record.(*db.SequenceExport); ok && sequence.Name == "events" {
						eventSequence = sequence.Seq
						continue
					}
					rows = append(rows, record)
				}
				return rows, eventSequence
			}
			beforeRows, beforeSequence := domainRows(before)
			afterRows, afterSequence := domainRows(after)
			require.Equal(t, beforeRows, afterRows, "all canonical/private/domain state rolls back")
			require.GreaterOrEqual(t, afterSequence, beforeSequence, "event allocation never rewinds")
			scopedAssertPredicateCurrent(t, s, predicate, false)
		})
	}
}
