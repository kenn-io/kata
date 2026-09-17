package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkListOrdering(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	p1, err := store.CreateProject(ctx, "list-ordering-one")
	if err != nil {
		return fmt.Errorf("create first project: %w", err)
	}
	p2, err := store.CreateProject(ctx, "list-ordering-two")
	if err != nil {
		return fmt.Errorf("create second project: %w", err)
	}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: p1.ID,
		Source:    "list-ordering",
		Actor:     "conformance-agent",
		Items: []db.ImportItem{
			{ExternalID: "first", Title: "First", Author: "conformance-agent", Status: "open", CreatedAt: base, UpdatedAt: base.Add(10 * time.Hour), Labels: []string{"tracked"}},
			{ExternalID: "second", Title: "Second", Author: "conformance-agent", Status: "open", CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
			{ExternalID: "tie-first", Title: "Tie first", Author: "conformance-agent", Status: "open", CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(4 * time.Hour)},
			{ExternalID: "tie-second", Title: "Tie second", Author: "conformance-agent", Status: "open", CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(5 * time.Hour), Labels: []string{"tracked"}},
		},
	})
	if err != nil {
		return fmt.Errorf("import first project: %w", err)
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: p2.ID,
		Source:    "list-ordering",
		Actor:     "conformance-agent",
		Items: []db.ImportItem{{
			ExternalID: "other-project", Title: "Other project", Author: "conformance-agent", Status: "open",
			CreatedAt: base.Add(3 * time.Hour), UpdatedAt: base.Add(3 * time.Hour), Labels: []string{"tracked"},
		}},
	})
	if err != nil {
		return fmt.Errorf("import second project: %w", err)
	}

	ids := make(map[string]int64, 5)
	for projectID, names := range map[int64][]string{
		p1.ID: {"first", "second", "tie-first", "tie-second"},
		p2.ID: {"other-project"},
	} {
		for _, name := range names {
			mapping, mappingErr := store.ImportMappingBySource(ctx, projectID, "list-ordering", "issue", name)
			if mappingErr != nil {
				return fmt.Errorf("find mapping %s: %w", name, mappingErr)
			}
			if mapping.IssueID == nil {
				return fmt.Errorf("mapping %s has no issue id", name)
			}
			ids[name] = *mapping.IssueID
		}
	}

	listIDs := func(issues []db.Issue) []int64 {
		out := make([]int64, len(issues))
		for i, issue := range issues {
			out[i] = issue.ID
		}
		return out
	}
	expect := func(label string, got []db.Issue, want ...int64) {
		t.Helper()
		assert.Equal(t, want, listIDs(got), label)
	}

	got, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: p1.ID})
	if err != nil {
		return fmt.Errorf("default scoped list: %w", err)
	}
	expect("default scoped order", got, ids["first"], ids["tie-second"], ids["tie-first"], ids["second"])
	got, err = store.ListIssues(ctx, db.ListIssuesParams{ProjectID: p1.ID, OldestFirst: true})
	if err != nil {
		return fmt.Errorf("oldest scoped list: %w", err)
	}
	expect("oldest scoped order", got, ids["first"], ids["second"], ids["tie-first"], ids["tie-second"])
	got, err = store.ListIssues(ctx, db.ListIssuesParams{ProjectID: p1.ID, OldestFirst: true, Limit: 1})
	if err != nil {
		return fmt.Errorf("oldest scoped limit: %w", err)
	}
	require.Len(t, got, 1)
	expect("oldest scoped limit", got, ids["first"])
	got, err = store.ListIssues(ctx, db.ListIssuesParams{ProjectID: p1.ID, OldestFirst: true, Labels: []string{"tracked"}})
	if err != nil {
		return fmt.Errorf("oldest scoped filter: %w", err)
	}
	expect("oldest scoped filter", got, ids["first"], ids["tie-second"])

	got, err = store.ListAllIssues(ctx, db.ListAllIssuesParams{})
	if err != nil {
		return fmt.Errorf("default global list: %w", err)
	}
	expect("default global order", got, ids["other-project"], ids["tie-second"], ids["tie-first"], ids["second"], ids["first"])
	got, err = store.ListAllIssues(ctx, db.ListAllIssuesParams{OldestFirst: true})
	if err != nil {
		return fmt.Errorf("oldest global list: %w", err)
	}
	expect("oldest global order", got, ids["first"], ids["second"], ids["tie-first"], ids["tie-second"], ids["other-project"])
	got, err = store.ListAllIssues(ctx, db.ListAllIssuesParams{ProjectID: p2.ID, OldestFirst: true})
	if err != nil {
		return fmt.Errorf("oldest project narrowing: %w", err)
	}
	expect("oldest project narrowing", got, ids["other-project"])
	got, err = store.ListAllIssues(ctx, db.ListAllIssuesParams{OldestFirst: true, Labels: []string{"tracked"}, Limit: 1})
	if err != nil {
		return fmt.Errorf("oldest global filter and limit: %w", err)
	}
	require.Len(t, got, 1)
	expect("oldest global filter and limit", got, ids["first"])
	return nil
}
