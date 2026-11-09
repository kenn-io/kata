//go:build !linux

package main

import "testing"

func watchOpens(_ *testing.T, _ string) func() {
	return func() {}
}
