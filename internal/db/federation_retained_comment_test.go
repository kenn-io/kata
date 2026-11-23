package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetainedCommentPayloadRejectsNull(t *testing.T) {
	_, err := RetainedCommentPayload("null", "issue.commented", "comment", "actor", "body")
	require.ErrorIs(t, err, ErrNotFound)
}
