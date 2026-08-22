// Package web serves the embedded Kata browser application.
package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

const viteManifestPath = ".vite/manifest.json"
const productionDistributionMarker = `<meta name="kata-web-distribution" content="production"`

type assetCatalog struct {
	immutable map[string]struct{}
}

type viteManifestEntry struct {
	File   string   `json:"file"`
	CSS    []string `json:"css"`
	Assets []string `json:"assets"`
}

func loadAssetCatalog(files fs.FS) (assetCatalog, error) {
	catalog := assetCatalog{immutable: make(map[string]struct{})}
	data, err := fs.ReadFile(files, viteManifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return catalog, nil
	}
	if err != nil {
		return assetCatalog{}, fmt.Errorf("read Vite manifest: %w", err)
	}
	var manifest map[string]viteManifestEntry
	if err := json.Unmarshal(data, &manifest); err != nil {
		return assetCatalog{}, fmt.Errorf("parse Vite manifest: %w", err)
	}
	for _, entry := range manifest {
		assets := append([]string{entry.File}, entry.CSS...)
		assets = append(assets, entry.Assets...)
		for _, name := range assets {
			if name == "" {
				continue
			}
			if !safeAssetName(name) {
				return assetCatalog{}, fmt.Errorf("vite manifest contains unsafe asset path %q", name)
			}
			info, err := fs.Stat(files, name)
			if err != nil {
				return assetCatalog{}, fmt.Errorf("vite manifest asset %q: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				return assetCatalog{}, fmt.Errorf("vite manifest asset %q is not a regular file", name)
			}
			catalog.immutable[name] = struct{}{}
		}
	}
	return catalog, nil
}

func validateReleaseDistribution(files fs.FS) error {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return fmt.Errorf("read release web index: %w", err)
	}
	if !bytes.Contains(index, []byte(productionDistributionMarker)) {
		return errors.New("embedded web distribution is the compilation stub or lacks its production marker")
	}
	if _, err := fs.Stat(files, viteManifestPath); err != nil {
		return fmt.Errorf("release Vite manifest: %w", err)
	}
	catalog, err := loadAssetCatalog(files)
	if err != nil {
		return err
	}
	if len(catalog.immutable) == 0 {
		return errors.New("release Vite manifest has no embedded assets")
	}
	return nil
}

func (c assetCatalog) isImmutable(name string) bool {
	_, ok := c.immutable[name]
	return ok
}

func safeAssetName(name string) bool {
	if !fs.ValidPath(name) || path.Clean(name) != name || strings.Contains(name, `\`) {
		return false
	}
	return !slices.ContainsFunc(strings.Split(name, "/"), unsafePathSegment)
}

func unsafePathSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") {
		return true
	}
	lower := strings.ToLower(segment)
	base := strings.TrimSuffix(lower, path.Ext(lower))
	switch base {
	case "credential", "credentials", "secret", "secrets", "token", "tokens",
		"password", "passwd", "id_rsa", "id_ed25519":
		return true
	}
	switch path.Ext(lower) {
	case ".key", ".pem", ".p12", ".pfx":
		return true
	}
	return false
}
