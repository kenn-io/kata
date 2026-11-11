//go:build windows

package recurrenceclient

import (
	"github.com/thommeo/winianatz"
	"golang.org/x/sys/windows/registry"
)

func nativeLocalZone() (string, bool) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, registry.QUERY_VALUE)
	if err != nil {
		return "", true
	}
	defer func() { _ = key.Close() }()
	name, _, err := key.GetStringValue("TimeZoneKeyName")
	if err != nil {
		return "", true
	}
	return ianaForWindowsZone(name), true
}

func ianaForWindowsZone(name string) string {
	entry, err := winianatz.FromMicrosoftAlias(name)
	if err != nil {
		return ""
	}
	return entry.IANA
}
