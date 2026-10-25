package federation_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/notification"
)

// Use the existing real HTTP matrix nodes and transport, including both peer
// arrival orders at each hop; no special reference or projection fixture.
func TestRelaySharedAccessAcrossHops(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", backend, reverse), func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member")
				hub := newRelayMatrixNode(t, backend, "hub-member")
				leaf := newRelayMatrixNode(t, backend, "leaf-member")
				roots := []*relayMatrixNode{root, root}
				hubs := []*relayMatrixNode{{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials},
					{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials}}
				leaves := []*relayMatrixNode{{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials},
					{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials}}
				var projects [2]db.Project
				var issues [2]db.Issue
				for i, name := range []string{"first-shared", "second-shared"} {
					project, err := root.store.CreateProject(ctx, name)
					require.NoError(t, err)
					projects[i] = project
					_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					root.project = project
					enrollRelayMatrixReplica(t, roots[i], hubs[i], name+"-hub", true)
					syncRelayMatrixNode(t, hubs[i])
					enrollRelayMatrixReplica(t, hubs[i], leaves[i], name+"-leaf", false)
					syncRelayMatrixNode(t, leaves[i])
					title := "Shared endpoint"
					if i == 1 {
						title = issues[0].UID
					}
					issues[i], _, err = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: "assistant", Title: title, Body: title})
					require.NoError(t, err)
				}
				linked, _, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account},
					db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
				order := []int{0, 1}
				if reverse {
					order = []int{1, 0}
				}
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {

					if nodes[0] == leaves[0] {
						upstream, err := hubs[0].store.FederationBindingByProject(ctx, hubs[0].project.ID)
						require.NoError(t, err)
						checkpoint, err := root.store.(db.RelayResetStore).CreateRelayReset(ctx, upstream.RelayConfig.BindingUID, root.signer)
						require.NoError(t, err)
						syncRelayMatrixNode(t, hubs[0])
						// Sync may refresh the root baseline before downloading a reset;
						// compare the forwarded bytes with the checkpoint it actually installed.
						checkpoint, err = root.store.(db.RelayResetStore).CreateRelayReset(ctx, upstream.RelayConfig.BindingUID, root.signer)
						require.NoError(t, err)
						downstream, err := leaves[0].store.FederationBindingByProject(ctx, leaves[0].project.ID)
						require.NoError(t, err)
						policy, err := hub.store.ProjectAccessPolicy(ctx, projects[1].UID)
						require.NoError(t, err)
						policy.Visibility = "teams"
						policy.TeamUIDs = nil
						_, _, err = hub.store.SetProjectAccessPolicy(ctx, policy, "admin")
						require.NoError(t, err)
						_, err = hub.store.(db.RelayResetStore).CreateRelayReset(ctx, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
						require.Error(t, err, "prepared forwarded checkpoint rechecks the child account's peer access")
						policy, err = hub.store.ProjectAccessPolicy(ctx, projects[1].UID)
						require.NoError(t, err)
						policy.Visibility = "all"
						_, _, err = hub.store.SetProjectAccessPolicy(ctx, policy, "admin")
						require.NoError(t, err)
						forwarded, err := hub.store.(db.RelayResetStore).CreateRelayReset(ctx, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
						require.NoError(t, err)
						require.Equal(t, checkpoint.Manifest, forwarded.Manifest)
						require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot, "forwarding preserves exact signed bodies")
					}
					for _, i := range order {
						syncRelayMatrixNode(t, nodes[i])
					}
					local, err := nodes[0].store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					links, err := nodes[0].store.LinksByIssue(ctx, local.ID)
					require.NoError(t, err)
					require.Len(t, links, 1)
					require.Equal(t, issues[1].UID, links[0].ToIssueUID)
					peer, err := nodes[1].store.IssueByUID(ctx, issues[1].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					_, err = nodes[0].store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: local.ID, ToIssueID: peer.ID, Type: "related", Author: nodes[0].account})
					require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
					title := "Replica edit remains local-first"
					_, err = nodes[0].store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: local.ID, Actor: nodes[0].account, Title: &title, AddRelated: []int64{peer.ID}})
					require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
					unchanged, err := nodes[0].store.IssueByID(ctx, local.ID)
					require.NoError(t, err)
					require.Equal(t, local.Title, unchanged.Title)
					require.ErrorIs(t, nodes[0].store.DeleteLinkByID(ctx, links[0].ID), db.ErrFederationCrossProjectBoundary)
				}
				_, err = root.store.DeleteLinkAndEvent(ctx, linked, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {
					for _, i := range order {
						syncRelayMatrixNode(t, nodes[i])
					}
					local, err := nodes[0].store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					links, err := nodes[0].store.LinksByIssue(ctx, local.ID)
					require.NoError(t, err)
					require.Empty(t, links)
				}
			})
		}
	}
}

func TestRelaySharedAccessExistingPair(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "root-member")
			ctx := db.WithRootAttribution(t.Context(), root.signer, root.account)
			var projects [2]db.Project
			var issues [2]db.Issue
			for i, name := range []string{"first-existing", "second-existing"} {
				var err error
				projects[i], err = root.store.CreateProject(ctx, name)
				require.NoError(t, err)
				issues[i], _, err = root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[i].ID, Author: root.account, Title: "Existing endpoint"})
				require.NoError(t, err)
			}
			_, _, err := root.store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "related", Author: root.account}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
			require.NoError(t, err)
			for _, project := range projects {
				_, err := root.store.EnableProjectFederation(ctx, project.ID, root.account)
				require.NoError(t, err)
			}
			assertRelayProjectPreview(t, root, projects[0])
			root.project = projects[0]
			hub := newRelayMatrixNode(t, backend, "hub-member")
			enrollRelayMatrixReplica(t, root, hub, "existing-hub", true)
		})
	}
}

func TestRelaySharedAccessReplyPurge(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, form := range []string{"reply", "notification", "notification-before-enrollment"} {
			t.Run(backend+"/"+form, func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member")
				leaf := newRelayMatrixNode(t, backend, "leaf-member")
				project, err := root.store.CreateProject(ctx, "reply-project")
				require.NoError(t, err)
				root.project = project
				attr := db.WithRootAttribution(ctx, root.signer, root.account)
				_, err = root.store.EnableProjectFederation(attr, project.ID, root.account)
				require.NoError(t, err)
				if form != "notification-before-enrollment" {
					enrollRelayMatrixReplica(t, root, leaf, "reply-leaf", false)
					syncRelayMatrixNode(t, leaf)
				}
				source, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: "Reply survives"})
				require.NoError(t, err)
				target, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: "Reply target"})
				require.NoError(t, err)
				comment, _, err := root.store.CreateComment(attr, db.CreateCommentParams{IssueID: target.ID, Author: root.account, Body: "Original"})
				require.NoError(t, err)
				var reply db.Comment
				if form == "reply" {
					reply, _, err = root.store.CreateComment(attr, db.CreateCommentParams{IssueID: source.ID, Author: root.account, Body: "Retained reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
				} else {
					slot, marshalErr := json.Marshal(notification.Value{From: root.account, Message: "Inspect finding", Re: comment.UID})
					require.NoError(t, marshalErr)
					_, err = root.store.PatchIssueMetadata(attr, db.PatchIssueMetadataIn{IssueID: source.ID, Actor: root.account, Patch: map[string]jsontext.Value{notification.MetadataKey(root.account): slot}})
				}
				require.NoError(t, err)
				if form != "notification-before-enrollment" {
					syncRelayMatrixNode(t, leaf)
				}
				_, err = root.store.PurgeIssue(attr, target.ID, root.account, nil)
				require.NoError(t, err)
				if form == "notification-before-enrollment" {
					assertRelayProjectPreview(t, root, project)
					enrollRelayMatrixReplica(t, root, leaf, "reply-leaf", false)
				}
				if form != "reply" {
					// Already-purged notification ownership is unprovable. Refuse
					// the whole history, even before the first relay release.
					before, err := root.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					cursor, err := root.store.MaxEventID(ctx)
					require.NoError(t, err)
					var previous []byte
					for attempt := range 2 {
						status, raw := relayMatrixRequest(t, root, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", project.ID), leaf.credential.Token, nil)
						require.Equal(t, http.StatusForbidden, status, string(raw))
						var refusal struct {
							Error struct {
								Code string `json:"code"`
							} `json:"error"`
						}
						require.NoError(t, json.Unmarshal(raw, &refusal))
						require.Equal(t, "auth_invalid", refusal.Error.Code)
						for _, hidden := range []string{source.UID, target.UID, comment.UID, "Inspect finding", "snapshot_uid", "signature"} {
							require.NotContains(t, string(raw), hidden)
						}
						if attempt > 0 {
							require.Equal(t, previous, raw, "retry consistently refuses without disclosing a checkpoint")
						}
						previous = raw
					}
					after, err := root.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					require.Equal(t, before, after, "refusal preserves source metadata and revision")
					afterCursor, err := root.store.MaxEventID(ctx)
					require.NoError(t, err)
					require.Equal(t, cursor, afterCursor, "denied checkpoint capture rolls back its source events")
					binding, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
					require.NoError(t, err)
					localBefore, localErr := leaf.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
					require.True(t, localErr == nil || errors.Is(localErr, db.ErrNotFound))
					require.ErrorContains(t, federation.SyncFederationOnce(ctx, leaf.store, binding, leaf.credential), "returned 403")
					current, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
					require.NoError(t, err)
					require.Equal(t, binding.PullCursorEventID, current.PullCursorEventID)
					require.Equal(t, binding.RelayConfig.ResetEpoch, current.RelayConfig.ResetEpoch)
					localAfter, err := leaf.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
					if localErr == nil {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, db.ErrNotFound)
					}
					require.Equal(t, localBefore, localAfter, "failed sync leaves existing replica data unchanged")
					return
				}
				// A surviving reply proves its purged target's ownership.
				syncRelayMatrixNode(t, leaf)
				local, err := leaf.store.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, source.Title, local.Title)
				comments, err := leaf.store.CommentsByIssue(ctx, local.ID)
				require.NoError(t, err)
				require.Len(t, comments, 1)
				require.Equal(t, reply.UID, comments[0].UID)
				require.Equal(t, reply.Body, comments[0].Body)
				require.Equal(t, reply.ReplyToUID, comments[0].ReplyToUID)
				require.Equal(t, reply.ReplyKind, comments[0].ReplyKind)
			})
		}
	}
}

func assertRelayProjectPreview(t *testing.T, root *relayMatrixNode, project db.Project) {
	t.Helper()
	status, raw := relayMatrixRequest(t, root, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation?relay_protocol_version=1", project.ID), root.userToken, nil)
	assert.Equal(t, http.StatusOK, status, string(raw))
}

// Ordinary scalar edits and close do not author crossing relationships. A
// fresh pair keeps the close independent of access-loss and purge scenarios.
func TestRelaySharedAccessOrdinaryClose(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "root-member")
			hub := newRelayMatrixNode(t, backend, "hub-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			ctx := t.Context()
			attr := db.WithRootAttribution(ctx, root.signer, root.account)
			var projects [2]db.Project
			var issues [2]db.Issue
			var hubs, leaves [2]*relayMatrixNode
			for i, name := range []string{"close-first", "close-second"} {
				var err error
				projects[i], err = root.store.CreateProject(ctx, name)
				require.NoError(t, err)
				_, err = root.store.EnableProjectFederation(attr, projects[i].ID, root.account)
				require.NoError(t, err)
				issues[i], _, err = root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[i].ID, Author: root.account, Title: "Shared endpoint"})
				require.NoError(t, err)
			}
			for _, kind := range []string{"parent", "blocks", "related"} {
				_, _, err := root.store.CreateLinkAndEvent(attr, db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: kind, Author: root.account}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
			}
			for i := range projects {
				root.project = projects[i]
				hubs[i] = &relayMatrixNode{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials}
				leaves[i] = &relayMatrixNode{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials}
				enrollRelayMatrixReplica(t, root, hubs[i], projects[i].Name+"-hub", true)
			}
			for _, node := range hubs {
				syncRelayMatrixNode(t, node)
			}
			for i := range projects {
				enrollRelayMatrixReplica(t, hubs[i], leaves[i], projects[i].Name+"-leaf", false)
			}
			for _, node := range leaves {
				syncRelayMatrixNode(t, node)
			}
			local, err := leaf.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			links, err := leaf.store.LinksByIssue(ctx, local.ID)
			require.NoError(t, err)
			require.Len(t, links, 3)
			title := "Edited at leaf"
			_, err = leaf.store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: local.ID, Actor: leaf.account, Title: &title})
			require.NoError(t, err)
			_, _, _, err = leaf.store.CloseIssueWithEvents(ctx, local.ID, "done", leaf.account, "Completed ordinary work", nil)
			require.NoError(t, err, "closing is ordinary work, not a crossing relationship mutation")
			for _, node := range []*relayMatrixNode{leaves[0], hubs[0], leaves[0]} {
				syncRelayMatrixNode(t, node)
			}
			atRoot, err := root.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "closed", atRoot.Status)
			require.Equal(t, title, atRoot.Title)
			links, err = leaf.store.LinksByIssue(ctx, local.ID)
			require.NoError(t, err)
			require.Len(t, links, 3, "ordinary edit preserves the root-authored relationships")
		})
	}
}

// Normal project-name reuse must not break a current, same-project close.
// Both evidence spellings are accepted by the registered public close API.
func TestRelaySharedAccessQualifiedEvidenceNameReuse(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, qualified := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/qualified=%v", backend, qualified), func(t *testing.T) {
				root := newRelayMatrixNode(t, backend, "root-member")
				leaf := newRelayMatrixNode(t, backend, "leaf-member")
				ctx := t.Context()
				attr := db.WithRootAttribution(ctx, root.signer, root.account)
				request := func(method, path string, body any) []byte {
					status, raw := relayMatrixRequest(t, root, method, path, root.userToken, body)
					require.Equal(t, http.StatusOK, status, "%s %s: %s", method, path, raw)
					return raw
				}
				// Keep ordinary native history for a different project which
				// previously used the current shared project's name.
				historical, err := root.store.CreateProject(ctx, "source-root")
				require.NoError(t, err)
				_, _, err = root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: historical.ID, Author: root.account, Title: "Historical issue"})
				require.NoError(t, err)
				request(http.MethodPatch, fmt.Sprintf("/api/v1/projects/%d", historical.ID), map[string]any{"name": "retired-root", "actor": root.account})
				var projects [2]db.Project
				var issues [2]db.Issue
				var replicas [2]*relayMatrixNode
				for i, name := range []string{"source-root", "target-root"} {
					projects[i], err = root.store.CreateProject(ctx, name)
					require.NoError(t, err)
					_, err = root.store.EnableProjectFederation(attr, projects[i].ID, root.account)
					require.NoError(t, err)
					issues[i], _, err = root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[i].ID, Author: root.account, Title: "Shared endpoint"})
					require.NoError(t, err)
				}
				target, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[0].ID, Author: root.account, Title: "Same-project replacement"})
				require.NoError(t, err)
				for _, kind := range []string{"parent", "blocks"} {
					_, _, err = root.store.CreateLinkAndEvent(attr,
						db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: kind, Author: root.account},
						db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
					require.NoError(t, err)
				}
				for i, name := range []string{"source-local", "target-local"} {
					root.project = projects[i]
					replicas[i] = &relayMatrixNode{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials}
					enrollRelayMatrixReplica(t, root, replicas[i], name, false)
				}
				for _, i := range []int{1, 0} {
					syncRelayMatrixNode(t, replicas[i])
				}
				local, err := leaf.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				links, err := leaf.store.LinksByIssue(ctx, local.ID)
				require.NoError(t, err)
				require.Len(t, links, 2)
				ref := target.UID
				if qualified {
					ref = projects[0].Name + "#" + target.ShortID
				}
				path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", projects[0].ID, issues[0].ShortID)
				lease := request(http.MethodPost, path+"/lease/actions/acquire", map[string]any{"holder": root.account, "actor": root.account, "purpose": "Complete ordinary issue work"})
				var granted struct {
					Granted bool `json:"granted"`
				}
				require.NoError(t, json.Unmarshal(lease, &granted))
				require.True(t, granted.Granted, string(lease))
				closedBody := request(http.MethodPost, path+"/actions/close", map[string]any{
					"actor": root.account, "reason": "superseded", "message": "Completed this work in the existing same-project replacement issue.",
					"evidence": []map[string]any{{"type": "superseded-by", "issue_ref": ref}},
				})
				atRoot, err := root.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "closed", atRoot.Status, "registered close API accepted the valid same-project target")
				var closed struct {
					Event db.Event `json:"event"`
				}
				require.NoError(t, json.Unmarshal(closedBody, &closed))
				require.NotEmpty(t, closed.Event.UID)
				require.Contains(t, closed.Event.Payload, ref, "native close keeps the submitted evidence spelling")
				binding, err := leaf.store.FederationBindingByProject(ctx, replicas[0].project.ID)
				require.NoError(t, err)
				require.NoError(t, federation.SyncFederationOnce(ctx, leaf.store, binding, replicas[0].credential),
					"an accepted same-project ordinary close must remain deliverable after project-name reuse")
				local, err = leaf.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "closed", local.Status)
				links, err = leaf.store.LinksByIssue(ctx, local.ID)
				require.NoError(t, err)
				require.Len(t, links, 2, "ordinary close preserves root-authored crossing relationships")
				copied, err := leaf.store.EventsByUIDs(ctx, replicas[0].project.ID, []string{closed.Event.UID})
				require.NoError(t, err)
				require.Len(t, copied, 1)
				require.Equal(t, closed.Event.Payload, copied[0].Payload, "serialized source evidence is unchanged")
				require.Equal(t, closed.Event.ContentHash, copied[0].ContentHash, "native source hash is unchanged")
			})
		}
	}
}

// Public parent replacement emits the same portable unlink contract as an
// ordinary delete, so its committed pair must converge through both hops.
func TestRelaySharedAccessParentReplacement(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "root-member")
			hub := newRelayMatrixNode(t, backend, "hub-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			ctx := t.Context()
			attr := db.WithRootAttribution(ctx, root.signer, root.account)
			var projects [2]db.Project
			var issues [2]db.Issue
			var hubs, leaves [2]*relayMatrixNode
			for i, name := range []string{"replace-first", "replace-second"} {
				var err error
				projects[i], err = root.store.CreateProject(ctx, name)
				require.NoError(t, err)
				_, err = root.store.EnableProjectFederation(attr, projects[i].ID, root.account)
				require.NoError(t, err)
				issues[i], _, err = root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[i].ID, Author: root.account, Title: "Parent endpoint"})
				require.NoError(t, err)
				root.project = projects[i]
				hubs[i] = &relayMatrixNode{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials}
				leaves[i] = &relayMatrixNode{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials}
				enrollRelayMatrixReplica(t, root, hubs[i], name+"-hub", true)
				syncRelayMatrixNode(t, hubs[i])
				enrollRelayMatrixReplica(t, hubs[i], leaves[i], name+"-leaf", false)
				syncRelayMatrixNode(t, leaves[i])
			}
			replacement, _, err := root.store.CreateIssue(attr, db.CreateIssueParams{ProjectID: projects[0].ID, Author: root.account, Title: "Replacement parent"})
			require.NoError(t, err)
			request := func(path string, body any) {
				status, raw := relayMatrixRequest(t, root, http.MethodPost, path, root.userToken, body)
				require.Equal(t, http.StatusOK, status, string(raw))
			}
			for _, issue := range []db.Issue{issues[0], issues[1], replacement} {
				request(fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease/actions/acquire", issue.ProjectID, issue.ShortID), map[string]any{"holder": root.account, "actor": root.account, "purpose": "Replace the shared parent"})
			}
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/links", projects[0].ID, issues[0].ShortID)
			request(path, map[string]any{"actor": root.account, "type": "parent", "to_ref": issues[1].UID})
			for _, node := range []*relayMatrixNode{hubs[0], hubs[1], leaves[1], leaves[0]} {
				syncRelayMatrixNode(t, node)
			}
			local, err := leaf.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			parent, err := leaf.store.ParentOf(ctx, local.ID)
			require.NoError(t, err)
			require.Equal(t, issues[1].UID, parent.ToIssueUID)
			before, err := root.store.MaxEventID(ctx)
			require.NoError(t, err)
			request(path, map[string]any{"actor": root.account, "type": "parent", "to_ref": replacement.UID, "replace": true})
			emitted, err := root.store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projects[0].ID, AfterID: before, Limit: 100})
			require.NoError(t, err)
			require.Len(t, emitted, 2, "atomic replacement emits its original unlink and link")
			require.Equal(t, "issue.unlinked", emitted[0].Type)
			require.Equal(t, "issue.linked", emitted[1].Type)
			for _, node := range []*relayMatrixNode{hubs[0], hubs[1], leaves[1], leaves[0]} {
				syncRelayMatrixNode(t, node)
			}
			parent, err = leaf.store.ParentOf(ctx, local.ID)
			require.NoError(t, err)
			require.Equal(t, replacement.UID, parent.ToIssueUID)
			links, err := leaf.store.LinksByIssue(ctx, local.ID)
			require.NoError(t, err)
			require.Len(t, links, 1, "the old crossing parent is removed")
			for _, original := range emitted {
				copied, err := leaf.store.EventsByUIDs(ctx, leaves[0].project.ID, []string{original.UID})
				require.NoError(t, err)
				require.Len(t, copied, 1)
				require.Equal(t, original.Payload, copied[0].Payload, "emitted payload is never rewritten by transport")
				require.Equal(t, original.ContentHash, copied[0].ContentHash)
			}
		})
	}
}

// Issue notification conventions do not interpret opaque project metadata.
func TestRelaySharedAccessProjectMetadata(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "root-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			ctx := t.Context()
			project, err := root.store.CreateProject(ctx, "metadata-project")
			require.NoError(t, err)
			root.project = project
			attr := db.WithRootAttribution(ctx, root.signer, root.account)
			_, err = root.store.EnableProjectFederation(attr, project.ID, root.account)
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, leaf, "metadata-replica", false)
			syncRelayMatrixNode(t, leaf)
			status, raw := relayMatrixRequest(t, root, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/metadata", project.ID), root.userToken, map[string]any{
				"actor": root.account, "patch": map[string]any{"notify.project_ui": map[string]any{
					"from": "project-consumer", "message": "Opaque project configuration", "re": "consumer-record-7",
				}},
			})
			require.Equal(t, http.StatusOK, status, string(raw))
			committed, err := root.store.ProjectByID(ctx, project.ID)
			require.NoError(t, err)
			require.Contains(t, string(committed.Metadata), "consumer-record-7")
			syncRelayMatrixNode(t, leaf)
			replicated, err := leaf.store.ProjectByID(ctx, leaf.project.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(committed.Metadata), string(replicated.Metadata))
			binding, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
			require.NoError(t, err)
			_, err = root.store.(db.RelayResetStore).CreateRelayReset(ctx, binding.RelayConfig.BindingUID, root.signer)
			require.NoError(t, err, "selected project reset metadata is opaque too")
			syncRelayMatrixNode(t, leaf)
			replicated, err = leaf.store.ProjectByID(ctx, leaf.project.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(committed.Metadata), string(replicated.Metadata))
		})
	}
}
