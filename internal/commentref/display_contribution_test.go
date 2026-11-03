package commentref

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDisplayContributionKeepsRoutingIdentity(t *testing.T) {
	writer := "writer"
	records := []Record{
		{UID: "01AAAAAAAAAAAAAAAAAAAAAAA1", Author: "actor", Teammate: "route", AttributedTeammate: &writer, IssueUID: "issue", IssueShortID: "abcd", ProjectID: 1},
		{UID: "01AAAAAAAAAAAAAAAAAAAAAAA2", Author: "reviewer", ReplyToUID: "01AAAAAAAAAAAAAAAAAAAAAAA1", ReplyKind: "reply", IssueUID: "issue", IssueShortID: "abcd", ProjectID: 1},
	}
	projected := Project(records, nil)
	data, err := json.Marshal(projected[1].Reply)
	require.NoError(t, err)
	var link map[string]any
	require.NoError(t, json.Unmarshal(data, &link))
	require.Equal(t, "writer", link["attributed_teammate"])
	require.Equal(t, "route", link["teammate"])
	selected, err := Select(projected, "issue", Options{Inbound: "actor/route"})
	require.NoError(t, err)
	require.Len(t, selected.Comments, 1)
	require.Equal(t, records[1].UID, selected.Comments[0].UID)
	selected, err = Select(projected, "issue", Options{Inbound: "actor/writer"})
	require.NoError(t, err)
	require.Empty(t, selected.Comments)
}
