// Package version exposes the kata build's version string, derived from
// runtime/debug.BuildInfo so a stripped binary still reports a stable
// identifier.
//
//nolint:revive // var-naming flags `version` as stdlib-conflicting but no such stdlib package exists.
package version

import (
	"regexp"
	"runtime/debug"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	defaultVersion   = "dev"
	modulePath       = "go.kenn.io/kata"
	unknown          = "unknown"
	shortHashLen     = 7
	settingRevision  = "vcs.revision"
	settingModified  = "vcs.modified"
	settingBuildTime = "vcs.time"
	dirtySuffix      = "-dirty"
)

var readBuildInfo = debug.ReadBuildInfo

// Version is the build identifier shared by the CLI, daemon, and TUI.
// Release builds can override it with ldflags; development builds use
// Go's embedded VCS metadata.
var Version = defaultVersion

// Commit is the short VCS revision the binary was built from. Release
// builds can override it with ldflags; otherwise it is derived from
// debug.BuildInfo's vcs.revision setting.
var Commit = unknown

// BuildDate is the commit timestamp the binary was built from, formatted
// as RFC3339. Release builds can override it with ldflags; otherwise it
// comes from debug.BuildInfo's vcs.time setting.
var BuildDate = unknown

// Distribution identifies a package manager that owns this binary. An empty
// value denotes an ordinary Kata archive or source build.
var Distribution string

func init() {
	if Version == defaultVersion {
		Version = versionFromVCS()
	}
	if Commit == unknown {
		Commit = commitFromVCS()
	}
	if BuildDate == unknown {
		BuildDate = buildDateFromVCS()
	}
}

func versionFromVCS() string {
	info, ok := readBuildInfo()
	if !ok {
		return defaultVersion
	}
	// VCS settings and Main describe the executable, which may embed Kata.
	if info.Main.Path != modulePath {
		for _, dep := range info.Deps {
			if dep.Path == modulePath {
				if dep.Replace != nil {
					dep = dep.Replace
				}
				if semver.IsValid(dep.Version) {
					return dep.Version
				}
				break
			}
		}
		return defaultVersion
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case settingRevision:
			rev = s.Value
		case settingModified:
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		// Module-proxy installations carry their tag in Main.Version rather
		// than VCS settings. Keep that identity for release migration policy.
		if semver.IsValid(info.Main.Version) {
			if dirty {
				return info.Main.Version + dirtySuffix
			}
			return info.Main.Version
		}
		return defaultVersion
	}
	if len(rev) > shortHashLen {
		rev = rev[:shortHashLen]
	}
	rev = "g" + rev
	if dirty {
		rev += dirtySuffix
	}
	return rev
}

func commitFromVCS() string {
	info, ok := readBuildInfo()
	if !ok {
		return unknown
	}
	for _, s := range info.Settings {
		if s.Key == settingRevision && s.Value != "" {
			if len(s.Value) > shortHashLen {
				return s.Value[:shortHashLen]
			}
			return s.Value
		}
	}
	return unknown
}

func buildDateFromVCS() string {
	info, ok := readBuildInfo()
	if !ok {
		return unknown
	}
	for _, s := range info.Settings {
		if s.Key == settingBuildTime && s.Value != "" {
			return s.Value
		}
	}
	return unknown
}

var describeVersion = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)
var fullVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+]|$)`)

// IsDevelopment reports builds without a clean release version, including
// bare VCS revisions and git-describe builds ahead of a release tag.
func IsDevelopment() bool {
	v := Version
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return !fullVersion.MatchString(v) || !semver.IsValid(v) || module.IsPseudoVersion(v) ||
		strings.Contains(v, "dirty") || strings.Contains(strings.ToLower(v), "snapshot") || describeVersion.MatchString(v)
}
