package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLinkPatchRecipients(t *testing.T) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			in := LinkInput{Sender: Identity{Actor: "worker", Teammate: "builder"}, Target: Identity{Actor: "reviewer", Teammate: "reader"}, ReplyUID: "new-reply", TargetUID: "old-finding", Kind: kind, Owner: "owner", ParentOwner: "lead", Current: map[string]jsontext.Value{MetadataKey("worker/builder"): jsontext.Value(`{"from":"lead","message":"respond","re":"old-finding"}`)}}
			patch, err := LinkPatch(in)
			require.NoError(t, err)
			var got Value
			require.NoError(t, json.Unmarshal(patch[MetadataKey("reviewer/reader")], &got))
			require.Equal(t, "new-reply", got.Re)
			require.Equal(t, kind, got.Kind)
			require.NotContains(t, patch, MetadataKey("worker/builder"), "SettlePatch owns the sender's request")
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
	in.Current[MetadataKey("reader")] = jsontext.Value(`{"from":"lead","message":"request","re":"old"}`)
	p, e = LinkPatch(in)
	require.NoError(t, e)
	require.Empty(t, p, "a request that names a comment is still a request")
	in.Current[MetadataKey("reader")] = jsontext.Value(`{"from":"lead","message":"link","re":"old","kind":"reply"}`)
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

func FuzzSettlePatchClearsMatchingRequest(f *testing.F) {
	f.Add("target", true, uint8(0))
	f.Add("target", false, uint8(1))
	f.Fuzz(func(t *testing.T, target string, matching bool, kindIndex uint8) {
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
		kinds := [...]string{"reply", "confirm", "refute", "supersede"}
		sender := Identity{Actor: "worker"}
		link, err := LinkValue(sender, "new", kinds[int(kindIndex)%len(kinds)])
		require.NoError(t, err)
		patch := SettlePatch(map[string]jsontext.Value{MetadataKey("worker"): raw}, sender, target, link)
		if matching {
			require.Equal(t, "null", string(patch[MetadataKey("worker")]))
			require.Equal(t, string(link), string(patch[MetadataKey("lead")]))
		} else {
			require.Empty(t, patch)
		}
	})
}

func TestSettlePatchPreservesRequestsAndSkipsNotices(t *testing.T) {
	sender := Identity{Actor: "worker", Teammate: "builder"}
	link, err := LinkValue(sender, "new", "confirm")
	require.NoError(t, err)
	key := MetadataKey("worker/builder")
	request := jsontext.Value(`{"from":"lead","teammate":"plan","message":"respond","re":"original"}`)

	patch := SettlePatch(map[string]jsontext.Value{key: request, MetadataKey("lead/plan"): jsontext.Value(`{"from":"other","message":"human"}`)}, sender, "original", link)
	require.Equal(t, map[string]jsontext.Value{key: jsontext.Value("null")}, patch, "the requester's own request is preserved")

	notice := jsontext.Value(`{"from":"lead","message":"latest: reply","re":"original","kind":"reply"}`)
	patch = SettlePatch(map[string]jsontext.Value{key: notice}, sender, "original", link)
	require.Equal(t, map[string]jsontext.Value{key: jsontext.Value("null")}, patch, "answering a reply notice tells nobody")

	selfRequest := jsontext.Value(`{"from":"worker","teammate":"builder","message":"note to self","re":"original"}`)
	patch = SettlePatch(map[string]jsontext.Value{key: selfRequest}, sender, "original", link)
	require.Equal(t, map[string]jsontext.Value{key: jsontext.Value("null")}, patch)
}

// Actors are stored as written, but inbox, wait, and notify --to address the
// normalized recipient. Every derived slot key must use that same form.
func TestNotificationKeysUseNormalizedRecipients(t *testing.T) {
	sender := Identity{Actor: " worker "}
	patch, err := LinkPatch(LinkInput{Sender: sender, Target: Identity{Actor: " reviewer ", Teammate: "reader"}, ReplyUID: "new", TargetUID: "target", Kind: "reply"})
	require.NoError(t, err)
	require.Contains(t, patch, MetadataKey("reviewer/reader"))

	link, err := LinkValue(sender, "new", "reply")
	require.NoError(t, err)
	request := jsontext.Value(`{"from":" lead ","message":"respond","re":"target"}`)
	settled := SettlePatch(map[string]jsontext.Value{MetadataKey("worker"): request}, sender, "target", link)
	require.Equal(t, map[string]jsontext.Value{MetadataKey("worker"): jsontext.Value("null"), MetadataKey("lead"): link}, settled)

	got, err := BroadcastRecipients([]Identity{{Actor: " reader "}, {Actor: "worker"}}, sender, false)
	require.NoError(t, err)
	require.Equal(t, []string{"reader"}, got)
}

// Two different authors that no inbox can address are not the same person:
// a confirmation between them still reaches the owners.
func TestLinkPatchUnaddressableAuthorsAreNotSelf(t *testing.T) {
	long := strings.Repeat("a", RecipientMaxBytes+1)
	patch, err := LinkPatch(LinkInput{Sender: Identity{Actor: long + "-worker"}, Target: Identity{Actor: long + "-reviewer"}, ReplyUID: "new", TargetUID: "target", Kind: "confirm", Owner: "owner"})
	require.NoError(t, err)
	require.Contains(t, patch, MetadataKey("owner"))

	self := Identity{Actor: long}
	patch, err = LinkPatch(LinkInput{Sender: self, Target: self, ReplyUID: "new", TargetUID: "target", Kind: "confirm", Owner: "owner"})
	require.NoError(t, err)
	require.Empty(t, patch, "confirming your own comment fans out to nobody")
}
