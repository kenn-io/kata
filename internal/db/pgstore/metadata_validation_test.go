package pgstore

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Metadata validation rejects bad values before opening a transaction, as it
// did before the shared comment/metadata transaction helper was extracted.
func TestMetadataValidationBeforeTransaction(t *testing.T) {
	store := &Store{}
	attempted := false
	_, err := store.patchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: 1, Actor: "worker", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value("42")}}, func(context.Context, transactionFunc) error {
		attempted = true
		return errors.New("transaction attempted")
	})
	require.ErrorContains(t, err, `validate "scheduled_on"`)
	require.False(t, attempted)
}
