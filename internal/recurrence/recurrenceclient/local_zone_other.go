//go:build !windows

package recurrenceclient

func nativeLocalZone() (string, bool) { return "", false }
