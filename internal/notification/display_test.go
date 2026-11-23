package notification

import "testing"

func TestDisplayValue(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	actor := "Ana [member:" + id + "; enrollment:" + id + "]"
	original := Value{From: actor, Teammate: "worker", Kind: "reply", Re: "uid", Message: "latest: reply c:abc by " + actor + "/worker"}
	got := DisplayValue(original)
	if got.From != "Ana" || got.Message != "latest: reply c:abc by Ana/worker" {
		t.Fatalf("display: %+v", got)
	}
	for _, plain := range []Value{
		{From: actor, Re: "uid", Message: "latest:  by " + actor},
		{From: actor, Kind: "reply", Message: "latest: reply by " + actor},
		{From: actor, Kind: "reply", Re: "uid", Message: "other: reply by " + actor},
	} {
		if got := DisplayValue(plain); got.Message != plain.Message {
			t.Fatalf("message changed: %q", got.Message)
		}
	}
}
