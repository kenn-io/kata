package federation_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// Existing edges remain removable at their root after an accessible peer is archived.
// Additions, replica writes and relay disclosure retain their stricter boundaries.
func TestRelaySharedAccessArchivedPeerCleanup(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			replica := newRelayMatrixNode(t, backend, "replica-member")
			attr := db.WithRootAttribution(ctx, root.signer, root.account)
			team, _, err := root.store.CreateTeam(ctx, "shared-audience", "admin")
			require.NoError(t, err)
			_, err = root.store.SetTeamMembership(ctx, team.UID, root.account, true, "admin")
			require.NoError(t, err)
			var projects [2]db.Project
			var mirrors [2]*relayMatrixNode
			for i, name := range []string{"active-project", "peer-project"} {
				projects[i], err = root.store.CreateProject(ctx, name)
				require.NoError(t, err)
				policy, err := root.store.ProjectAccessPolicy(ctx, projects[i].UID)
				require.NoError(t, err)
				policy.Visibility, policy.TeamUIDs = "teams", []string{team.UID}
				_, _, err = root.store.SetProjectAccessPolicy(ctx, policy, "admin")
				require.NoError(t, err)
				_, err = root.store.EnableProjectFederation(attr, projects[i].ID, root.account)
				require.NoError(t, err)
				root.project = projects[i]
				mirrors[i] = &relayMatrixNode{store: replica.store, http: replica.http, signer: replica.signer, userToken: replica.userToken, account: replica.account, credentials: replica.credentials}
				enrollRelayMatrixReplica(t, root, mirrors[i], name+"-replica", false)
				syncRelayMatrixNode(t, mirrors[i])
			}
			peer, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[1].ID, Author: root.account, Title: "Archived endpoint"})
			require.NoError(t, err)
			replacement, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[0].ID, Author: root.account, Title: "Active replacement parent"})
			require.NoError(t, err)
			var issues [3]db.Issue
			var links [3]db.Link
			kinds := []string{"related", "blocks", "parent"}
			linkPath := func(issue db.Issue) string {
				return fmt.Sprintf("/api/v1/projects/%d/issues/%s/links", issue.ProjectID, issue.UID)
			}
			for i, kind := range kinds {
				issues[i], _, err = root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[0].ID, Author: root.account, Title: kind + " source"})
				require.NoError(t, err)
			}
			for _, issue := range append(append([]db.Issue{}, issues[:]...), peer, replacement) {
				status, raw := relayMatrixRequest(t, root, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease/actions/acquire", issue.ProjectID, issue.UID), root.userToken, map[string]any{"holder": root.account, "actor": root.account, "purpose": "Clean up an archived shared peer"})
				require.Equal(t, http.StatusOK, status, string(raw))
				var acquired api.ClaimActionResponseBody
				require.NoError(t, json.Unmarshal(raw, &acquired))
				require.True(t, acquired.Granted, string(raw))
			}
			for i, kind := range kinds {
				status, raw := relayMatrixRequest(t, root, http.MethodPost, linkPath(issues[i]), root.userToken, map[string]any{"actor": root.account, "type": kind, "to_ref": peer.UID})
				require.Equal(t, http.StatusOK, status, string(raw))
				var response api.CreateLinkResponse
				require.NoError(t, json.Unmarshal(raw, &response.Body))
				require.NotNil(t, response.Body.Event)
				require.Equal(t, root.store.InstanceUID(), response.Body.Event.OriginInstanceUID, "native HTTP source is authored at the actual root")
				links[i], err = root.store.LinkByID(ctx, response.Body.Link.ID)
				require.NoError(t, err)
			}
			// Populate genuine replica edges before archiving the root's peer.
			for _, mirror := range []*relayMatrixNode{mirrors[1], mirrors[0]} {
				syncRelayMatrixNode(t, mirror)
			}
			binding, err := replica.store.FederationBindingByProject(ctx, mirrors[0].project.ID)
			require.NoError(t, err)
			checkpoint, err := root.store.(db.RelayResetStore).CreateRelayReset(ctx, binding.RelayConfig.BindingUID, root.signer)
			require.NoError(t, err)
			require.NotEmpty(t, checkpoint.Snapshot.Entities)
			// This is the supported administrative archive route and real user token.
			// RemoveProjectRequest requires actor/force query, no If-Match/revision.
			status, raw := relayMatrixRequest(t, root, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%d?actor=%s&force=true", projects[1].ID, root.account), root.userToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			archived, err := root.store.ProjectByID(ctx, projects[1].ID)
			require.NoError(t, err)
			require.NotNil(t, archived.DeletedAt)
			accessible, err := root.store.AccessibleProjectUIDs(ctx, root.account)
			require.NoError(t, err)
			for _, project := range projects {
				require.Contains(t, accessible, project.UID, "current native access remains positive even for archived rows")
				binding, err := root.store.FederationBindingByProject(ctx, project.ID)
				require.NoError(t, err)
				require.True(t, binding.Enabled)
				require.Equal(t, db.FederationRoleHub, binding.Role)
			}
			assertUnchanged := func(t *testing.T, node *relayMatrixNode, before int64, expected db.Link) {
				t.Helper()
				current, err := node.store.LinkByID(ctx, expected.ID)
				require.NoError(t, err)
				require.Equal(t, expected, current)
				events, err := node.store.EventsAfter(ctx, db.EventsAfterParams{AfterID: before, Limit: 100})
				require.NoError(t, err)
				require.Empty(t, events, "refused operation emits no events")
			}
			t.Run("replica-removals-refused", func(t *testing.T) {
				for i, source := range issues {
					local, err := replica.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					rows, err := replica.store.LinksByIssue(ctx, local.ID)
					require.NoError(t, err)
					require.Len(t, rows, 1)
					before, err := replica.store.MaxEventID(ctx)
					require.NoError(t, err)
					_, err = replica.store.DeleteLinkAndEvent(ctx, rows[0], db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: local.ID, Actor: replica.account, FromUID: local.UID, ToUID: peer.UID, FromShortID: local.ShortID, ToShortID: peer.ShortID})
					require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary, kinds[i])
					assertUnchanged(t, replica, before, rows[0])
				}
			})
			t.Run("replica-parent-replace-refused", func(t *testing.T) {
				child, err := replica.store.IssueByUID(ctx, issues[2].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				old, err := replica.store.ParentOf(ctx, child.ID)
				require.NoError(t, err)
				active, err := replica.store.IssueByUID(ctx, replacement.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				before, err := replica.store.MaxEventID(ctx)
				require.NoError(t, err)
				_, err = replica.store.(db.ParentLinkReplacementStorage).ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
					ExpectedParentLinkID: old.ID, ExpectedParentIssueID: old.ToIssueID,
					Link:        db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: active.ID, Type: "parent", Author: replica.account},
					UnlinkEvent: db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: child.ID, Actor: replica.account, FromUID: child.UID, ToUID: peer.UID, FromShortID: child.ShortID, ToShortID: peer.ShortID},
					LinkEvent:   db.LinkEventParams{EventType: "issue.linked", EventIssueID: child.ID, Actor: replica.account, FromUID: child.UID, ToUID: active.UID, FromShortID: child.ShortID, ToShortID: active.ShortID},
				})
				require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
				assertUnchanged(t, replica, before, old)
			})
			refusedNativeMutations := func(t *testing.T, actor string) {
				t.Helper()
				access := db.WithProjectAccessTransactionCheck(ctx, func(ctx context.Context, tx db.Transaction, projectUID string) error {
					return root.store.ProjectAccessTransactionFence(actor, []string{projectUID})(ctx, tx)
				})
				for i, source := range issues {
					before, err := root.store.MaxEventID(ctx)
					require.NoError(t, err)
					unlink := db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: source.ID, Actor: actor, FromUID: source.UID, ToUID: peer.UID, FromShortID: source.ShortID, ToShortID: peer.ShortID}
					if kinds[i] == "parent" {
						_, err = root.store.(db.ParentLinkReplacementStorage).ReplaceParentAndEvents(access, db.ReplaceParentAndEventsParams{
							ExpectedParentLinkID: links[i].ID, ExpectedParentIssueID: peer.ID,
							Link:        db.CreateLinkParams{FromIssueID: source.ID, ToIssueID: replacement.ID, Type: "parent", Author: actor},
							UnlinkEvent: unlink,
							LinkEvent:   db.LinkEventParams{EventType: "issue.linked", EventIssueID: source.ID, Actor: actor, FromUID: source.UID, ToUID: replacement.UID, FromShortID: source.ShortID, ToShortID: replacement.ShortID},
						})
					} else {
						_, err = root.store.DeleteLinkAndEvent(access, links[i], unlink)
					}
					require.ErrorIs(t, err, db.ErrNotFound, kinds[i]+" keeps the native current-access fence")
					assertUnchanged(t, root, before, links[i])
				}
			}
			t.Run("unprivileged-actor-refused", func(t *testing.T) {
				limited, _, err := root.store.CreateTeam(ctx, "active-only", "admin")
				require.NoError(t, err)
				_, _, err = root.store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "limited-member", AdminActor: "admin", TeamUIDs: []string{limited.UID}, PlaintextToken: "limited-member-test-token"})
				require.NoError(t, err)
				policy, err := root.store.ProjectAccessPolicy(ctx, projects[0].UID)
				require.NoError(t, err)
				policy.TeamUIDs = append(policy.TeamUIDs, limited.UID)
				_, _, err = root.store.SetProjectAccessPolicy(ctx, policy, "admin")
				require.NoError(t, err)
				accessible, err := root.store.AccessibleProjectUIDs(ctx, "limited-member")
				require.NoError(t, err)
				require.Contains(t, accessible, projects[0].UID)
				require.NotContains(t, accessible, projects[1].UID)
				refusedNativeMutations(t, "limited-member")
			})
			t.Run("archived-additions-refused", func(t *testing.T) {
				before, err := root.store.MaxEventID(ctx)
				require.NoError(t, err)
				status, raw := relayMatrixRequest(t, root, http.MethodPost, linkPath(replacement), root.userToken, map[string]any{"actor": root.account, "type": "related", "to_ref": peer.UID})
				require.Equal(t, http.StatusConflict, status, string(raw))
				var envelope api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &envelope))
				require.Equal(t, "link_target_archived", envelope.Error.Code)
				events, err := root.store.EventsAfter(ctx, db.EventsAfterParams{AfterID: before, Limit: 100})
				require.NoError(t, err)
				require.Empty(t, events)
				rows, err := root.store.LinksByIssue(ctx, replacement.ID)
				require.NoError(t, err)
				require.Empty(t, rows)
			})
			t.Run("cached-disclosure-stays-refused", func(t *testing.T) {
				_, err := root.store.(db.RelayResetStore).CreateRelayReset(ctx, binding.RelayConfig.BindingUID, root.signer)
				require.ErrorIs(t, err, db.ErrNotFound, "archived peer still fences relay disclosure")
			})
			for i, kind := range kinds {
				t.Run("root-cleanup-"+kind, func(t *testing.T) {
					before, err := root.store.MaxEventID(ctx)
					require.NoError(t, err)
					method, path, body := http.MethodDelete, fmt.Sprintf("%s/%d?actor=%s", linkPath(issues[i]), links[i].ID, root.account), any(nil)
					if kind == "parent" {
						method, path, body = http.MethodPost, linkPath(issues[i]), map[string]any{"actor": root.account, "type": "parent", "to_ref": replacement.UID, "replace": true}
					}
					status, raw := relayMatrixRequest(t, root, method, path, root.userToken, body)
					require.Equal(t, http.StatusOK, status, string(raw))
					_, err = root.store.LinkByID(ctx, links[i].ID)
					require.ErrorIs(t, err, db.ErrNotFound)
					emitted, err := root.store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projects[0].ID, AfterID: before, Limit: 100})
					require.NoError(t, err)
					if kind == "parent" {
						require.Len(t, emitted, 2)
						require.Equal(t, "issue.unlinked", emitted[0].Type)
						require.Equal(t, "issue.linked", emitted[1].Type)
						parent, err := root.store.ParentOf(ctx, issues[i].ID)
						require.NoError(t, err)
						require.Equal(t, replacement.UID, parent.ToIssueUID)
					} else {
						require.Len(t, emitted, 1)
						require.Equal(t, "issue.unlinked", emitted[0].Type)
					}
				})
			}
		})
	}
}

func TestRelaySharedAccessPurgedBareEvidence(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			project, err := root.store.CreateProject(ctx, "closed-project")
			require.NoError(t, err)
			root.project = project
			attr := db.WithRootAttribution(ctx, root.signer, root.account)
			_, err = root.store.EnableProjectFederation(attr, project.ID, root.account)
			require.NoError(t, err)
			source, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: "Duplicate source"})
			require.NoError(t, err)
			target, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: "Original issue"})
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", project.ID, source.UID)
			status, raw := relayMatrixRequest(t, root, http.MethodPost, path+"/lease/actions/acquire", root.userToken, map[string]any{"holder": root.account, "actor": root.account, "purpose": "Close the duplicate issue"})
			require.Equal(t, http.StatusOK, status, string(raw))
			var acquired api.ClaimActionResponseBody
			require.NoError(t, json.Unmarshal(raw, &acquired))
			require.True(t, acquired.Granted, string(raw))
			before, err := root.store.MaxEventID(ctx)
			require.NoError(t, err)
			status, raw = relayMatrixRequest(t, root, http.MethodPost, path+"/actions/close", root.userToken, map[string]any{
				"actor": root.account, "reason": "duplicate", "message": "This work is already represented by the existing original issue.",
				"evidence": []map[string]any{{"type": "duplicate-of", "issue_ref": target.ShortID}},
			})
			require.Equal(t, http.StatusOK, status, string(raw))
			events, err := root.store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, AfterID: before, Limit: 100})
			require.NoError(t, err)
			var closed db.Event
			closedCount := 0
			for _, event := range events {
				if event.Type == "issue.closed" {
					closed = event
					closedCount++
				}
			}
			require.Equal(t, 1, closedCount)
			_, err = root.store.PurgeIssue(attr, target.ID, root.account, nil)
			require.NoError(t, err)
			hub := newRelayMatrixNode(t, backend, "hub-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			enrollRelayMatrixReplica(t, root, hub, "closed-hub", true)
			syncRelayMatrixNode(t, hub)
			local, err := hub.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "closed", local.Status)
			require.Equal(t, "verified", local.Verification)
			_, err = hub.store.IssueByUID(ctx, target.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
			copied, err := hub.store.EventsByUIDs(ctx, hub.project.ID, []string{closed.UID})
			require.NoError(t, err)
			require.Len(t, copied, 1)
			require.Equal(t, closed.Payload, copied[0].Payload)
			require.Equal(t, closed.ContentHash, copied[0].ContentHash)
			enrollRelayMatrixReplica(t, hub, leaf, "closed-leaf", false)
			downstream, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
			require.NoError(t, err)
			cursor, err := hub.store.MaxEventID(ctx)
			require.NoError(t, err)
			for range 2 {
				withheld, err := hub.store.(db.RelayResetStore).CreateRelayReset(ctx, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
				require.ErrorIs(t, err, db.ErrNotFound, "the fresh hub lacks concrete ownership for the old bare ref")
				require.Equal(t, db.RelayResetCheckpoint{}, withheld)
			}
			status, refusal := relayMatrixRequest(t, hub, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", hub.project.ID), leaf.credential.Token, nil)
			require.Equal(t, http.StatusForbidden, status, string(refusal))
			for _, hidden := range []string{source.UID, target.UID, closed.UID, closed.ContentHash, "snapshot_uid", "signature"} {
				require.NotContains(t, string(refusal), hidden)
			}
			after, err := hub.store.IssueByID(ctx, local.ID)
			require.NoError(t, err)
			require.Equal(t, local, after)
			afterCursor, err := hub.store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, afterCursor)
		})
	}
}
