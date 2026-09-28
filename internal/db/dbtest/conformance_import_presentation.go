package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/notionsync"
)

// Exercise real provider mapping and storage: dropping same-version label
// refresh leaves existing imports without tags when the presentation changes.
func checkImportPresentationLabels(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	sourceAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	for _, provider := range []string{"notion", "github"} {
		for _, collision := range []string{"none", "local", "upstream"} {
			if provider == "notion" && collision == "upstream" {
				continue
			}
			t.Run(provider+"/"+collision, func(t *testing.T) {
				project, err := store.CreateProject(ctx, fmt.Sprintf("example-%s-%s", provider, collision))
				require.NoError(t, err)
				build := func(prefix bool, at time.Time, upstream []string) db.ImportBatchParams {
					var batch db.ImportBatchParams
					if provider == "notion" {
						config := notionsync.Config{DataSourceID: "11111111-1111-4111-8111-111111111111", DatabaseID: "22222222-2222-4222-8222-222222222222", TitlePropertyID: "title", StatusPropertyID: "status", AssigneePropertyID: "people", DoneStatusIDs: []string{"done"}, TitlePrefix: new(prefix)}
						batch, err = notionsync.BuildImportBatch("notion:"+config.DataSourceID, config, []notionsync.PageContent{{Page: notionsync.Page{ID: "33333333-3333-4333-8333-333333333333", DataSourceID: config.DataSourceID, URL: "https://www.notion.so/example-page", CreatedAt: sourceAt.Add(-time.Hour), UpdatedAt: at}, Title: "Source task", Markdown: "Source body"}})
						require.NoError(t, err)
					} else {
						labels := make([]githubsync.Label, 0, len(upstream))
						for _, label := range upstream {
							labels = append(labels, githubsync.Label{Name: label})
						}
						created := sourceAt.Add(-time.Hour)
						batch = githubsync.BuildImportBatchWithConfig("github:example-node", githubsync.Config{TitlePrefix: new(prefix)}, []githubsync.Issue{{ID: 123, Number: 123, Title: "Source task", Body: "Source body", State: "open", HTMLURL: "https://github.example/example-owner/example-repo/issues/123", CreatedAt: &created, UpdatedAt: &at, Labels: labels}}, nil, githubsync.ParentData{}, at)
					}
					batch.ProjectID = project.ID
					return batch
				}
				upstream := []string{"bug"}
				if collision == "upstream" {
					upstream = append(upstream, "GitHub", "github")
				}
				initial := build(true, sourceAt, upstream)
				_, _, err = store.ImportBatch(ctx, initial)
				require.NoError(t, err)
				mapping, err := store.ImportMappingBySource(ctx, project.ID, initial.Source, "issue", initial.Items[0].ExternalID)
				require.NoError(t, err)
				require.NotNil(t, mapping.IssueID)
				issueID := *mapping.IssueID
				_, err = store.AddLabel(ctx, issueID, "local", "editor")
				require.NoError(t, err)
				if collision == "local" {
					_, err = store.AddLabel(ctx, issueID, provider, "editor")
					require.NoError(t, err)
				}
				expected := []string{"local"}
				if provider == "github" {
					expected = append(expected, "bug")
				}
				assertLabels := func(want ...string) {
					labels, err := store.LabelsByIssue(ctx, issueID)
					require.NoError(t, err)
					require.ElementsMatch(t, want, importLabelNames(labels))
				}
				for _, prefix := range []bool{false, true, false} {
					batch := build(prefix, sourceAt, upstream)
					_, events, err := store.ImportBatch(ctx, batch)
					require.NoError(t, err)
					issue, err := store.IssueByID(ctx, issueID)
					require.NoError(t, err)
					wantTitle := "Source task"
					if prefix {
						if provider == "notion" {
							wantTitle = "[Notion] Source task"
						} else {
							wantTitle = "[GitHub #123] Source task"
						}
					}
					require.Equal(t, wantTitle, issue.Title)
					want := append([]string{}, expected...)
					if !prefix || collision != "none" {
						want = append(want, provider)
					}
					assertLabels(want...)
					require.NotEmpty(t, events)
					_, replayEvents, err := store.ImportBatch(ctx, batch)
					require.NoError(t, err)
					require.Empty(t, replayEvents, "same-source replay is idempotent")
				}
				// The presentation label is source-owned even at the same version.
				if collision == "none" {
					_, err = store.RemoveLabelAndEvent(ctx, issueID, db.LabelEventParams{Label: provider, Actor: "editor"})
					require.NoError(t, err)
					assertLabels(expected...)
					batch := build(false, sourceAt, upstream)
					result, events, err := store.ImportBatch(ctx, batch)
					require.NoError(t, err)
					require.Equal(t, 1, result.Unchanged)
					assertLabels(append(expected, provider)...)
					require.Len(t, events, 1)
					require.Equal(t, "issue.labeled", events[0].Type)
					require.Equal(t, &issueID, events[0].IssueID)
					require.Contains(t, events[0].Payload, `"label":"`+provider+`"`)
					_, replayEvents, err := store.ImportBatch(ctx, batch)
					require.NoError(t, err)
					require.Empty(t, replayEvents)
				}
				// A stale title/remote-label observation must not remove the active tag;
				// repeated stale replay must not roll the source timestamp backwards.
				stale := build(true, sourceAt.Add(-time.Minute), []string{"stale-remote"})
				stale.Items[0].Body = "Stale body"
				for range 2 {
					_, events, err := store.ImportBatch(ctx, stale)
					require.NoError(t, err)
					require.Empty(t, events)
					assertLabels(append(expected, provider)...)
					observed, err := store.ImportMappingBySource(ctx, project.ID, initial.Source, "issue", initial.Items[0].ExternalID)
					require.NoError(t, err)
					require.Equal(t, sourceAt, *observed.SourceUpdatedAt)
				}
				// Local scalar edits fence title/body writes but do not strand source tags.
				localTitle, localBody := "Local task", "Local body"
				_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issueID, Actor: "editor", Title: &localTitle, Body: &localBody})
				require.NoError(t, err)
				for _, prefix := range []bool{true, false, true} {
					batch := build(prefix, sourceAt, []string{"stale-remote"})
					// Preserve the current upstream github label even in the same-version
					// payload; the source tag and that upstream label share one identity.
					if collision == "upstream" {
						batch = build(prefix, sourceAt, upstream)
					}
					_, _, err := store.ImportBatch(ctx, batch)
					require.NoError(t, err)
					issue, err := store.IssueByID(ctx, issueID)
					require.NoError(t, err)
					require.Equal(t, localTitle, issue.Title)
					require.Equal(t, localBody, issue.Body)
					want := append([]string{}, expected...)
					if !prefix || collision != "none" {
						want = append(want, provider)
					}
					assertLabels(want...)
				}
				// A newly observed source version may still predate local scalar edits.
				// Its presentation tag applies immediately without importing remote labels.
				pendingAt := sourceAt.Add(time.Minute)
				pending := build(false, pendingAt, []string{"pending-remote"})
				if collision == "upstream" {
					pending = build(false, pendingAt, upstream)
				}
				_, _, err = store.ImportBatch(ctx, pending)
				require.NoError(t, err)
				pendingIssue, err := store.IssueByID(ctx, issueID)
				require.NoError(t, err)
				require.True(t, pendingIssue.UpdatedAt.After(pendingAt))
				require.Equal(t, localTitle, pendingIssue.Title)
				require.Equal(t, localBody, pendingIssue.Body)
				assertLabels(append(expected, provider)...)
				// Repeating an older true-prefix observation cannot remove that tag.
				_, staleEvents, err := store.ImportBatch(ctx, build(true, sourceAt, upstream))
				require.NoError(t, err)
				require.Empty(t, staleEvents)
				assertLabels(append(expected, provider)...)
				// A genuinely newer source version still follows normal import ownership.
				current, err := store.IssueByID(ctx, issueID)
				require.NoError(t, err)
				newer := build(false, current.UpdatedAt.Add(time.Hour), []string{"new-remote"})
				result, _, err := store.ImportBatch(ctx, newer)
				require.NoError(t, err)
				require.Equal(t, 1, result.Updated)
				refreshed, err := store.IssueByID(ctx, issueID)
				require.NoError(t, err)
				require.Equal(t, "Source task", refreshed.Title)
				require.Equal(t, newer.Items[0].Body, refreshed.Body)
				want := []string{"local", provider}
				if provider == "github" {
					want = append(want, "new-remote")
				}
				assertLabels(want...)
			})
		}
	}
	return nil
}

// Retaining the latest observed source time prevents a second older replay
// from being mistaken for a current presentation refresh after a local edit.
func checkImportPresentationLabelStaleReplay(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "example-stale-replay")
	require.NoError(t, err)
	config := notionsync.Config{DataSourceID: "11111111-1111-4111-8111-111111111111", DatabaseID: "22222222-2222-4222-8222-222222222222", TitlePropertyID: "title", StatusPropertyID: "status", AssigneePropertyID: "people", DoneStatusIDs: []string{"done"}, TitlePrefix: new(false)}
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	page := notionsync.PageContent{Page: notionsync.Page{ID: "33333333-3333-4333-8333-333333333333", DataSourceID: config.DataSourceID, URL: "https://www.notion.so/example-page", CreatedAt: now.Add(-time.Hour), UpdatedAt: now}, Title: "Source task"}
	batch, err := notionsync.BuildImportBatch("notion:"+config.DataSourceID, config, []notionsync.PageContent{page})
	require.NoError(t, err)
	batch.ProjectID = project.ID
	_, _, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, batch.Source, "issue", batch.Items[0].ExternalID)
	require.NoError(t, err)
	local := "Local task"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: *mapping.IssueID, Actor: "editor", Title: &local})
	require.NoError(t, err)
	// A synthetic parent-only item must not reinterpret the local edit time
	// as an observed provider version. Exercise that path on both backends.
	current, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	synthetic := batch
	synthetic.ReconcileLabelsForUnchanged = map[string][]string{}
	synthetic.Items = append([]db.ImportItem(nil), batch.Items...)
	synthetic.Items[0].Title = local
	synthetic.Items[0].UpdatedAt = current.UpdatedAt
	synthetic.Items[0].Labels = nil
	_, syntheticEvents, err := store.ImportBatch(ctx, synthetic)
	require.NoError(t, err)
	require.Empty(t, syntheticEvents)
	observedSynthetic, err := store.ImportMappingBySource(ctx, project.ID, batch.Source, "issue", batch.Items[0].ExternalID)
	require.NoError(t, err)
	require.Equal(t, now, *observedSynthetic.SourceUpdatedAt)
	config.TitlePrefix = new(true)
	page.Page.UpdatedAt = now.Add(-time.Minute)
	stale, err := notionsync.BuildImportBatch(batch.Source, config, []notionsync.PageContent{page})
	require.NoError(t, err)
	stale.ProjectID = project.ID
	for range 2 {
		_, events, err := store.ImportBatch(ctx, stale)
		require.NoError(t, err)
		require.Empty(t, events)
	}
	labels, err := store.LabelsByIssue(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, []string{"notion"}, importLabelNames(labels))
	observed, err := store.ImportMappingBySource(ctx, project.ID, batch.Source, "issue", batch.Items[0].ExternalID)
	require.NoError(t, err)
	require.Equal(t, now, *observed.SourceUpdatedAt)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, local, issue.Title)
	return nil
}
