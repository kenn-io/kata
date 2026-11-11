//go:build windows

package recurrenceclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsLocalZoneDiscovery(t *testing.T) {
	require.Equal(t, "America/New_York", ianaForWindowsZone("Eastern Standard Time"))
	require.Empty(t, ianaForWindowsZone("unknown timezone"))
}

func TestWindowsLocalZoneHonorsTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	require.Equal(t, "Asia/Tokyo", LocalZone(""))
	t.Setenv("TZ", "")
	require.Equal(t, "UTC", LocalZone(""))
}
