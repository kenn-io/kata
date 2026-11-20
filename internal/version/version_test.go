//nolint:revive // var-naming flags `version` as stdlib-conflicting but no such stdlib package exists; matches the production package name.
package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func mockBuildInfo(t *testing.T, revision, buildTime string, modified bool) {
	t.Helper()
	orig := readBuildInfo
	t.Cleanup(func() { readBuildInfo = orig })

	settings := []debug.BuildSetting{
		{Key: settingRevision, Value: revision},
	}
	if buildTime != "" {
		settings = append(settings, debug.BuildSetting{Key: settingBuildTime, Value: buildTime})
	}
	if modified {
		settings = append(settings, debug.BuildSetting{Key: settingModified, Value: "true"})
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: settings}, true
	}
}

func TestVersionFromVCSPrefixesShortRevisionWithG(t *testing.T) {
	mockBuildInfo(t, "1697674abcdef", "", false)
	assert.Equal(t, "g1697674", versionFromVCS())
}

func TestVersionFromVCSPrefixesDirtyRevisionWithG(t *testing.T) {
	mockBuildInfo(t, "1697674abcdef", "", true)
	assert.Equal(t, "g1697674-dirty", versionFromVCS())
}

func TestVersionFromModuleBuildInfo(t *testing.T) {
	orig := readBuildInfo
	t.Cleanup(func() { readBuildInfo = orig })

	for _, tc := range []struct {
		name, moduleVersion, revision, want string
	}{
		{name: "release", moduleVersion: "v1.2.3", want: "v1.2.3"},
		{name: "release with revision", moduleVersion: "v1.2.3", revision: "1697674abcdef", want: "v1.2.3"},
		{name: "development with revision", moduleVersion: "(devel)", revision: "1697674abcdef", want: "g1697674"},
		{name: "development without revision", moduleVersion: "(devel)", want: "dev"},
		{name: "empty version", want: "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readBuildInfo = func() (*debug.BuildInfo, bool) {
				return &debug.BuildInfo{Main: debug.Module{Version: tc.moduleVersion}, Settings: []debug.BuildSetting{{Key: settingRevision, Value: tc.revision}}}, true
			}
			assert.Equal(t, tc.want, versionFromVCS())
		})
	}
}

func TestCommitReturnsShortRevisionFromBuildInfo(t *testing.T) {
	mockBuildInfo(t, "1697674abcdef0123456789", "", false)
	assert.Equal(t, "1697674", commitFromVCS())
}

func TestCommitReturnsUnknownWhenBuildInfoMissingRevision(t *testing.T) {
	mockBuildInfo(t, "", "", false)
	assert.Equal(t, "unknown", commitFromVCS())
}

func TestBuildDateReturnsVCSTime(t *testing.T) {
	mockBuildInfo(t, "1697674abcdef", "2026-02-10T01:00:19Z", false)
	assert.Equal(t, "2026-02-10T01:00:19Z", buildDateFromVCS())
}

func TestBuildDateReturnsUnknownWhenBuildInfoMissingTime(t *testing.T) {
	mockBuildInfo(t, "1697674abcdef", "", false)
	assert.Equal(t, "unknown", buildDateFromVCS())
}
