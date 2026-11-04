package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type summaryResetProbe struct {
	db.RelayResetBackend
	grants, projects, gates, pins int
}

func (b *summaryResetProbe) RelayGrant(context.Context, string, string) (db.FederationEnrollment, error) {
	b.grants++
	return db.FederationEnrollment{ID: 1, ProjectID: new(int64(1))}, nil
}
func (b *summaryResetProbe) Project(context.Context, int64) (db.Project, error) {
	b.projects++
	return db.Project{ID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA"}, nil
}
func (b *summaryResetProbe) RequireProjectEventFeatures(context.Context, string) error {
	b.gates++
	return db.ErrUnsupportedEventFeatures
}
func (b *summaryResetProbe) RootPin(context.Context, string, string) (db.RootKeyPin, error) {
	b.pins++
	return db.RootKeyPin{}, errors.New("checkpoint work started before feature gate")
}
func TestSummaryResetChecksFeaturesInSharedProtocol(t *testing.T) {
	b := &summaryResetProbe{}
	p := db.RelayResetProtocol{Backend: b}
	_, err := p.CreateReset(t.Context(), "fixture-binding", db.RootAttributionSigner{})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Equal(t, 1, b.grants)
	require.Equal(t, 1, b.projects)
	require.Equal(t, 1, b.gates)
	require.Zero(t, b.pins)
}
