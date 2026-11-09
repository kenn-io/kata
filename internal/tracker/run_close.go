package tracker

import (
	"context"
	"sync"
)

type runCloserKey struct{}
type runCloser struct {
	once  sync.Once
	close func(context.Context) error
	err   error
}

func WithRunCloser(ctx context.Context, close func(context.Context) error) context.Context {
	return context.WithValue(ctx, runCloserKey{}, &runCloser{close: close})
}

// CloseRun releases only volatile owner resources. Callers supply a bounded
// cleanup context when closing must survive cancellation of the root run.
func CloseRun(ctx context.Context) error {
	closer, ok := ctx.Value(runCloserKey{}).(*runCloser)
	if !ok || closer.close == nil {
		return nil
	}
	closer.once.Do(func() { closer.err = closer.close(ctx) })
	return closer.err
}
