//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func replaceExportOutput(tmpName, output string) error {
	from, err := windows.UTF16PtrFromString(tmpName)
	if err != nil {
		return fmt.Errorf("encode export temp path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(output)
	if err != nil {
		return fmt.Errorf("encode export output path: %w", err)
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("replace export output: %w", err)
	}
	return nil
}
