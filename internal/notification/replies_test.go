package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: linked requests retain their pointer and kind through serialization.
func TestCommentNotificationValueRoundTrip(t *testing.T) {
	raw := jsontext.Value(`{"from":"worker","teammate":"reviewer","message":"open this finding","re":"01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"refute","broadcast":true}`)
	var v Value
	require.NoError(t, json.Unmarshal(raw, &v))
	got, err := json.Marshal(v)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(got))
}

func FuzzCommentNotificationRoundTrip(f *testing.F) {
	f.Add("worker", "reviewer", "open finding", "reply", true)
	f.Fuzz(func(t *testing.T, actor, tm, message, kind string, broadcast bool) {
		if len(actor)+len(tm)+len(message)+len(kind) > 2048 {
			return
		}
		want := map[string]any{"from": actor, "message": message, "re": "01ARZ3NDEKTSV4RRFFQ69G5FAV"}
		if tm != "" {
			want["teammate"] = tm
		}
		if kind != "" {
			want["kind"] = kind
		}
		if broadcast {
			want["broadcast"] = true
		}
		raw, err := json.Marshal(want)
		if err != nil {
			return
		}
		var v Value
		require.NoError(t, json.Unmarshal(raw, &v))
		got, err := json.Marshal(v)
		require.NoError(t, err)
		require.JSONEq(t, string(raw), string(got), fmt.Sprint(want))
	})
}

func TestLinkPatchRecipientsAndClearing(t *testing.T) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			in := LinkInput{Sender: Identity{Actor: "worker", Teammate: "builder"}, Target: Identity{Actor: "reviewer", Teammate: "reader"}, ReplyUID: "new-reply", TargetUID: "old-finding", Kind: kind, Owner: "owner", ParentOwner: "lead", Current: map[string]jsontext.Value{MetadataKey("worker/builder"): jsontext.Value(`{"from":"lead","message":"respond","re":"old-finding"}`)}}
			patch, err := LinkPatch(in)
			require.NoError(t, err)
			var got Value
			require.NoError(t, json.Unmarshal(patch[MetadataKey("reviewer/reader")], &got))
			require.Equal(t, "new-reply", got.Re)
			require.Equal(t, kind, got.Kind)
			if kind == "reply" {
				require.Equal(t, "null", string(patch[MetadataKey("worker/builder")]))
			} else {
				require.NotContains(t, patch, MetadataKey("worker/builder"))
			}
			if kind == "confirm" {
				require.Contains(t, patch, MetadataKey("owner"))
				require.Contains(t, patch, MetadataKey("lead"))
			} else {
				require.NotContains(t, patch, MetadataKey("owner"))
			}
		})
	}
}

func TestLinkPatchHumanSelfAndLatest(t *testing.T) {
	in := LinkInput{Sender: Identity{Actor: "worker"}, Target: Identity{Actor: "reader"}, ReplyUID: "new", TargetUID: "target", Kind: "refute", Current: map[string]jsontext.Value{}}
	in.Current[MetadataKey("reader")] = jsontext.Value(`{"from":"lead","message":"human"}`)
	p, e := LinkPatch(in)
	require.NoError(t, e)
	require.Empty(t, p)
	in.Current[MetadataKey("reader")] = jsontext.Value(`{"from":"lead","message":"link","re":"old"}`)
	p, e = LinkPatch(in)
	require.NoError(t, e)
	require.Contains(t, p, MetadataKey("reader"))
	in.Target = in.Sender
	p, e = LinkPatch(in)
	require.NoError(t, e)
	require.Empty(t, p)
	in.Target = Identity{Actor: "reader"}
	in.Kind = "reply"
	in.Current[MetadataKey("worker")] = jsontext.Value(`{"from":"lead","message":"human"}`)
	p, e = LinkPatch(in)
	require.NoError(t, e)
	require.NotContains(t, p, MetadataKey("worker"))
	in.Current[MetadataKey("worker")] = jsontext.Value(`{"from":"lead","message":"other","re":"different"}`)
	p, e = LinkPatch(in)
	require.NoError(t, e)
	require.NotContains(t, p, MetadataKey("worker"))
}

func TestLinkPatchConfirmBoundAndPendingTarget(t *testing.T) {
	in := LinkInput{Sender: Identity{Actor: "worker"}, Target: Identity{Actor: "reader"}, ReplyUID: "new", TargetUID: "target", Kind: "confirm", Owner: "owner", ParentOwner: "lead", PendingTargets: map[string]string{"reader": "target"}}
	for i := range 12 {
		in.PriorLinkers = append(in.PriorLinkers, Identity{Actor: fmt.Sprintf("linker-%02d", i)}, Identity{Actor: fmt.Sprintf("linker-%02d", i)})
	}
	p, e := LinkPatch(in)
	require.NoError(t, e)
	require.Len(t, p, 10)
	require.NotContains(t, p, MetadataKey("reader"))
	require.Contains(t, p, MetadataKey("linker-07"))
	require.NotContains(t, p, MetadataKey("linker-08"))
}

func FuzzLinkPatchHumanPreservation(f *testing.F) {
	f.Add("please inspect", "refute")
	f.Fuzz(func(t *testing.T, message, kind string) {
		if len(message) > 1024 {
			return
		}
		if kind != "reply" && kind != "confirm" && kind != "refute" && kind != "supersede" {
			return
		}
		raw, e := json.Marshal(map[string]any{"from": "lead", "message": message})
		if e != nil {
			return
		}
		in := LinkInput{Sender: Identity{Actor: "worker"}, Target: Identity{Actor: "reader"}, ReplyUID: "new", TargetUID: "target", Kind: kind, Current: map[string]jsontext.Value{MetadataKey("reader"): raw}}
		p, e := LinkPatch(in)
		require.NoError(t, e)
		require.NotContains(t, p, MetadataKey("reader"))
	})
}

// Shrunk fuzz counterexample 82e514e543e03aad: json/v2 omitempty retains false.
func TestCommentNotificationFalseBroadcastOmitted(t *testing.T) {
	raw := jsontext.Value(`{"from":"0","teammate":"0","message":"0","re":"01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"0"}`)
	var v Value
	require.NoError(t, json.Unmarshal(raw, &v))
	got, err := json.Marshal(v)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(got))
}

func TestLegacyAnswerKindDoesNotClearRequest(t *testing.T) {
	in := LinkInput{Sender: Identity{Actor: "worker"}, Target: Identity{Actor: "reader"}, ReplyUID: "new", TargetUID: "target", Kind: "answer", Current: map[string]jsontext.Value{MetadataKey("worker"): jsontext.Value(`{"from":"lead","message":"respond","re":"target"}`)}}
	patch, err := LinkPatch(in)
	require.NoError(t, err)
	require.NotContains(t, patch, MetadataKey("worker"))
}

func FuzzReplyAutoClear(f *testing.F) {
	f.Add("target", true)
	f.Fuzz(func(t *testing.T, target string, matching bool) {
		if target == "" || len(target) > 128 {
			return
		}
		re := target
		if !matching {
			re = "other-" + target
		}
		raw, err := json.Marshal(Value{From: "lead", Message: "Respond to this comment", Re: re})
		if err != nil {
			return
		}
		patch, err := LinkPatch(LinkInput{Sender: Identity{Actor: "worker"}, Target: Identity{Actor: "reader"}, ReplyUID: "new", TargetUID: target, Kind: "reply", Current: map[string]jsontext.Value{MetadataKey("worker"): raw}})
		require.NoError(t, err)
		if matching {
			require.Equal(t, "null", string(patch[MetadataKey("worker")]))
		} else {
			require.NotContains(t, patch, MetadataKey("worker"))
		}
	})
}
