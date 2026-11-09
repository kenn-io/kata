package tracker

import "testing"

func controlFixture() ControlReceipt {
	d, p := fixture()
	return ControlReceipt{Version: 1, RootInstanceUID: root, ProjectUID: project, UID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Principal: p, Selection: d.Selection, Kind: ControlBind, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: ControlPending}
}

func TestControlReceiptBindsCallerDigestAndSelection(t *testing.T) {
	c := controlFixture()
	if c.Validate() != nil {
		t.Fatal("valid control receipt rejected")
	}
	for _, change := range []func(*ControlReceipt){func(x *ControlReceipt) { x.Principal.AuthorityInstanceUID = owner }, func(x *ControlReceipt) {
		x.RequestDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}, func(x *ControlReceipt) { x.Selection.Generation = "other" }, func(x *ControlReceipt) { x.Kind = ControlDisable }} {
		other := c
		change(&other)
		if SameControlRequest(c, other) {
			t.Fatal("control identity/digest conflict ignored")
		}
	}
	complete := c
	complete.State = ControlComplete
	complete.Result = &ControlResult{Code: "ok", Enabled: true}
	if complete.Validate() != nil || !SameControlRequest(c, complete) {
		t.Fatal("completion changed request identity")
	}
	complete.Result.Code = "fixture-private-diagnostic"
	if complete.Validate() == nil {
		t.Fatal("unclassified provider diagnostic entered receipt")
	}
}
