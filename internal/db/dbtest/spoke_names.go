package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunFederationSpokeNames checks hub-local labels and their backup contract.
func RunFederationSpokeNames(t *testing.T, store db.Storage) {
	ctx := t.Context()
	const first = "01HZNQ7VFPK1XGD8R5MABCD4EA"
	const second = "01HZNQ7VFPK1XGD8R5MABCD4EB"
	create := func(uid, name string) error {
		_, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: uid, SpokeName: name, Actor: "member", Capabilities: "pull"})
		return err
	}
	require.NoError(t, create(first, "laptop"))
	names, err := store.ListFederationSpokeNames(ctx)
	require.NoError(t, err)
	require.Equal(t, "laptop", names[first])
	require.NoError(t, create(first, ""))
	require.ErrorIs(t, create(second, "laptop"), db.ErrFederationSpokeNameValidation)
	enrollments, err := store.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 2, "a rejected name must not strand a new credential")
	require.NoError(t, create(second, "build-box"))
	require.ErrorContains(t, store.SetFederationSpokeName(ctx, second, "laptop"), first)
	for _, bad := range []string{"Laptop", "bad name", "bad\nname", "-bad", "abcdefghijklmnopqrstuvwxyzabcdefg"} {
		require.ErrorIs(t, store.SetFederationSpokeName(ctx, second, bad), db.ErrFederationSpokeNameValidation)
	}
	require.ErrorIs(t, store.SetFederationSpokeName(ctx, "invalid", "phone"), db.ErrFederationSpokeNameValidation)
	require.ErrorIs(t, store.SetFederationSpokeName(ctx, "01HZNQ7VFPK1XGD8R5MABCD4EC", "phone"), db.ErrNotFound)
	require.NoError(t, store.SetFederationSpokeName(ctx, first, "desktop"))
	require.NoError(t, store.SetFederationSpokeName(ctx, second, "laptop"))
	var records []db.ImportRecord
	for rec, err := range store.ExportMeta(ctx) {
		require.NoError(t, err)
		records = append(records, &rec)
	}
	for rec, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &rec)
	}
	for rec, err := range store.ExportFederationEnrollments(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &rec)
	}
	require.NoError(t, store.SetFederationSpokeName(ctx, first, ""))
	names, err = store.ListFederationSpokeNames(ctx)
	require.NoError(t, err)
	require.NotContains(t, names, first)
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
	names, err = store.ListFederationSpokeNames(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{first: "desktop", second: "laptop"}, names)
	for _, bad := range []*db.MetaKV{
		{Key: "federation_spoke_name." + first, Value: "Bad"},
		{Key: "federation_spoke_name.invalid", Value: "phone"},
		{Key: "federation_spoke_name.01HZNQ7VFPK1XGD8R5MABCD4EC", Value: "phone"},
		{Key: "federation_spoke_name." + first, Value: "laptop"},
	} {
		invalid := append(append([]db.ImportRecord{}, records...), bad)
		require.ErrorIs(t, store.ImportReplay(ctx, invalid, db.ImportOptions{}), db.ErrFederationSpokeNameValidation)
		names, err = store.ListFederationSpokeNames(ctx)
		require.NoError(t, err)
		require.Equal(t, "desktop", names[first], "invalid import is atomic")
	}
	// Replacement restores must not retain target-only labels for absent peers.
	withoutSecond := make([]db.ImportRecord, 0, len(records))
	for _, record := range records {
		if m, ok := record.(*db.MetaKV); ok && m.Key == "federation_spoke_name."+second {
			continue
		}
		withoutSecond = append(withoutSecond, record)
	}
	require.NoError(t, store.ImportReplay(ctx, withoutSecond, db.ImportOptions{}))
	names, err = store.ListFederationSpokeNames(ctx)
	require.NoError(t, err)
	require.NotContains(t, names, second)
	// Enrollment and later naming share the same uniqueness serialization.
	require.NoError(t, store.SetFederationSpokeName(ctx, first, ""))
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- create(first, "phone") }()
	go func() { <-start; results <- store.SetFederationSpokeName(ctx, second, "phone") }()
	close(start)
	a, b := <-results, <-results
	if a == nil {
		require.ErrorIs(t, b, db.ErrFederationSpokeNameValidation)
	} else {
		require.ErrorIs(t, a, db.ErrFederationSpokeNameValidation)
		require.NoError(t, b)
	}

}
