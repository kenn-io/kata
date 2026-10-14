package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Hybrid search hydrates vector hits while a text search runs on another
// goroutine with the same request context, so both record into one registry.
func TestExposedIssuesRecordConcurrently(t *testing.T) {
	ctx := db.WithExposedIssues(context.Background())
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 500 {
				db.RecordExposedIssue(ctx, int64(worker*1000+i))
				_ = db.ExposedIssues(ctx)
			}
		})
	}
	wg.Wait()
	require.Len(t, db.ExposedIssues(ctx), 8*500)
}

func TestIssueScopeTargetsRecordConcurrently(t *testing.T) {
	ctx := db.WithIssueScopeTargets(context.Background())
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 500 {
				db.RecordIssueScopeTarget(ctx, int64(worker*1000+i))
				_ = db.IssueScopeTargets(ctx)
			}
		})
	}
	wg.Wait()
	require.Len(t, db.IssueScopeTargets(ctx), 8*500)
}
