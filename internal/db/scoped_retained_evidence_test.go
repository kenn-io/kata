package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedRetainedEvidenceShapes(t *testing.T) {
	v, _, _ := verificationView(t)
	principal := CustodyPrincipal{AuthorityUID: v.AuthorityUID, BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: v.RouteDigest}
	admission := ScopedAdmissionEvidence{Version: 1, OperationUID: verificationID(50), OperationDigest: strings.Repeat("a", 64), Principal: principal}
	require.NoError(t, admission.Validate())
	admission.Version = 0
	require.Error(t, admission.Validate())
	admission.Version = 1
	admission.Principal.Epoch = 0
	require.Error(t, admission.Validate())
	predicate := ScopedMutationPredicate{Version: 1, Preparation: ScopedMutationPreparation{Kind: "patchIssueMetadata", Subject: ScopedMutationSubject{Kind: "issue", ProjectUID: verificationID(1), IssueUID: verificationID(40)}, Predicate: "metadata", MetadataGuard: &ScopedMetadataPredicate{Key: "work.attention", IfAbsent: true}}, Incarnation: 2, CanonicalDigest: strings.Repeat("a", 64)}
	require.NoError(t, predicate.Validate())
	predicate.Version = 0
	require.Error(t, predicate.Validate())
	predicate.Version = 1
	predicate.Incarnation = 0
	require.Error(t, predicate.Validate())
	predicate.Incarnation = 2
	predicate.CanonicalDigest = strings.Repeat("A", 64)
	require.Error(t, predicate.Validate())
	predicate.CanonicalDigest = strings.Repeat("a", 64)
	predicate.Preparation.MetadataGuard = nil
	require.Error(t, predicate.Validate(), "same strict native preparation union")
}
