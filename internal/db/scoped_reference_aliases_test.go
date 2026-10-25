package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Alias correspondence is data below custody/adjudication. It cannot replace
// signature, creator, liveness, command-specific deletion or permission checks.
func TestScopedReferenceAliasResolver(t *testing.T) {
	p, q, a, b := verificationID(1), verificationID(2), verificationID(41), verificationID(42)
	first, second := ScopedEndpoint{ProjectUID: p, IssueUID: a}, ScopedEndpoint{ProjectUID: q, IssueUID: b}
	identities := []ScopedIssueState{{UID: a, ProjectUID: p, ShortID: "same", Title: "First"}, {UID: b, ProjectUID: q, ShortID: "same", Title: "Second"}}
	allowed := map[string]ScopedEndpoint{a: first, b: second}
	t.Run("exact-project-alias-with-independent-authorized-endpoint", func(t *testing.T) {
		resolve := NewScopedReferenceAliasResolver(identities, allowed)
		got, err := resolve(p, "same")
		require.NoError(t, err)
		require.Equal(t, first, got)
		got, err = resolve(q, "same")
		require.NoError(t, err)
		require.Equal(t, second, got)
		for _, value := range []string{a, "SAME", "project#same", "ame", "missing"} {
			_, err := resolve(p, value)
			require.ErrorIs(t, err, ErrNotFound, "UID/suffix/case/project-name inference never establishes an alias")
		}
	})
	t.Run("missing-data-and-missing-authority-stay-refused", func(t *testing.T) {
		_, err := NewScopedReferenceAliasResolver(nil, allowed)(p, "same")
		require.ErrorIs(t, err, ErrNotFound)
		_, err = NewScopedReferenceAliasResolver(identities, map[string]ScopedEndpoint{})(p, "same")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("duplicate-identical-triples-and-unrelated-unallowed-rows", func(t *testing.T) {
		rows := append(append([]ScopedIssueState{}, identities...), identities[0])
		rows = append(rows, ScopedIssueState{UID: verificationID(43), ProjectUID: p, ShortID: "same"})
		got, err := NewScopedReferenceAliasResolver(rows, allowed)(p, "same")
		require.NoError(t, err)
		require.Equal(t, first, got)
	})
	t.Run("conflicting-eligible-key-cannot-use-paired-uid-to-choose", func(t *testing.T) {
		rows := append([]ScopedIssueState{}, identities...)
		rows[1].ProjectUID = p
		eligible := map[string]ScopedEndpoint{a: first, b: {ProjectUID: p, IssueUID: b}}
		_, err := NewScopedReferenceAliasResolver(rows, eligible)(p, "same")
		require.ErrorIs(t, err, ErrScopedSourceConflict)
	})
	t.Run("authorized-uid-project-disagreement-is-not-an-alias", func(t *testing.T) {
		rows := []ScopedIssueState{{UID: a, ProjectUID: q, ShortID: "same"}}
		_, err := NewScopedReferenceAliasResolver(rows, allowed)(p, "same")
		require.ErrorIs(t, err, ErrScopedSourceConflict)
	})
	t.Run("capture-only-identity-values-not-mutable-caller-storage", func(t *testing.T) {
		rows := append([]ScopedIssueState{}, identities...)
		eligible := map[string]ScopedEndpoint{a: first}
		resolve := NewScopedReferenceAliasResolver(rows, eligible)
		rows[0].ShortID = "changed"
		delete(eligible, a)
		got, err := resolve(p, "same")
		require.NoError(t, err)
		require.Equal(t, first, got)
	})
}
