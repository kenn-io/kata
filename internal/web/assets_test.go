package web

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
)

func TestValidateReleaseDistributionRejectsCompilationStub(t *testing.T) {
	stub := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("Kata UI assets are not built")},
	}
	assert.Error(t, validateReleaseDistribution(stub))
}

func TestValidateReleaseDistributionAcceptsProductionMarkerAndManifest(t *testing.T) {
	files := fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte(`<meta name="kata-web-distribution" content="production"><script src="/assets/app-a1b2c3d4.js"></script>`)},
		".vite/manifest.json":    &fstest.MapFile{Data: []byte(`{"entry":{"file":"assets/app-a1b2c3d4.js","isEntry":true}}`)},
		"assets/app-a1b2c3d4.js": &fstest.MapFile{Data: []byte("export const ready = true")},
	}
	assert.NoError(t, validateReleaseDistribution(files))
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
