package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentsViewCommandsRequireExplicitFilesAndDigest(t *testing.T) {
	for _, args := range [][]string{
		{"agentsview", "prepare", "example#abcd"},
		{"agentsview", "share", "consent.json"},
		{"close", "example#abcd", "--done", "-m", "A substantive completion message for this selected issue.", "--reviewed", "README.md", "--snapshot", "snapshot.json"},
	} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		require.Error(t, root.ExecuteContext(t.Context()))
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
	close, _, err := root.Find([]string{"close"})
	require.NoError(t, err)
	require.NotNil(t, close.Flags().Lookup("share-transcript"))
	require.NotNil(t, close.Flags().Lookup("confirm-transcript-sha256"))
}
