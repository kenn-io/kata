package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentsViewCommandsRequireExplicitFilesAndDigest(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"agentsview", "prepare", "example#abcd"}, "--snapshot and --output are required"},
		{[]string{"agentsview", "share", "consent.json"}, "--issue, --snapshot, --close-event and --confirm-transcript-sha256 are required"},
		{[]string{"close", "example#abcd", "--done", "-m", "A substantive completion message for this selected issue.", "--reviewed", "README.md", "--snapshot", "snapshot.json"}, "sharing requires --share-transcript, --snapshot and --confirm-transcript-sha256"},
	} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(tc.args)
		err := root.ExecuteContext(t.Context())
		require.ErrorContains(t, err, tc.message)
		require.Equal(t, ExitValidation, exitCodeForErr(err, true))
	}
}

func TestAgentsViewNamespaceAndFlags(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"prepare", "share"} {
		command, _, err := root.Find([]string{"agentsview", name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
		require.NotNil(t, command.Flags().Lookup("snapshot"))
	}
	closeCmd, _, err := root.Find([]string{"close"})
	require.NoError(t, err)
	require.NotNil(t, closeCmd.Flags().Lookup("share-transcript"))
	require.NotNil(t, closeCmd.Flags().Lookup("confirm-transcript-sha256"))
}
