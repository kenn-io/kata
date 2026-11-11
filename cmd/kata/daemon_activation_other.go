//go:build !linux

package main

import (
	"context"
	"fmt"
	"net"
)

func adoptActivatedListener(_ context.Context, mode string) (net.Listener, error) {
	return nil, fmt.Errorf("socket activation %q is unsupported on this platform", mode)
}
