package mcpserver

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestScheduleSummaryUsesSavedRules(t *testing.T) {
	values := map[string]any{"timezone": "Asia/Tokyo", "scheduled_on": map[string]any{"value": "2026-01-01T02:00:00Z", "timezone": "America/New_York", "tzdata_version": metadata.ScheduleTZDataVersion}}
	summary := (toolHandlers{}).summaryFromIssue(ProjectIdentity{Name: "example"}, generated.Issue{Metadata: values})
	require.Equal(t, "America/New_York", *summary.Timezone)
	require.Equal(t, "2026-01-01T02:00:00Z", *summary.ScheduledOn)
	values["scheduled_on"].(map[string]any)["tzdata_version"] = "unknown"
	handler := validateScheduleOutput(func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, IssueListOutput, error) {
		return successResult(), IssueListOutput{Issues: []IssueSummary{summary}}, nil
	})
	_, _, err := handler(t.Context(), nil, struct{}{})
	require.ErrorContains(t, err, "unsupported schedule tzdata version")
	delete(values, "scheduled_on")
	values["deadline_on"] = map[string]any{"value": "2026-01-01", "timezone": "UTC", "tzdata_version": "unknown"}
	_, _, err = handler(t.Context(), nil, struct{}{})
	require.ErrorContains(t, err, "unsupported schedule tzdata version")
}
