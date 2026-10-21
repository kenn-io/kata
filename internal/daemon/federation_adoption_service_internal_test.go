package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

func TestAdoptionSourceErrorExplainsAuthorRewriteIneligibility(t *testing.T) {
	err := adoptionSourceError(errors.New("author-rewrite history cannot be relayed"))

	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Contains(t, apiErr.Hint, "project.author_rewritten")
}
