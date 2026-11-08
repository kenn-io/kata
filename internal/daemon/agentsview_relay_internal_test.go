package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type missingAgentsViewEventStore struct{ db.Storage }

func (missingAgentsViewEventStore) EventsByUIDs(context.Context, int64, []string) ([]db.Event, error) {
	return nil, db.ErrNotFound
}

type emptyAgentsViewEventStore struct{ db.Storage }

func (emptyAgentsViewEventStore) EventsByUIDs(context.Context, int64, []string) ([]db.Event, error) {
	return nil, nil
}

func TestAgentsViewMissingOriginEventIsPendingForOpenAndClosedIssue(t *testing.T) {
	for _, store := range []db.Storage{missingAgentsViewEventStore{}, emptyAgentsViewEventStore{}} {
		for _, status := range []string{"open", "closed"} {
			issue := db.Issue{UID: "issue", ProjectID: 1, Status: status}
			_, err := loadAgentsViewClose(t.Context(), ServerConfig{DB: store}, issue, agentsviewrelay.Control{IssueUID: issue.UID})
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, "close_not_at_origin", apiErr.Code)
		}
	}
}

func TestAgentsViewFederationUsesEnrollmentActor(t *testing.T) {
	actor, err := agentsViewUploadActor(t.Context(), true, "", federationPrincipal{Actor: "enrolled-worker"})
	require.NoError(t, err)
	require.Equal(t, "enrolled-worker", actor)
}

func TestAgentsViewSignerReadinessRejectsMissingAndWrongKeys(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	pin := db.RootKeyPin{AuthorityUID: "root", KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey))}
	for _, signer := range []db.RootAttributionSigner{{AuthorityUID: "root"}, {AuthorityUID: "root", PrivateKey: other}, {AuthorityUID: "other", PrivateKey: key}} {
		require.False(t, agentsViewSignerMatches(signer, pin))
	}
	require.True(t, agentsViewSignerMatches(db.RootAttributionSigner{AuthorityUID: "root", PrivateKey: key}, pin))
}

func TestAgentsViewRootAdmissionDistinguishesPinFromSigner(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pin := db.RootKeyPin{AuthorityUID: "root", KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey))}
	association := agentsviewrelay.Association{AuthorityUID: pin.AuthorityUID, RootKeyID: pin.KeyID}
	signer := db.RootAttributionSigner{AuthorityUID: "root", PrivateKey: key}
	retired := pin
	retired.Retired = true
	for _, tc := range []struct {
		pin    db.RootKeyPin
		signer db.RootAttributionSigner
		code   string
	}{
		{retired, signer, "agentsview_route_changed"},
		{db.RootKeyPin{AuthorityUID: "other", KeyID: pin.KeyID}, signer, "agentsview_route_changed"},
		{pin, db.RootAttributionSigner{AuthorityUID: "root"}, "agentsview_unavailable"},
	} {
		var apiErr *api.APIError
		require.ErrorAs(t, agentsViewRootAdmission(association, tc.pin, tc.signer), &apiErr)
		require.Equal(t, tc.code, apiErr.Code)
	}
	require.NoError(t, agentsViewRootAdmission(association, pin, signer))
}
