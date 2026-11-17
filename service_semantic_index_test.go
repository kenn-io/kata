package kata

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
)

func TestServiceSemanticIndex(t *testing.T) {
	service, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	service.embeddingReconciler = &daemon.Reconciler{}

	var narrow []string
	for _, worker := range service.selectedWorkers(semanticIndexOnly) {
		narrow = append(narrow, worker.name)
	}
	require.Equal(t, []string{"embeddings"}, narrow)
	var full []string
	for _, worker := range service.selectedWorkers(allWorkers) {
		full = append(full, worker.name)
	}
	require.Equal(t, []string{"federation", "github-sync", "notion-sync", "plane-sync", "linear-sync", "twenty-sync", "todoist-sync", "ticktick-sync", "timed-claim", "due-notification", "assignment-expiry", "embeddings"}, full)
}

func TestServiceSemanticIndexDisabled(t *testing.T) {
	service, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.RunSemanticIndex(ctx) }()
	//nolint:kennlint // sleeptest: wait for the lifecycle to start.
	require.Eventually(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.running
	}, time.Second, time.Millisecond)
	require.ErrorContains(t, service.RunSemanticIndex(ctx), "already running")
	cancel()
	require.NoError(t, <-done)
}
