package fakeconnector

import (
	"context"
	"time"
)

const (
	stateLockTimeout = 10 * time.Second
	stateLockPoll    = 2 * time.Millisecond
)

func lock(path string) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateLockTimeout)
	defer cancel()
	return lockContext(ctx, path+".lock")
}
