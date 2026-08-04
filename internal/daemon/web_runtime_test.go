package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebRuntimeMetadata(t *testing.T) {
	runtimeInfo, err := NewWebRuntime(WebRuntimeOptions{
		Origin:       "http://127.0.0.1:27123",
		OriginStable: true,
		Capabilities: []string{"loopback", "sse"},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"web_origin":        "http://127.0.0.1:27123",
		"web_origin_stable": "true",
		"web_capabilities":  "loopback,sse",
	}, runtimeInfo.Metadata())
}
