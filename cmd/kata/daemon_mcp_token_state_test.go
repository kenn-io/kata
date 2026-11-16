package main

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestModernTokenStateExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var states modernTokenStates
		expires := time.Now().Add(time.Minute)
		first := states.get(t.Context(), "expiring", expires)
		require.Same(t, first, states.get(t.Context(), "expiring", expires))
		permanent := states.get(t.Context(), "permanent", time.Time{})
		time.Sleep(time.Minute)
		synctest.Wait()
		_, present := states.tokens.Load("expiring")
		require.False(t, present)
		require.Same(t, permanent, states.get(t.Context(), "permanent", time.Time{}))
	})
}
