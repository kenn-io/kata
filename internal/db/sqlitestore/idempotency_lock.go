package sqlitestore

import (
	"context"
	"hash/fnv"
	"strconv"
	"sync"
)

const idempotencyLockStripes = 256

type idempotencyLockSet struct {
	stripes [idempotencyLockStripes]chan struct{}
}

func newIdempotencyLockSet() *idempotencyLockSet {
	set := &idempotencyLockSet{}
	for i := range set.stripes {
		set.stripes[i] = make(chan struct{}, 1)
		set.stripes[i] <- struct{}{}
	}
	return set
}

// AcquireIdempotencyLock serializes concurrent create retries within the one
// SQLite daemon that owns this database. SQLite daemon discovery prevents a
// second process from serving the same file; fixed stripes keep memory bounded.
func (d *Store) AcquireIdempotencyLock(
	ctx context.Context,
	projectID int64,
	key string,
) (func() error, error) {
	if key == "" {
		return func() error { return nil }, nil
	}
	locks := d.idempotencyLocks
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strconv.FormatInt(projectID, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key))
	stripe := locks.stripes[hash.Sum64()%idempotencyLockStripes]
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-stripe:
	}
	var once sync.Once
	return func() error {
		once.Do(func() { stripe <- struct{}{} })
		return nil
	}, nil
}
