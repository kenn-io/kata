package planesync

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testWorkItem() WorkItem {
	at := time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC)
	return WorkItem{ID: testItemID, ProjectID: testProjectID, StateID: testStateID, SequenceID: 42, Name: "Source task", DescriptionHTML: "<h2>Details</h2><p>Complete <strong>content</strong> &amp; <a href=\"https://example.com/reference\">reference</a>.</p><ul><li>First</li><li>Second</li></ul><pre><code>run example</code></pre>", CreatorID: testUserID, AssigneeIDs: []string{testUserID}, CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
}

func TestBuildImportBatch(t *testing.T) {
	c := testConfig()
	for _, group := range []string{"backlog", "unstarted", "started", "completed", "cancelled"} {
		t.Run(group, func(t *testing.T) {
			item := testWorkItem()
			batch, err := BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX", Name: "Example project"}, []State{{ID: testStateID, Group: group}}, []WorkItem{item})
			require.NoError(t, err)
			require.Equal(t, "plane-sync", batch.Actor)
			require.True(t, batch.ReconcileStatusForUnchanged)
			require.Equal(t, []string{"plane"}, batch.ReconcileLabelsForUnchanged["work-item:"+testItemID])
			got := batch.Items[0]
			require.Equal(t, "work-item:"+testItemID, got.ExternalID)
			require.Equal(t, "[Plane EX-42] Source task", got.Title)
			require.Equal(t, "plane:"+testUserID, got.Author)
			require.Equal(t, new("plane:"+testUserID), got.Owner)
			require.Nil(t, got.Priority)
			require.True(t, got.UpdatedAt.Equal(item.UpdatedAt.Truncate(time.Millisecond)))
			for _, text := range []string{"## Details", "**content**", "[reference](https://example.com/reference)", "First", "Second", "run example", "Imported from Plane: https://app.plane.so/example-workspace/projects/" + testProjectID + "/issues/" + testItemID + "/"} {
				require.Contains(t, got.Body, text)
			}
			if group == "completed" || group == "cancelled" {
				require.Equal(t, "closed", got.Status)
				reason := "done"
				if group == "cancelled" {
					reason = "wontfix"
				}
				require.Equal(t, new(reason), got.ClosedReason)
				require.Equal(t, &got.UpdatedAt, got.ClosedAt)
			} else {
				require.Equal(t, "open", got.Status)
				require.Nil(t, got.ClosedAt)
			}
		})
	}
}

func TestMappingEmptyAndPresentation(t *testing.T) {
	c := testConfig()
	c.TitlePrefix = new(false)
	item := testWorkItem()
	item.Name, item.CreatorID, item.AssigneeIDs, item.DescriptionHTML = "  ", "", nil, ""
	batch, err := BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "started"}}, []WorkItem{item})
	require.NoError(t, err)
	require.Equal(t, "(untitled)", batch.Items[0].Title)
	require.Equal(t, []string{"plane"}, batch.Items[0].Labels)
	require.Equal(t, "plane-unknown", batch.Items[0].Author)
	require.Nil(t, batch.Items[0].Owner)
}

func TestMappingRejectsInvalidCompleteObservations(t *testing.T) {
	for _, mutate := range []func(*WorkItem){
		func(i *WorkItem) { i.ProjectID = testItemID },
		func(i *WorkItem) { i.ID = "bad" },
		func(i *WorkItem) { i.StateID = testItemID },
		func(i *WorkItem) { i.CreatorID = "bad" },
		func(i *WorkItem) { i.AssigneeIDs = []string{testUserID, "bad"} },
		func(i *WorkItem) { i.UpdatedAt = i.CreatedAt.Add(-time.Hour) },
		func(i *WorkItem) { i.CreatedAt = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) },
		func(i *WorkItem) { i.SequenceID = 0 },
		func(i *WorkItem) { i.Name = "bad\x00title" },
		func(i *WorkItem) { i.DescriptionHTML = strings.Repeat("x", (1<<20)+1) },
	} {
		c, item := testConfig(), testWorkItem()
		mutate(&item)
		_, err := BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "started"}}, []WorkItem{item})
		require.Error(t, err)
		require.Contains(t, err.Error(), item.ID)
	}
	c := testConfig()
	_, err := BuildImportBatch("plane:elsewhere", c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "started"}}, nil)
	require.Error(t, err)
	_, err = BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "unknown"}}, nil)
	require.Error(t, err)
	_, err = BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "started"}}, []WorkItem{testWorkItem(), testWorkItem()})
	require.ErrorContains(t, err, testItemID)
}

func TestMappingKeepsTextFromUnsupportedLinks(t *testing.T) {
	for _, href := range []string{"tel:+15555550100", "vscode://file/example", "slack://channel", "zoommtg://join", "https://user@example.com/x", "http://[bad", "javascript:alert(1)", "java&#x09;script:alert(1)", "data:text/html,example"} {
		t.Run(href, func(t *testing.T) {
			c, item := testConfig(), testWorkItem()
			item.DescriptionHTML = `<p>Before <a href="` + href + `">review this</a> after.</p><img src="` + href + `" alt="Diagram">`
			batch, err := BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "started"}}, []WorkItem{item})
			require.NoError(t, err)
			require.Contains(t, batch.Items[0].Body, "Before review this after.")
			require.NotContains(t, batch.Items[0].Body, href)
			require.NotContains(t, batch.Items[0].Body, "javascript:")
		})
	}
}
