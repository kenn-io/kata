package daemon

import (
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

func enqueueHookWithDrain(sink hooks.Sink, event db.Event, parent *activity.Lease) {
	if parent == nil {
		sink.Enqueue(event)
		return
	}
	hooks.EnqueueFrom(sink, event, parent.Fork)
}
