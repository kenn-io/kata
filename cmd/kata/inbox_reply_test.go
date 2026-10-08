package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestInboxCommentAndBroadcastPresentation(t *testing.T) {
	old := flags
	t.Cleanup(func() { flags = old })
	flags = globalFlags{}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	requests := []inboxRequest{{Ref: "abcd", Title: "Finding", From: "worker", Teammate: "review", Message: "inspect evidence", Re: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Kind: "refute"}, {Ref: "efgh", Title: "Coordination", From: "lead", Message: "check finding", Broadcast: true}}
	require.NoError(t, printInbox(cmd, "reader", requests, false))
	require.Contains(t, out.String(), "latest: refute c:9g5fav by worker/review")
	require.Contains(t, out.String(), "broadcast from lead")
}

// A project handle can exceed six characters after a collision. The inbox
// must preserve the writing daemon's selected handle.
func TestInboxKeepsExtendedStoredHandle(t *testing.T) {
	request := inboxRequest{From: "worker", Re: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Kind: "refute", Message: "latest: refute c:69g5fav by worker"}
	require.Equal(t, request.Message, inboxAttentionText(request))
}

func TestInboxExplicitPointerUsesProjectWideUID(t *testing.T) {
	request := inboxRequest{From: "worker", Re: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Message: "Inspect finding"}
	// c:<suffix> is issue-relative; --re can point to another issue.
	require.Equal(t, "re "+request.Re+": Inspect finding", inboxAttentionText(request))
}

func FuzzInboxKeepsCommentHandle(f *testing.F) {
	f.Add(uint8(7))
	f.Fuzz(func(t *testing.T, length uint8) {
		uid := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		n := 6 + int(length)%5
		message := "latest: refute c:" + strings.ToLower(uid[len(uid)-n:]) + " by worker"
		request := inboxRequest{From: "worker", Re: uid, Kind: "refute", Message: message}
		require.Equal(t, message, inboxAttentionText(request))
	})
}

func TestInboxBroadcastSenderAppearsOnce(t *testing.T) {
	old := flags
	t.Cleanup(func() { flags = old })
	flags = globalFlags{}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, printInbox(cmd, "reader", []inboxRequest{{Ref: "abcd", Title: "Coordination", From: "lead", Message: "Inspect finding", Broadcast: true}}, false))
	require.Contains(t, out.String(), "broadcast from lead: Inspect finding")
	require.Equal(t, 1, strings.Count(out.String(), "lead"))
}
