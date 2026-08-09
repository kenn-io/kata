// Package version exposes the kata build's version string, derived from
// runtime/debug.BuildInfo so a stripped binary still reports a stable
// identifier.
//
//nolint:revive // var-naming flags `version` as stdlib-conflicting but no such stdlib package exists.
package version

import "runtime/debug"

const (
	defaultVersion   = "dev"
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
