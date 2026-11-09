package db_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: connection identity is nonsecret binding metadata, independent of
// provider configuration and private runner checkpoints. Absence is legacy-local.
func TestIssueSyncConnectionConfigRoundTrip(t *testing.T) {
	selection := db.IssueSyncConnection{
		OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ConnectionUID:    "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		Generation:       "generation-1",
	}
	legacy := jsontext.Value(`{"status_sync":"two-way","source":"example-source"}`)
	missing, err := db.IssueSyncConnectionFromConfig(legacy)
	require.NoError(t, err)
	require.Nil(t, missing)
	stored, err := db.SetIssueSyncConnectionConfig(legacy, selection)
	require.NoError(t, err)
	got, err := db.IssueSyncConnectionFromConfig(stored)
	require.NoError(t, err)
	require.Equal(t, &selection, got)
	provider, err := db.ProviderIssueSyncConfig(stored)
	require.NoError(t, err)
	require.JSONEq(t, string(legacy), string(provider))
	public, err := db.PublicIssueSyncConfig(stored)
	require.NoError(t, err)
	require.JSONEq(t, string(stored), string(public))
}

// Contract: provider metadata refresh and ordinary re-enable cannot replace the
// selected account. Replacement requires the dedicated guarded selection path.
func TestIssueSyncConnectionSurvivesRefreshAndCannotBeInjected(t *testing.T) {
	previous := jsontext.Value(`{"status_sync":"two-way","_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"original"},"_provider_checkpoint":{"cursor":"fixture"}}`)
	next := jsontext.Value(`{"status_sync":"one-way","_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAX","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAY","generation":"replacement"}}`)
	merged, err := db.PreserveIssueStatusScanConfig(previous, next)
	require.NoError(t, err)
	want, err := db.IssueSyncConnectionFromConfig(previous)
	require.NoError(t, err)
	got, err := db.IssueSyncConnectionFromConfig(merged)
	require.NoError(t, err)
	require.Equal(t, want, got)
	public, err := db.PublicIssueSyncConfig(merged)
	require.NoError(t, err)
	require.NotContains(t, string(public), "_provider_checkpoint")
	require.Contains(t, string(public), "original")
	require.NotContains(t, string(public), "replacement")
}

// Contract: selecting a different generation must invalidate an enable snapshot,
// although private scan/checkpoint progress is intentionally ignored.
func TestIssueSyncConnectionParticipatesInBindingPrecondition(t *testing.T) {
	before := jsontext.Value(`{"status_sync":"one-way","_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"first"}}`)
	after := jsontext.Value(`{"status_sync":"one-way","_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"second"}}`)
	matches, err := db.IssueSyncConfigMatches(before, after)
	require.NoError(t, err)
	require.False(t, matches)
}

func TestLegacyIssueSyncRefreshCannotAcquireConnectionFromConfig(t *testing.T) {
	previous := jsontext.Value(`{"status_sync":"one-way"}`)
	next := jsontext.Value(`{"status_sync":"one-way","_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"injected"}}`)
	merged, err := db.PreserveIssueStatusScanConfig(previous, next)
	require.NoError(t, err)
	got, err := db.IssueSyncConnectionFromConfig(merged)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestIssueSyncConnectionRejectsPartialOrMalformedIdentity(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`,
		`{"_tracker_connection":null}`,
		`{"_tracker_connection":{}}`,
		`{"_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}`,
		`{"_tracker_connection":{"owner_instance_uid":"invalid","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"first"}}`,
		`{"_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"first","token":"fixture-only"}}`,
		`{"_tracker_connection":{"owner_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","connection_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","generation":"line\nbreak"}}`,
	} {
		_, err := db.IssueSyncConnectionFromConfig(jsontext.Value(raw))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "fixture-only")
		_, err = db.ProviderIssueSyncConfig(jsontext.Value(raw))
		require.Error(t, err)
	}
}

func TestIssueSyncConnectionMarkerMustAgreeWithSelection(t *testing.T) {
	selection := db.IssueSyncConnection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Generation: "first"}
	legacy := jsontext.Value(`{"status_sync":"one-way"}`)
	named, err := db.SetIssueSyncConnectionConfig(legacy, selection)
	require.NoError(t, err)
	require.NoError(t, db.ValidateIssueSyncConnectionMarker(nil, legacy))
	require.NoError(t, db.ValidateIssueSyncConnectionMarker(&selection.ConnectionUID, named))
	require.Error(t, db.ValidateIssueSyncConnectionMarker(nil, named))
	require.Error(t, db.ValidateIssueSyncConnectionMarker(&selection.ConnectionUID, legacy))
	other := "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	require.Error(t, db.ValidateIssueSyncConnectionMarker(&other, named))
}

func TestIssueSyncNamedSelectionNeedsTypedAdmission(t *testing.T) {
	selection := db.IssueSyncConnection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Generation: "first"}
	raw, err := db.SetIssueSyncConnectionConfig(jsontext.Value(`{}`), selection)
	require.NoError(t, err)
	_, _, err = db.PrepareIssueSyncBindingConfig(nil, db.UpsertIssueSyncBindingParams{Config: raw})
	require.Error(t, err, "raw config is not selection authority")
	config, marker, err := db.PrepareIssueSyncBindingConfig(nil, db.UpsertIssueSyncBindingParams{Config: jsontext.Value(`{}`), Connection: &selection})
	require.NoError(t, err)
	require.Equal(t, selection.ConnectionUID, *marker)
	require.NoError(t, db.ValidateIssueSyncConnectionMarker(marker, config))
	previous := db.IssueSyncBinding{Config: config, ConnectionUID: marker}
	unchanged, gotMarker, err := db.PrepareIssueSyncBindingConfig(&previous, db.UpsertIssueSyncBindingParams{Config: jsontext.Value(`{}`)})
	require.NoError(t, err)
	require.Equal(t, marker, gotMarker)
	require.NoError(t, db.ValidateIssueSyncConnectionMarker(gotMarker, unchanged))
	replacement := selection
	replacement.Generation = "next"
	_, _, err = db.PrepareIssueSyncBindingConfig(&previous, db.UpsertIssueSyncBindingParams{Config: jsontext.Value(`{}`), Connection: &replacement})
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged, "ordinary enable cannot replace account selection")
}
