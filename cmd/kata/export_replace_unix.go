//go:build !windows

package main

import (
	"fmt"
	"os"
)

func replaceExportOutput(tmpName, output string) error {
	if err := os.Rename(tmpName, output); err != nil { //nolint:gosec // output is the user-requested export destination; tmpName comes from os.CreateTemp in the same dir.
		return fmt.Errorf("replace export output: %w", err)
	}
	return nil
}
