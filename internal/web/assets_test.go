package web

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerReleaseStatusRejectsCompilationStub(t *testing.T) {
	stub := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("Kata UI assets are not built")},
	}
	_, release, err := newHandlerWithReleaseStatus(stub)
	require.NoError(t, err)
	assert.False(t, release)
}

func TestHandlerReleaseStatusAcceptsProductionMarkerAndManifest(t *testing.T) {
	files := fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte(`<meta name="kata-web-distribution" content="production"><script src="/assets/app-a1b2c3d4.js"></script>`)},
		".vite/manifest.json":    &fstest.MapFile{Data: []byte(`{"entry":{"file":"assets/app-a1b2c3d4.js","isEntry":true}}`)},
		"assets/app-a1b2c3d4.js": &fstest.MapFile{Data: []byte("export const ready = true")},
	}
	_, release, err := newHandlerWithReleaseStatus(files)
	require.NoError(t, err)
	assert.True(t, release)
}

func TestWebHandlerRejectsUnsafeManifestAssetPaths(t *testing.T) {
	unsafeManifests := map[string]string{ //nolint:gosec // Deliberately credential-like filenames exercise rejection.
		"escape":     `{"entry":{"file":"../outside.js"}}`,
		"absolute":   `{"entry":{"file":"/assets/app.js"}}`,
		"hidden":     `{"entry":{"file":"assets/.hidden.js"}}`,
		"credential": `{"entry":{"file":"assets/client.pem"}}`,
	}
	for name, manifest := range unsafeManifests {
		t.Run(name, func(t *testing.T) {
			_, err := NewHandler(fstest.MapFS{
				"index.html":          &fstest.MapFile{Data: []byte("stub")},
				".vite/manifest.json": &fstest.MapFile{Data: []byte(manifest)},
			})
			assert.Error(t, err)
		})
	}
}

func TestWebHandlerRejectsMalformedManifest(t *testing.T) {
	_, err := NewHandler(fstest.MapFS{
		"index.html":          &fstest.MapFile{Data: []byte("stub")},
		".vite/manifest.json": &fstest.MapFile{Data: []byte(`{"entry":`)},
	})
	assert.Error(t, err)
}
