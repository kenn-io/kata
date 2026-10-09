package main

import (
	"strings"
	"testing"
)

func TestAgentFriendlyHelpForProjectAccessAndRelayCommands(t *testing.T) {
	tests := []struct {
		path     []string
		long     string
		examples []string
	}{
		{
			path: []string{"teams"},
			long: "Teams decide which canonical actors can see which projects (see kata projects access). Needs the daemon owner's authority. Team arguments take a name or UID.",
		},
		{
			path:     []string{"teams", "create"},
			long:     "Create a team. Add members with teams members add, then grant it projects with projects access set.",
			examples: []string{"  kata teams create reviewers --agent"},
		},
		{
			path:     []string{"teams", "list"},
			long:     "List the project-access teams on this daemon.",
			examples: []string{"  kata teams list --agent"},
		},
		{
			path:     []string{"teams", "show"},
			long:     "Show one team and its members.",
			examples: []string{"  kata teams show reviewers --agent"},
		},
		{
			path:     []string{"teams", "members", "add"},
			long:     "Add a canonical account actor to a team. tokens create --team does the same when issuing a token.",
			examples: []string{"  kata teams members add reviewers --actor alice --agent"},
		},
		{
			path:     []string{"teams", "members", "remove"},
			long:     "Remove a canonical account actor from a team.",
			examples: []string{"  kata teams members remove reviewers --actor alice --agent"},
		},
		{
			path: []string{"projects", "access"},
			long: "Control which teams can see a project. Needs the daemon owner's authority.",
		},
		{
			path:     []string{"projects", "access", "show"},
			long:     "Show the project's visibility (all or teams) and the teams that can see it.",
			examples: []string{"  kata projects access show example-project --agent"},
		},
		{
			path: []string{"projects", "access", "set"},
			long: "Set who can see the project: --visibility all, or --visibility teams with one\n--team per team (name or UID). --visibility teams with no --team denies\nordinary access. Fails with conflict (exit 5) if another administrator\nchanged the policy in the meantime; re-read with access show and retry.",
			examples: []string{
				"  kata projects access set example-project --visibility teams --team reviewers --agent",
				"  kata projects access set example-project --visibility all --agent",
			},
		},
		{
			path: []string{"federation", "bridge"},
			long: "Relay one local project through a hub from the daemon catalog. Not the same as the root bridge command, which binds issues to external roots.",
		},
		{
			path: []string{"federation", "bridge", "connect"},
			long: "Enroll the --project project with an existing project on a hub from the\ndaemon catalog (--hub-daemon, --hub-project; both required), using the hub\naccount saved in that daemon. --preflight shows the accounts and project\nwithout enrolling. --serve-downstream=false makes this daemon a leaf.\nRetrying after a lost response reuses the pending enrollment.",
			examples: []string{
				"  kata federation bridge connect --project example-project --hub-daemon hub --hub-project example-project --preflight --agent",
				"  kata federation bridge connect --project example-project --hub-daemon hub --hub-project example-project --agent",
			},
		},
		{
			path:     []string{"federation", "bridge", "status"},
			long:     "Show the bridge state recorded locally for --project, without contacting the hub. A pending enrollment shows before the local replica exists.",
			examples: []string{"  kata federation bridge status --project example-project --agent"},
		},
		{
			path: []string{"federation", "bridge", "disconnect"},
			long: "Revoke the bridge grant for --project and keep the local project, detached.\nResolve pending deliveries and downstream enrollments first; --preflight\nchecks them without contacting the hub or changing state. Safe to retry\nafter an interruption.",
			examples: []string{
				"  kata federation bridge disconnect --project example-project --preflight --agent",
				"  kata federation bridge disconnect --project example-project --agent",
			},
		},
		{
			path:     []string{"federation", "embedding-recipe"},
			long:     "Export the exact document recipe from the selected Kata home's configuration. This does not inspect a remote daemon, open a vector index, or make an embedding request. Use the JSON with the root's existing project metadata API to select a producer.",
			examples: []string{"  kata federation embedding-recipe"},
		},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.path, " "), func(t *testing.T) {
			root := newRootCmd()
			cmd, _, err := root.Find(tt.path)
			if err != nil {
				t.Fatalf("find command: %v", err)
			}
			if got := cmd.Long; got != tt.long {
				t.Errorf("Long = %q, want %q", got, tt.long)
			}
			gotExamples := strings.Split(strings.TrimSuffix(cmd.Example, "\n"), "\n")
			if len(tt.examples) == 0 {
				if cmd.Example != "" {
					t.Errorf("Example = %q, want empty", cmd.Example)
				}
				return
			}
			if len(gotExamples) != len(tt.examples) {
				t.Fatalf("Example lines = %q, want %q", gotExamples, tt.examples)
			}
			for i, example := range tt.examples {
				if gotExamples[i] != example {
					t.Errorf("Example line %d = %q, want %q", i, gotExamples[i], example)
				}
				assertExampleParses(t, example)
			}
		})
	}

	root := newRootCmd()
	teams, _, err := root.Find([]string{"teams"})
	if err != nil {
		t.Fatalf("find teams command: %v", err)
	}
	if teams.GroupID != "admin" {
		t.Errorf("teams GroupID = %q, want %q", teams.GroupID, "admin")
	}
	if !root.ContainsGroup("admin") {
		t.Error("root command does not define the admin command group")
	}
	rootHelp := string(executeRoot(t, newRootCmd(), "--help"))
	if !strings.Contains(rootHelp, "Administrative commands") {
		t.Error("root help does not render the admin command group")
	}
}

func assertExampleParses(t *testing.T, example string) {
	t.Helper()
	words := strings.Fields(example)
	if len(words) < 2 || words[0] != "kata" {
		t.Fatalf("example must start with kata and a command: %q", example)
	}
	root := newRootCmd()
	cmd, args, err := root.Find(words[1:])
	if err != nil {
		t.Fatalf("find example command in %q: %v", example, err)
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Errorf("parse flags in %q: %v", example, err)
		return
	}
	if err := cmd.ValidateArgs(cmd.Flags().Args()); err != nil {
		t.Errorf("validate arguments in %q: %v", example, err)
	}
	if err := cmd.ValidateRequiredFlags(); err != nil {
		t.Errorf("validate required flags in %q: %v", example, err)
	}
}
