package db

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The context is an in-process hint, copied by value and never a wire grant.
func TestScopedExpectationSourceAccess(t *testing.T) {
	_, ok := ScopedSourceAccessFromContext(context.Background())
	require.False(t, ok)
	id := "00000000000000000000000001"
	access := ScopedSourceAccess{View: ScopedViewKey{ViewUID: id, Generation: 1}, Principal: CustodyPrincipal{AuthorityUID: id, BindingUID: id, Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}}
	ctx := WithScopedSourceAccess(context.Background(), access)
	access.Principal.AccountableActor = "forged"
	access.View.Generation = 2
	got, ok := ScopedSourceAccessFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "member", got.Principal.AccountableActor)
	require.Equal(t, int64(1), got.View.Generation)
	got.Principal.RouteDigest = "changed"
	again, ok := ScopedSourceAccessFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, strings.Repeat("a", 64), again.Principal.RouteDigest)
	_, err := json.Marshal(again)
	require.Error(t, err)
	require.Error(t, json.Unmarshal([]byte(`{"view":{},"principal":{}}`), &got))
	_, ok = ScopedSourceAccessFromContext(WithScopedSourceAccess(context.Background(), ScopedSourceAccess{}))
	require.False(t, ok, "malformed hints must not become admitted context")
}
