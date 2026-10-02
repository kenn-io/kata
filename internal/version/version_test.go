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
		return &debug.BuildInfo{Main: debug.Module{Path: "go.kenn.io/kata"}, Settings: settings}, true
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

// Tagged module-proxy installs are releases even without VCS settings. Local
// modules and pseudo-version installs still require migration consent.
func TestModuleBuildMigrationClassification(t *testing.T) {
	for _, tc := range []struct {
		moduleVersion string
		development   bool
	}{
		{"v0.18.0", false}, {"v0.19.0-rc.1", false},
		{"(devel)", true}, {"", true},
		{"v0.18.1-0.20261001000000-123456789abc", true},
	} {
		t.Run(tc.moduleVersion, func(t *testing.T) {
			originalReader, originalVersion := readBuildInfo, Version
			t.Cleanup(func() { readBuildInfo, Version = originalReader, originalVersion })
			readBuildInfo = func() (*debug.BuildInfo, bool) {
				return &debug.BuildInfo{Main: debug.Module{Path: "go.kenn.io/kata", Version: tc.moduleVersion}}, true
			}
			Version = versionFromVCS()
			assert.Equal(t, tc.development, IsDevelopment())
			if tc.moduleVersion != "" && tc.moduleVersion != "(devel)" {
				assert.Equal(t, tc.moduleVersion, Version)
			}
		})
	}
}

func TestEmbeddedVersionIgnoresHostBuild(t *testing.T) {
	for _, hostVersion := range []string{"v1.2.3", "(devel)"} {
		t.Run(hostVersion, func(t *testing.T) {
			original := readBuildInfo
			t.Cleanup(func() { readBuildInfo = original })
			info := &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/host", Version: hostVersion},
				Deps: []*debug.Module{{Path: "go.kenn.io/kata", Version: "v0.18.0"}},
			}
			if hostVersion == "(devel)" {
				info.Settings = []debug.BuildSetting{{Key: settingRevision, Value: "abcdef012345"}, {Key: settingModified, Value: "true"}}
			}
			readBuildInfo = func() (*debug.BuildInfo, bool) { return info, true }
			assert.Equal(t, "v0.18.0", versionFromVCS())
			info.Deps[0].Replace = &debug.Module{Path: "../kata"}
			assert.Equal(t, "dev", versionFromVCS(), "a local replacement has no release identity")
		})
	}
}

func TestDevelopmentVersionStrings(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	for _, tc := range []struct {
		version     string
		development bool
	}{
		{"1234567", true}, {"v1234567", true}, {"v1.2", true},
		{"v0.18.0-SNAPSHOT-abcdef0", true},
		{"v0.18.0", false}, {"0.18.0", false}, {"v0.19.0-rc.1", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			Version = tc.version
			assert.Equal(t, tc.development, IsDevelopment())
		})
	}
}
