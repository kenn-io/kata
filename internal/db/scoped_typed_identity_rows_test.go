package db

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestScopedTypedIdentityPredicateRows(t *testing.T) {
	p, entity := verificationID(1), verificationID(40)
	principal := CustodyPrincipal{AuthorityUID: verificationID(9), BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	principalBytes, err := json.Marshal(principal)
	require.NoError(t, err)
	for _, kind := range []string{"job", "workflow"} {
		preparation := ScopedMutationPreparation{Kind: "replaceCronJob", Subject: ScopedMutationSubject{Kind: kind, ProjectUID: p, DefinitionUID: entity}, Predicate: "definition"}
		if kind == "workflow" {
			preparation.Kind = "replaceCronWorkflow"
		}
		predicate := ScopedMutationPredicate{Version: 1, Preparation: preparation, Incarnation: 2, CanonicalDigest: strings.Repeat("b", 64)}
		predicateBytes, err := json.Marshal(predicate)
		require.NoError(t, err)
		row := ScopedMutationExpectationExport{TokenDigest: strings.Repeat("c", 64), ProjectUID: p, SubjectUID: entity, PredicateKind: kind + ".definition", PredicateBytes: predicateBytes, PrincipalBytes: principalBytes, RouteDigest: principal.RouteDigest, Nonce: "nonce", ExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)}
		require.NoError(t, ValidateScopedImportRecord(&row), "qualified typed predicate row")
		row.PredicateKind = "definition"
		require.Error(t, ValidateScopedImportRecord(&row), "untyped counter alias refused")
		row.PredicateKind = "job.definition"
		if kind == "workflow" {
			require.Error(t, ValidateScopedImportRecord(&row), "different namespace cannot borrow predicate")
		}
		incarnation := ScopedPredicateIncarnationExport{ProjectUID: p, SubjectUID: entity, PredicateKind: kind + ".definition", Incarnation: 2, CanonicalDigest: predicate.CanonicalDigest}
		require.NoError(t, ValidateScopedImportRecord(&incarnation))
		incarnation.PredicateKind = "definition"
		require.Error(t, ValidateScopedImportRecord(&incarnation))
	}
}
