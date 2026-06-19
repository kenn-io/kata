// Package release holds release artifact naming helpers shared by tests and CI.
package release

import "go.kenn.io/kit/selfupdate"

// BinaryName is the executable name used in release archives.
const BinaryName = "kata"

// AssetName returns the kit-compatible release archive name for a platform.
func AssetName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return selfupdate.DefaultAssetName(selfupdate.AssetRequest{
		BinaryName: BinaryName,
		Version:    version,
		GOOS:       goos,
		GOARCH:     goarch,
		Extension:  ext,
	})
}
