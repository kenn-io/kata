package main

import (
	"strings"
	"testing"
)

// The team and project-access commands belong to the administration group,
// which the root help renders for agents that rarely need it. Their help text
// and examples are validated with every other command by TestHelpExamplesParse.
func TestAgentFriendlyHelpGroupsAdministrationCommands(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{{"teams"}, {"federation"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("find %v command: %v", path, err)
		}
		if cmd.GroupID != "admin" {
			t.Errorf("%v GroupID = %q, want %q", path, cmd.GroupID, "admin")
		}
	}
	if !root.ContainsGroup("admin") {
		t.Error("root command does not define the admin command group")
	}
	rootHelp := string(executeRoot(t, newRootCmd(), "--help"))
	if !strings.Contains(rootHelp, "Administer (rarely needed by agents):") {
		t.Error("root help does not render the admin command group")
	}
}
