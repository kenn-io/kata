package mcpserver

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Check the identity as received on the wire, independently of its builder.
func assertServerIcon(t *testing.T, info map[string]any) {
	t.Helper()
	require.Equal(t, "kata", info["name"])
	require.Equal(t, "test-version", info["version"])
	require.Equal(t, "https://katatracker.com", info["websiteUrl"])
	icons, ok := info["icons"].([]any)
	require.True(t, ok, "server info must advertise an icon")
	require.Len(t, icons, 1)
	icon := icons[0].(map[string]any)
	require.Equal(t, "image/png", icon["mimeType"])
	require.Equal(t, []any{"256x256"}, icon["sizes"])
	source, ok := icon["src"].(string)
	require.True(t, ok)
	const prefix = "data:image/png;base64,"
	require.True(t, strings.HasPrefix(source, prefix))
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(source, prefix))
	require.NoError(t, err)
	require.Less(t, len(data), 16*1024, "keep server info small")
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 256, img.Bounds().Dx())
	require.Equal(t, 256, img.Bounds().Dy())
}
