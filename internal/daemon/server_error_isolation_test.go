package daemon_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
)

func TestNewServerLeavesHumaErrorFactoryUntouched(t *testing.T) {
	original := huma.NewError
	t.Cleanup(func() { huma.NewError = original })
	originalPointer := reflect.ValueOf(original).Pointer()

	_ = daemon.NewServer(daemon.ServerConfig{})

	require.Equal(t, originalPointer, reflect.ValueOf(huma.NewError).Pointer())
	_, ok := huma.NewError(400, "foreign error").(*huma.ErrorModel)
	require.True(t, ok, "constructing Kata must not change another Huma API's error model")
}

func TestNewServerConcurrentConstructionLeavesHumaErrorFactoryUntouched(t *testing.T) {
	original := huma.NewError
	t.Cleanup(func() { huma.NewError = original })
	originalPointer := reflect.ValueOf(original).Pointer()

	const servers = 16
	var wg sync.WaitGroup
	wg.Add(servers)
	for range servers {
		go func() {
			defer wg.Done()
			_ = daemon.NewServer(daemon.ServerConfig{})
		}()
	}
	wg.Wait()

	require.Equal(t, originalPointer, reflect.ValueOf(huma.NewError).Pointer())
}
