package federation

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetainedRecoveryUsesSharedHubErrors(t *testing.T) {
	const issueUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
	missing := &HubStatusError{StatusCode: http.StatusBadRequest, Body: `{"error":{"code":"validation","message":"federation ingest validation: issue.updated references unknown issue ` + issueUID + `"}}`}
	require.Equal(t, issueUID, retainedUnknownIssue(missing))
	existing := &HubStatusError{StatusCode: http.StatusBadRequest, Body: `{"error":{"code":"validation","message":"federation ingest validation: fresh issue.created targets existing issue ` + issueUID + `"}}`}
	require.True(t, retainedInitializerAlreadyKnown(existing, "issue.created", issueUID))
	require.False(t, retainedInitializerAlreadyKnown(existing, "issue.snapshot", issueUID))
}
