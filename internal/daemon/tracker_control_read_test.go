package daemon

import (
	"go.kenn.io/kata/internal/tracker"
	"testing"
)

// A declared MCP client may use its real database-token account only. It cannot
// turn an owner-local/static self-call into a member connection grant.
func TestTrackerMCPClientDoesNotBorrowOwnerAuthority(t *testing.T) {
	const instance = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, kind := range []PrincipalKind{PrincipalStaticToken, PrincipalBootstrap} {
		ctx := WithPrincipal(t.Context(), Principal{Kind: kind, Actor: "member"})
		ctx, err := trackerClientContext(ctx, "mcp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := trackerRequestPrincipal(ctx, instance); err == nil {
			t.Fatal("MCP borrowed owner authority", kind)
		}
	}
	ctx := WithPrincipal(t.Context(), Principal{Kind: PrincipalDBToken, Actor: "member", TokenID: 1})
	ctx, err := trackerClientContext(ctx, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	p, err := trackerRequestPrincipal(ctx, instance)
	if err != nil || p.Kind != tracker.SubjectAccount {
		t.Fatal("real MCP account denied", err)
	}
	if _, err := trackerClientContext(ctx, "invented"); err == nil {
		t.Fatal("unknown client class")
	}
}
