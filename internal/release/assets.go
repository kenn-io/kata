package release

import "go.kenn.io/kit/selfupdate"

const BinaryName = "kata"

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
