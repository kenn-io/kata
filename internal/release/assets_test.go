package release

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/selfupdate"
)

func TestAssetNameMatchesKitDefault(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch, ext string
	}{
		{"linux", "amd64", ".tar.gz"},
		{"linux", "arm64", ".tar.gz"},
		{"darwin", "amd64", ".tar.gz"},
		{"darwin", "arm64", ".tar.gz"},
		{"windows", "amd64", ".zip"},
		{"windows", "arm64", ".zip"},
	} {
		t.Run(tc.goos+"_"+tc.goarch, func(t *testing.T) {
			req := selfupdate.AssetRequest{
				BinaryName: "kata",
				Version:    "0.5.0",
				GOOS:       tc.goos,
				GOARCH:     tc.goarch,
				Extension:  tc.ext,
			}
			assert.Equal(t, selfupdate.DefaultAssetName(req), AssetName("0.5.0", tc.goos, tc.goarch))
		})
	}
}

func TestAssetNameUsesBareSemver(t *testing.T) {
	assert.Equal(t, "kata_0.5.0_linux_amd64.tar.gz", AssetName("0.5.0", "linux", "amd64"))
	assert.NotContains(t, AssetName("0.5.0", "linux", "amd64"), "v0.5.0")
}

func TestReleaseArchiveNameScriptMatchesAssetName(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
	}{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
		{"windows", "arm64"},
	} {
		t.Run(tc.goos+"_"+tc.goarch, func(t *testing.T) {
			//nolint:gosec // test executes the repository-local script with fixed args.
			cmd := exec.Command("bash", "../../scripts/release-archive-name.sh", "0.5.0", tc.goos, tc.goarch)
			out, err := cmd.CombinedOutput()
			assert.NoError(t, err, string(out))
			assert.Equal(t, AssetName("0.5.0", tc.goos, tc.goarch), strings.TrimSpace(string(out)))
		})
	}
}
