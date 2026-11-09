package daemon

import (
	"go.kenn.io/kata/internal/tracker"
	"testing"
)

func TestTrackerPrincipalDoesNotUpgradeClientClass(t *testing.T) {
	const instance = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, kind := range []PrincipalKind{PrincipalBootstrap, PrincipalWebLocal, PrincipalTrustedProxy, PrincipalTrustedProxyAbsent, PrincipalHost} {
		ctx := WithPrincipal(t.Context(), Principal{Kind: kind, Actor: "member", Subject: "member"})
		if _, err := trackerRequestPrincipal(ctx, instance); err == nil {
			t.Fatalf("unsupported client %s gained tracker authority", kind)
		}
	}
	if _, err := trackerRequestPrincipal(t.Context(), instance); err == nil {
		t.Fatal("missing local transport proof accepted")
	}
	ctx := WithPrincipal(t.Context(), Principal{Kind: PrincipalDBToken, Actor: "member", TokenID: 1})
	p, err := trackerRequestPrincipal(ctx, instance)
	if err != nil || p.Kind != tracker.SubjectAccount || p.Subject != "member" || p.AuthorityInstanceUID != instance {
		t.Fatal("verified account mapping", err)
	}
	static := WithPrincipal(t.Context(), Principal{Kind: PrincipalStaticToken, Actor: "ignored"})
	p, err = trackerRequestPrincipal(static, instance)
	if err != nil || p.Kind != tracker.SubjectStaticOwner || p.Subject != instance {
		t.Fatal("static owner mapping", err)
	}
	readonly := withInsecureReadonlyRequest(static)
	if _, err := trackerRequestPrincipal(readonly, instance); err == nil {
		t.Fatal("readonly principal upgraded")
	}
}
