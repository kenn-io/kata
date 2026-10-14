package main

import (
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// R9: normal commands resolve names through the existing daemon client and
// native team/policy API. Token enrollment stays atomic and credentials redact.
func TestTeamsProjectPolicyCommands(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-test-token"), testenv.WithRequireTokenIdentity())
	out := requireCmdOutput(t, env, "--json", "teams", "create", "engineering")
	var created struct {
		Team db.Team `json:"team"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	require.NotEmpty(t, created.Team.UID)
	out = requireCmdOutput(t, env, "--json", "teams", "list")
	require.Contains(t, out, created.Team.UID)
	requireCmdOutput(t, env, "teams", "members", "add", "engineering", "--actor", "member")
	members, err := env.DB.TeamMembers(t.Context(), created.Team.UID)
	require.NoError(t, err)
	require.Contains(t, members, "member")
	project, err := env.DB.CreateProject(t.Context(), "shared-command-project")
	require.NoError(t, err)
	requireCmdOutput(t, env, "projects", "access", "set", project.Name, "--visibility", "teams", "--team", "engineering")
	policy, err := env.DB.ProjectAccessPolicy(t.Context(), project.UID)
	require.NoError(t, err)
	require.Equal(t, "teams", policy.Visibility)
	require.Equal(t, []string{created.Team.UID}, policy.TeamUIDs)
	out = requireCmdOutput(t, env, "--json", "projects", "access", "show", project.Name)
	require.Contains(t, out, created.Team.UID)
	requireCmdOutput(t, env, "teams", "members", "remove", "engineering", "--actor", "member")
	members, err = env.DB.TeamMembers(t.Context(), created.Team.UID)
	require.NoError(t, err)
	require.NotContains(t, members, "member")
	out = requireCmdOutput(t, env, "tokens", "create", "--actor", "joined-member", "--team", "engineering")
	secret := extractTokenPlaintext(t, out)
	require.NotEmpty(t, secret)
	members, err = env.DB.TeamMembers(t.Context(), created.Team.UID)
	require.NoError(t, err)
	require.Contains(t, members, "joined-member")
	out = requireCmdOutput(t, env, "teams", "show", "engineering")
	require.Contains(t, out, "joined-member")
	require.NotContains(t, out, secret)
}

func TestScopedTokenCreateWithInitialTeam(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-test-token"), testenv.WithRequireTokenIdentity())
	team, _, err := env.DB.CreateTeam(t.Context(), "engineering", db.BootstrapActor)
	require.NoError(t, err)
	project, err := env.DB.CreateProject(t.Context(), "scoped-team-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Scoped work", Author: "owner"})
	require.NoError(t, err)
	path := privateTokenFilePath(t)
	out := requireCmdOutput(t, env, "tokens", "create", "--actor", "scoped-member", "--team", "engineering", "--issue", project.Name+"#"+root.ShortID, "--expires-in", "1h", "--token-file", path)
	//nolint:gosec // The helper creates an owner-only temporary fixture file.
	secret, err := os.ReadFile(path)
	require.NoError(t, err)
	token, err := env.DB.ResolveAPIToken(t.Context(), strings.TrimSpace(string(secret)))
	require.NoError(t, err)
	require.NotNil(t, token.Scope)
	require.Equal(t, root.UID, token.Scope.RootIssueUID)
	members, err := env.DB.TeamMembers(t.Context(), team.UID)
	require.NoError(t, err)
	require.Contains(t, members, "scoped-member")
	require.NotContains(t, out, strings.TrimSpace(string(secret)))
}

// Agent mode keeps the documented contract for the team and project-access
// commands: every success line starts with OK and names the command, an empty
// list still emits its count header, and values stay key=value fields.
func TestTeamsProjectPolicyAgentOutput(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-test-token"), testenv.WithRequireTokenIdentity())
	require.Equal(t, "OK teams count=0\n", requireCmdOutput(t, env, "--agent", "teams", "list"))

	out := requireCmdOutput(t, env, "--agent", "teams", "create", "reviewers")
	require.True(t, strings.HasPrefix(out, "OK teams-create team=reviewers uid="), out)
	teams, err := env.DB.ListTeams(t.Context())
	require.NoError(t, err)
	require.Len(t, teams, 1)
	uid := teams[0].UID

	require.Equal(t, "OK teams count=1\n- team=reviewers uid="+uid+" revision=1\n", requireCmdOutput(t, env, "--agent", "teams", "list"))
	require.Equal(t, "OK teams-members-add team="+uid+" actor=member\n", requireCmdOutput(t, env, "--agent", "teams", "members", "add", "reviewers", "--actor", "member"))
	out = requireCmdOutput(t, env, "--agent", "teams", "show", "reviewers")
	require.Equal(t, "OK teams-show team=reviewers uid="+uid+" revision=2 members=member\n", out)
	require.Equal(t, "OK teams-members-remove team="+uid+" actor=member\n", requireCmdOutput(t, env, "--agent", "teams", "members", "remove", "reviewers", "--actor", "member"))
	out = requireCmdOutput(t, env, "--agent", "teams", "show", "reviewers")
	require.Equal(t, "OK teams-show team=reviewers uid="+uid+" revision=3\n", out, "an empty member list is omitted, never a sentinel")

	project, err := env.DB.CreateProject(t.Context(), "shared-command-project")
	require.NoError(t, err)
	out = requireCmdOutput(t, env, "--agent", "projects", "access", "show", project.Name)
	require.Equal(t, "OK projects-access project=shared-command-project visibility=all revision=1\n", out)
	out = requireCmdOutput(t, env, "--agent", "projects", "access", "set", project.Name, "--visibility", "teams", "--team", "reviewers")
	require.Equal(t, "OK projects-access project=shared-command-project visibility=teams revision=2 teams="+uid+"\n", out)
}
