package joincommand_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/federation/joincommand"
)

func TestJoinCommandPreservesArguments(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell argument parsing requires a POSIX shell")
	}
	for _, value := range []string{"spoke-project", "project with spaces", "project'quote", "project\nsecond line", "$(echo exposed); * | &"} {
		t.Run(value, func(t *testing.T) {
			in := api.FederationJoinInstructions{
				ProjectName: value, HubURL: "https://hub.example", HubProjectID: 42,
				HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", Token: value, Actor: value,
				Capabilities: "claim,pull,push", PushEnabled: true, AdoptExisting: true,
				AllowInsecure: true, ReplayHorizonEventID: 7,
			}
			// Execute only shell argument parsing. The kata function captures arguments
			// as NUL-separated words rather than launching the enrollment command.
			script := `kata() { printf '%s\0' "$@"; }; ` + joincommand.Build(in)
			out, err := exec.Command("sh", "-c", script).CombinedOutput() //nolint:gosec // G204: tests the generated command against a shell stub that only captures arguments.
			require.NoError(t, err, "%s", out)
			args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			assert.Equal(t, []string{
				"federation", "join", "--project", value, "--hub-url", "https://hub.example",
				"--hub-project-id", "42", "--token", value,
				"--capabilities", "lease,pull,push", "--actor", value, "--push", "--allow-insecure", "--adopt-existing",
			}, args)
		})
	}
}

func TestJoinCommandRequiresJoinAuthority(t *testing.T) {
	base := api.FederationJoinInstructions{ProjectName: "spoke-project", HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", Token: "join-token", Capabilities: "pull", Actor: "tester", ReplayHorizonEventID: 7}
	for _, mutate := range []func(*api.FederationJoinInstructions){
		func(in *api.FederationJoinInstructions) { in.Capabilities = "push" },
		func(in *api.FederationJoinInstructions) { in.HubURL = "" },
		func(in *api.FederationJoinInstructions) { in.ProjectName = "" },
		func(in *api.FederationJoinInstructions) { in.HubProjectID = 0 },
		func(in *api.FederationJoinInstructions) { in.Token = "" },
		func(in *api.FederationJoinInstructions) { in.Actor = "" },
		func(in *api.FederationJoinInstructions) { in.PushEnabled = true },
		func(in *api.FederationJoinInstructions) { in.AdoptExisting = true },
	} {
		in := base
		mutate(&in)
		assert.Empty(t, joincommand.Build(in))
	}
}
