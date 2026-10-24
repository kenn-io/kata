//go:build !linux

package daemonruntime_test

import (
	"testing"

	kitdaemon "go.kenn.io/kit/daemon"
)

func foreignNamespaceRuntimeRecord(t *testing.T) kitdaemon.RuntimeRecord {
	t.Helper()
	t.Skip("foreign PID namespace fixture requires Linux")
	return kitdaemon.RuntimeRecord{}
}
