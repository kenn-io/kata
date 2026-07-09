//go:build tools

// Package tui keeps Plan 6 toolchain dependencies pinned so go mod tidy
// does not drop them before the first real consumer in Task 4.
//
// Note: the teatest pin pulled colorprofile from v0.2.3-pre to v0.3.2 as
// a transitive bump. Tests pass under the new version, so the upgrade is
// considered benign and intentional.
package tui

import (
	_ "github.com/charmbracelet/x/exp/teatest/v2"
)
