package federation_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// A7: two personal hubs must deliver simultaneous leaf writes without losing
// source identities, and the root must grant exactly one competing leaf lease.
func TestRelayConcurrentPersonalHubs(t *testing.T) {
	for _, rootBackend := range []string{"sqlite", "postgres"} {
		for _, relayBackend := range []string{"sqlite", "postgres"} {
			for _, leafBackend := range []string{"sqlite", "postgres"} {
				t.Run(fmt.Sprintf("%s_%s_%s", rootBackend, relayBackend, leafBackend), func(t *testing.T) {
					root := newRelayMatrixNode(t, rootBackend, "root-member")
					relays := []*relayMatrixNode{newRelayMatrixNode(t, relayBackend, "first-member"), newRelayMatrixNode(t, relayBackend, "second-member")}
					leaves := []*relayMatrixNode{newRelayMatrixNode(t, leafBackend, "leaf-member"), newRelayMatrixNode(t, leafBackend, "leaf-member")}
					project, err := root.store.CreateProject(t.Context(), "shared-project")
					require.NoError(t, err)
					root.project = project
					_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
					pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
					require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))

					// Both requests must reach the authority before either is dispatched.
					// This makes contention explicit instead of relying on goroutine timing.
					var eventPhase, claimPhase atomic.Bool
					var eventsEntered, claimsEntered atomic.Int32
					eventGate, claimGate := make(chan struct{}), make(chan struct{})
					rootHandler := root.http.Config.Handler
					root.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var gate chan struct{}
						var entered *atomic.Int32
						if eventPhase.Load() && strings.HasSuffix(r.URL.Path, "/federation/relay:accept") {
							gate, entered = eventGate, &eventsEntered
						} else if claimPhase.Load() && strings.HasSuffix(r.URL.Path, "/lease/actions/acquire") {
							gate, entered = claimGate, &claimsEntered
						}
						if gate != nil {
							if entered.Add(1) == 2 {
								close(gate)
							}
							select {
							case <-gate:
							case <-r.Context().Done():
								return
							}
						}
						rootHandler.ServeHTTP(w, r)
					}))
					t.Cleanup(root.http.Close)
					for i := range relays {
						enrollRelayMatrixReplica(t, root, relays[i], fmt.Sprintf("personal-%d", i), true)
						enrollRelayMatrixReplica(t, relays[i], leaves[i], fmt.Sprintf("device-%d", i), false)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
					t.Cleanup(cancel)
					issues, sources := make([]db.Issue, 2), make([]db.Event, 2)
					runRelayConcurrent(t, leaves, func(i int, node *relayMatrixNode) error {
						var err error
						issues[i], sources[i], err = node.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: node.project.ID, Author: fmt.Sprintf("source-agent-%d", i), Title: fmt.Sprintf("Concurrent issue %d", i)})
						return err
					})
					syncNodes := func(_ int, node *relayMatrixNode) error {
						binding, err := node.store.FederationBindingByProject(ctx, node.project.ID)
						if err != nil {
							return err
						}
						return federation.SyncFederationOnce(ctx, node.store, binding, node.credential)
					}
					runRelayConcurrent(t, leaves, syncNodes)
					eventPhase.Store(true)
					runRelayConcurrent(t, relays, syncNodes)
					eventPhase.Store(false)
					require.GreaterOrEqual(t, eventsEntered.Load(), int32(2))
					for range 2 {
						runRelayConcurrent(t, relays, syncNodes)
						runRelayConcurrent(t, leaves, syncNodes)
					}
					for _, node := range append(append([]*relayMatrixNode{root}, relays...), leaves...) {
						for i, issue := range issues {
							mirrored, err := node.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
							require.NoError(t, err)
							require.Equal(t, issue.Author, mirrored.Author)
							require.Equal(t, root.account, mirrored.AccountableActor)
							proof, err := node.store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
							require.NoError(t, err)
							require.NoError(t, db.VerifyRootReceipt(pin, proof))
							require.Equal(t, sources[i].UID, proof.EventUID)
							require.Equal(t, sources[i].ContentHash, proof.ContentHash)
						}
					}

					commentIssueIDs := []int64{issueIDForRelayMatrix(t, leaves[0], issues[0].UID), issueIDForRelayMatrix(t, leaves[1], issues[0].UID)}
					comments, commentSources := make([]db.Comment, 2), make([]db.Event, 2)
					runRelayConcurrent(t, leaves, func(i int, node *relayMatrixNode) error {
						var err error
						comments[i], commentSources[i], err = node.store.CreateComment(ctx, db.CreateCommentParams{IssueID: commentIssueIDs[i], Author: fmt.Sprintf("comment-agent-%d", i), Teammate: fmt.Sprintf("worker-%d", i), Body: fmt.Sprintf("Concurrent relay reply %d", i)})
						return err
					})
					runRelayConcurrent(t, leaves, syncNodes)
					for range 2 {
						runRelayConcurrent(t, relays, syncNodes)
						runRelayConcurrent(t, leaves, syncNodes)
					}
					for _, node := range append(append([]*relayMatrixNode{root}, relays...), leaves...) {
						visible, err := node.store.CommentsByIssue(ctx, issueIDForRelayMatrix(t, node, issues[0].UID))
						require.NoError(t, err)
						require.Len(t, visible, 2)
						for i, comment := range comments {
							found := false
							for _, row := range visible {
								if row.UID == comment.UID {
									found = true
									require.Equal(t, comment.Author, row.Author)
									require.Equal(t, comment.Teammate, row.Teammate)
									require.Equal(t, comment.Body, row.Body)
									require.Equal(t, root.account, row.AccountableActor)
								}
							}
							require.True(t, found)
							proof, err := node.store.EntityAttribution(ctx, project.UID, "comment", comment.UID)
							require.NoError(t, err)
							require.NoError(t, db.VerifyRootReceipt(pin, proof))
							require.Equal(t, root.account, proof.AccountableActor)
							require.Equal(t, comment.Author, proof.SourceActor)
							require.Equal(t, comment.Teammate, proof.Teammate)
							require.Equal(t, commentSources[i].UID, proof.EventUID)
							require.Equal(t, commentSources[i].ContentHash, proof.ContentHash)
						}
					}

					claims := make([]api.ClaimActionResponseBody, 2)
					claimPhase.Store(true)
					runRelayConcurrent(t, leaves, func(i int, node *relayMatrixNode) error {
						path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/lease/actions/acquire", node.http.URL, node.project.ID, issues[0].UID)
						request, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewBufferString(`{"holder":"shared-holder-label","client_kind":"cli","claim_kind":"timed","ttl_seconds":300}`))
						if err != nil {
							return err
						}
						request.Header.Set("Authorization", "Bearer "+node.userToken)
						request.Header.Set("Content-Type", "application/json")
						response, err := node.http.Client().Do(request)
						if err != nil {
							return err
						}
						defer func() { _ = response.Body.Close() }()
						body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
						if err != nil {
							return err
						}
						if response.StatusCode != http.StatusOK {
							return fmt.Errorf("leaf %d claim status %d: %s", i, response.StatusCode, body)
						}
						return json.Unmarshal(body, &claims[i])
					})
					claimPhase.Store(false)
					require.Equal(t, int32(2), claimsEntered.Load())
					require.NotEqual(t, claims[0].Granted, claims[1].Granted, "exactly one leaf wins at the root")
					winner := 0
					if claims[1].Granted {
						winner = 1
					}
					require.NotNil(t, claims[winner].Lease)
					state, err := root.store.ClaimStatusReadOnly(ctx, project.ID, issues[0].UID, time.Now().UTC())
					require.NoError(t, err)
					require.True(t, state.Held)
					require.Equal(t, root.account, state.Claim.Holder)
					require.Equal(t, claims[winner].Lease.ClaimUID, state.Claim.ClaimUID)
					for _, action := range []string{"renew", "release"} {
						status, _ := relayClaimAction(t, leaves[1-winner], issues[0].UID, action, claims[winner].Lease.ClientKind)
						require.Equal(t, http.StatusConflict, status)
					}
					status, released := relayClaimAction(t, leaves[winner], issues[0].UID, "release", "cli")
					require.Equal(t, http.StatusOK, status)
					require.True(t, released.Granted)
					status, acquired := relayClaimAction(t, leaves[1-winner], issues[0].UID, "acquire", "cli")
					require.Equal(t, http.StatusOK, status)
					require.True(t, acquired.Granted)
					require.NotEqual(t, claims[winner].Lease.ClaimUID, acquired.Lease.ClaimUID)
				})
			}
		}
	}
}

func runRelayConcurrent(t *testing.T, nodes []*relayMatrixNode, fn func(int, *relayMatrixNode) error) {
	t.Helper()
	start := make(chan struct{})
	errs := make([]error, len(nodes))
	var workers sync.WaitGroup
	workers.Add(len(nodes))
	for i, node := range nodes {
		go func() {
			defer workers.Done()
			<-start
			errs[i] = fn(i, node)
		}()
	}
	close(start)
	workers.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
}
