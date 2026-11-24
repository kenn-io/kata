package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var embeddedDistribution embed.FS

// NewEmbeddedHandler serves the distribution compiled into the Kata binary.
func NewEmbeddedHandler() (http.Handler, error) {
	files, err := embeddedFiles()
	if err != nil {
		return nil, err
	}
	return NewHandler(files)
}

// NewEmbeddedHandlerWithReleaseStatus also reports whether the embedded assets are a release distribution.
func NewEmbeddedHandlerWithReleaseStatus() (http.Handler, bool, error) {
	files, err := embeddedFiles()
	if err != nil {
		return nil, false, err
	}
	return newHandlerWithReleaseStatus(files)
}

func newHandlerWithReleaseStatus(files fs.FS) (http.Handler, bool, error) {
	h, err := newHandler(files)
	if err != nil {
		return nil, false, err
	}
	return h, validateReleaseCatalog(files, h.index, h.assets) == nil, nil
}

func embeddedFiles() (fs.FS, error) {
	files, err := fs.Sub(embeddedDistribution, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded web distribution: %w", err)
	}
	return files, nil
}

// ValidateEmbeddedRelease rejects the compilation stub and incomplete release
// distributions. Installers and release smoke tests call this through the
// hidden CLI verifier after the final binary has been assembled.
func ValidateEmbeddedRelease() error {
	files, err := embeddedFiles()
	if err != nil {
		return err
	}
	return validateReleaseDistribution(files)
}
