//go:build windows

package recurrenceclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsLocalZoneDiscovery(t *testing.T) {
	zone, supported := nativeLocalZone()
	require.True(t, supported)
	require.NotEmpty(t, zone, "Windows must expose a mapped local timezone")
	require.Equal(t, "America/New_York", ianaForWindowsZone("Eastern Standard Time"))
	require.Empty(t, ianaForWindowsZone("unknown timezone"))
}
