package storeopen

import (
	"sync"

	"go.kenn.io/kata/internal/db/sqlitelock"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type lockedStorage struct {
	*sqlitestore.Store
	lock      *sqlitelock.Lock
	closeOnce sync.Once
	closeErr  error
}

func (s *lockedStorage) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.Store.Close()
		s.lock.Release()
	})
	return s.closeErr
}
