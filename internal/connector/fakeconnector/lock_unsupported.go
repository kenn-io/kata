//go:build !linux && !darwin && !windows

package fakeconnector

import (
	"context"
	"fmt"
	"runtime"
)

func lockContext(context.Context, string) (func(), error) {
	return nil, fmt.Errorf("fixture state locking is unsupported on %s", runtime.GOOS)
}
