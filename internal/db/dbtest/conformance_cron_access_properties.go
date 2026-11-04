package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

// Three existing teams give every overlap/disjoint/empty union shape; each
// example replaces all memberships and both independent grant lists. Project
// visibility must intersect those grants even when the cron list permits use.
func checkCronAccessTeamUnionProperty(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	teams := make([]string, 3)
	for i := range teams {
		team, _, err := store.CreateTeam(ctx, fmt.Sprintf("team-%d", i), "owner")
		require.NoError(t, err)
		teams[i] = team.UID
	}
	policy, err := store.CronAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	visibility, err := store.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		membership := hegel.Draw(ht, hegel.Integers(0, 7))
		define := hegel.Draw(ht, hegel.Integers(0, 7))
		observe := hegel.Draw(ht, hegel.Integers(0, 7))
		visible := hegel.Draw(ht, hegel.Integers(0, 7))
		anyDefine := hegel.Draw(ht, hegel.Booleans())
		anyObserve := hegel.Draw(ht, hegel.Booleans())
		allVisible := hegel.Draw(ht, hegel.Booleans())
		selectTeams := func(mask int) []string {
			result := []string{}
			for i, team := range teams {
				if mask&(1<<i) != 0 {
					result = append(result, team)
				}
			}
			return result
		}
		for i, team := range teams {
			_, err := store.SetTeamMembership(ctx, team, "runtime", membership&(1<<i) != 0, "owner")
			require.NoError(ht, err)
		}
		policy.Define = db.CronGrant{Mode: "teams", TeamUIDs: selectTeams(define)}
		if anyDefine {
			policy.Define = db.CronGrant{Mode: "any"}
		}
		policy.Observe = db.CronGrant{Mode: "teams", TeamUIDs: selectTeams(observe)}
		if anyObserve {
			policy.Observe = db.CronGrant{Mode: "any"}
		}
		policy, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
		require.NoError(ht, err)
		visibility.Visibility = "teams"
		visibility.TeamUIDs = selectTeams(visible)
		if allVisible {
			visibility.Visibility = "all"
			visibility.TeamUIDs = nil
		}
		visibility, _, err = store.SetProjectAccessPolicy(ctx, visibility, "owner")
		require.NoError(ht, err)
		got, err := store.CronActorAccess(ctx, project.UID, "runtime")
		require.NoError(ht, err)
		canSee := allVisible || membership&visible != 0
		require.Equal(ht, db.CronAccess{Define: canSee && (anyDefine || membership&define != 0), Observe: canSee && (anyObserve || membership&observe != 0)}, got)
	}, hegel.WithDatabase(t.TempDir()))
	return nil
}
