package federationpublication_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/federationpublication"
)

func TestDecodeWithCounts(t *testing.T) {
	document := dbtest.PublicationFixture(t, "01K00000000000000000000006")
	document.Digest = ""
	raw, err := federationpublication.Encode(document)
	require.NoError(t, err)
	decoded, counts, err := federationpublication.DecodeWithCounts(raw)
	require.NoError(t, err)
	require.Equal(t, document.ProjectUID, decoded.ProjectUID)
	require.Equal(t, db.FederationPublicationCounts{Issues: 2, Comments: 2, Events: 2}, counts)

	// A rejected envelope must not return counts that a caller could present as
	// a successfully validated publication preview.
	_, counts, err = federationpublication.DecodeWithCounts([]byte(`{"version":1}`))
	require.ErrorIs(t, err, db.ErrFederationPublicationInvalid)
	require.Zero(t, counts)
}
