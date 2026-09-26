package main

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMoveCLI_PreviewRejectsFederationWithoutRefreshingClaims(t *testing.T) {
	for _, role := range []db.FederationRole{db.FederationRoleHub, db.FederationRoleSpoke} {
		for _, sourceBound := range []bool{true, false} {
			t.Run(string(role)+map[bool]string{true: "-source", false: "-target"}[sourceBound], func(t *testing.T) {
				env, dir, source, target, issue := setupMoveCLIProjects(t)
				bound := target
				if sourceBound {
					bound = source
				}
				_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
					ProjectID: bound.ID, Role: role, HubURL: "https://hub.example", HubProjectID: bound.ID, HubProjectUID: bound.UID, Enabled: true, PushEnabled: true, Actor: "tester",
				})
				require.NoError(t, err)
				if role == db.FederationRoleHub && sourceBound {
					_, err = env.DB.AcquireClaim(t.Context(), db.AcquireClaimParams{
						ProjectID: source.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: env.DB.InstanceUID(), Holder: "tester", ClientKind: "cli"},
						ClaimKind: "timed", TTL: time.Minute, Now: time.Now().UTC().Add(-time.Hour),
					})
					require.NoError(t, err)
				}
				cursor, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				out, stderr, err := runCLIWithErr(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name, "--dry-run")
				require.Error(t, err)
				assert.Empty(t, out)
				assert.Contains(t, stderr, "cross-project moves involving a federated project are unsupported")
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				assert.Equal(t, cursor, after, "preview must not refresh claims")
				stored, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				assert.Equal(t, issue, stored)
			})
		}
	}
}

func TestMoveCLI_PreviewRejectsSameProject(t *testing.T) {
	env, dir, source, _, issue := setupMoveCLIProjects(t)
	_, _, err := runCLIWithErr(t, env, dir, "--project", source.Name, "move", issue.ShortID, source.Name, "--dry-run")
	require.Error(t, err)
}

func TestMoveCLI_PreviewOutputAndNoComment(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json", "quiet"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, source, target, issue := setupMoveCLIProjects(t)
			cursor, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			args := []string{"--project", source.Name, "move", issue.ShortID, target.Name, "--dry-run", "--comment", "preview only"}
			if mode != "human" {
				args = append([]string{"--" + mode}, args...)
			}
			out := runCLI(t, env, dir, args...)
			switch mode {
			case "human":
				assert.Contains(t, out, "dry-run: would move "+source.Name+"#"+issue.ShortID)
			case "agent":
				assert.Contains(t, out, "changed=false dry_run=true")
			case "json":
				var preview map[string]any
				require.NoError(t, json.Unmarshal([]byte(out), &preview))
				assert.Equal(t, true, preview["dry_run"])
				assert.Equal(t, false, preview["changed"])
				assert.Equal(t, source.Name+"#"+issue.ShortID, preview["from"])
			case "quiet":
				assert.Empty(t, out)
			}
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			assert.Equal(t, cursor, after)
			comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			assert.Empty(t, comments)
		})
	}
}

func TestMoveCLI_PreviewRejectsRecurrence(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	recurrence, _, err := env.DB.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{
		ProjectID: source.ID, Actor: "tester", Rule: "FREQ=WEEKLY", DTStart: "2026-09-28", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "recurring"},
	})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(), `UPDATE issues SET recurrence_id=?, occurrence_key='2026-09-28' WHERE id=?`, recurrence.ID, issue.ID)
	require.NoError(t, err)
	_, stderr, err := runCLIWithErr(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name, "--dry-run")
	require.Error(t, err)
	assert.Contains(t, stderr, "recurrence")
}
