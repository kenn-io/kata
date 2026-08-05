//go:build !darwin && !linux && !windows

package main

import (
	"context"
	"errors"
)

func platformOpenTarget(context.Context, string) error {
	return errors.New("opening a browser is not supported on this platform")
}
