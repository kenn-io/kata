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
	files, err := fs.Sub(embeddedDistribution, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded web distribution: %w", err)
	}
	return NewHandler(files)
}

// NewEmbeddedHandlerWithReleaseStatus reports whether the embedded assets are a release distribution.
func NewEmbeddedHandlerWithReleaseStatus() (http.Handler, bool, error) {
	files, err := fs.Sub(embeddedDistribution, "dist")
	if err != nil {
		return nil, false, fmt.Errorf("open embedded web distribution: %w", err)
	}
	return newHandlerWithReleaseStatus(files)
}

func newHandlerWithReleaseStatus(files fs.FS) (http.Handler, bool, error) {
	handler, err := newHandler(files)
	if err != nil {
		return nil, false, err
	}
	return handler, validateReleaseCatalog(files, handler.index, handler.assets) == nil, nil
}

// ValidateEmbeddedRelease rejects the compilation stub and incomplete release
// distributions. Installers and release smoke tests call this through the
// hidden CLI verifier after the final binary has been assembled.
func ValidateEmbeddedRelease() error {
	files, err := fs.Sub(embeddedDistribution, "dist")
	if err != nil {
		return fmt.Errorf("open embedded web distribution: %w", err)
	}
	return validateReleaseDistribution(files)
}
