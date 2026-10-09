package federation_test

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
)

type artifactWire struct {
	method            string
	request, response []byte
	digests           []string
}

// R7/A6: a lost reply after committing complete misses resumes with a manifest
// hit; revocation between an emitted offer and download discloses no vectors.
func TestArtifactManifestRetryAndRevocation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"lost_push_reply", "revoke_before_download", "self_revoke_before_download"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member")
				personal := newRelayMatrixNode(t, backend, "relay-member")
				project, err := root.store.CreateProject(ctx, "artifact-fault-project")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				var armed atomic.Bool
				var uploads atomic.Int32
				var capture artifactWireCapture
				handler := root.http.Config.Handler
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					r.Body = io.NopCloser(bytes.NewReader(raw))
					var batch db.RelayBatch
					_ = json.Unmarshal(decodeArtifactWire(t, raw, r.Header.Get("Content-Encoding")), &batch)
					if batch.Stream == db.RelayStreamArtifact && len(batch.Artifacts) > 0 {
						uploads.Add(1)
					}
					if (mode == "revoke_before_download" || mode == "self_revoke_before_download") && len(r.URL.Query()["artifact_digest"]) > 0 && armed.Swap(false) {
						grants, err := root.store.ListFederationEnrollments(ctx)
						require.NoError(t, err)
						require.Len(t, grants, 1)
						if mode == "self_revoke_before_download" {
							revoker, ok := root.store.(db.RelaySelfRevoker)
							require.True(t, ok)
							require.NoError(t, revoker.RevokeOwnRelayEnrollment(ctx, personal.credential.Token, project.ID, personal.store.InstanceUID()))
						} else {
							require.NoError(t, root.store.RevokeFederationEnrollment(ctx, grants[0].ID))
						}
					}
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, r)
					if mode == "lost_push_reply" && len(batch.Artifacts) > 0 && armed.Swap(false) && recorder.Code == http.StatusOK {
						http.Error(w, "response lost after durable artifact commit", http.StatusServiceUnavailable)
						return
					}
					if len(r.URL.Query()["artifact_digest"]) > 0 {
						capture.add(artifactWire{response: decodeArtifactWire(t, recorder.Body.Bytes(), recorder.Header().Get("Content-Encoding"))})
					}
					for key, values := range recorder.Header() {
						for _, value := range values {
							w.Header().Add(key, value)
						}
					}
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(recorder.Body.Bytes())
				}))
				t.Cleanup(proxy.Close)
				root.http = proxy
				enrollRelayMatrixReplica(t, root, personal, "artifact-fault-replica", true)
				issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Artifact fault recovery", Author: "source-assistant"})
				require.NoError(t, err)
				syncRelayMatrixNode(t, personal)
				producer := personal
				if mode == "revoke_before_download" || mode == "self_revoke_before_download" {
					producer = root
				}
				identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: producer.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
				require.NoError(t, err)
				_, err = producer.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				binding, err := personal.store.FederationBindingByProject(ctx, personal.project.ID)
				require.NoError(t, err)
				armed.Store(true)
				require.Error(t, federation.SyncFederationOnce(ctx, personal.store, binding, personal.credential))
				if mode == "lost_push_reply" {
					stored, err := root.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
					require.NoError(t, err)
					require.Equal(t, artifact, stored)
					pending, err := personal.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
					require.NoError(t, err)
					require.Len(t, pending, 1)
					require.Equal(t, artifact.Digest, pending[0].SourceUID)
					syncRelayMatrixNode(t, personal)
					require.Equal(t, int32(1), uploads.Load(), "committed retry offers metadata without reuploading complete vectors")
					pending, err = personal.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
					require.NoError(t, err)
					require.Empty(t, pending)
				} else {
					_, err := personal.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
					require.ErrorIs(t, err, db.ErrNotFound)
					frames := capture.take()
					require.Len(t, frames, 1)
					require.NotContains(t, string(frames[0].response), `"vector_bytes"`)
					require.NotContains(t, string(frames[0].response), artifact.Digest)
					current, err := personal.store.FederationBindingByProject(ctx, personal.project.ID)
					require.NoError(t, err)
					require.True(t, current.RelayConfig.UpstreamRevoked)
				}
			})
		}
	}
}

// R7/A2/A12: real HTTP offers no vectors for an exact durable hit and transfers
// complete misses in both directions through the existing enrolled connection.
// This proves wire reuse; semantic retrieval and provider calls are separate gates.
func TestArtifactManifestTransfer(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			personal := newRelayMatrixNode(t, backend, "relay-member")
			project, err := root.store.CreateProject(ctx, "shared-artifact-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			var capture artifactWireCapture
			handler := root.http.Config.Handler
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var batch db.RelayBatch
				_ = json.Unmarshal(decodeArtifactWire(t, raw, r.Header.Get("Content-Encoding")), &batch)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				if r.URL.Query().Get("stream") == db.RelayStreamArtifact || batch.Stream == db.RelayStreamArtifact {
					capture.add(artifactWire{method: r.Method, request: decodeArtifactWire(t, raw, r.Header.Get("Content-Encoding")), response: decodeArtifactWire(t, recorder.Body.Bytes(), recorder.Header().Get("Content-Encoding")), digests: r.URL.Query()["artifact_digest"]})
				}
				for key, values := range recorder.Header() {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
			}))
			t.Cleanup(proxy.Close)
			root.http = proxy
			enrollRelayMatrixReplica(t, root, personal, "artifact-replica", true)
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Shared original vectors", Body: strings.Repeat("界", 2100), Author: "source-assistant"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, personal)
			identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: root.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
			makeArtifact := func(vectors [][]float32) embedding.EmbeddingArtifact {
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), vectors)
				require.NoError(t, err)
				return artifact
			}
			retain := func(node *relayMatrixNode, artifact embedding.EmbeddingArtifact) {
				durable, err := node.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				require.True(t, durable)
			}
			var privateCanaries []string
			for _, node := range []*relayMatrixNode{root, personal} {
				privateProject, err := node.store.CreateProject(ctx, "unselected-vector-project")
				require.NoError(t, err)
				privateIssue, _, err := node.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: privateProject.ID, Title: "Private artifact canary", Author: "source-assistant"})
				require.NoError(t, err)
				privateIdentity := identity
				privateIdentity.ProjectUID, privateIdentity.IssueUID = privateProject.UID, privateIssue.UID
				privateIdentity.ProducerInstanceUID = node.store.InstanceUID()
				privateArtifact, err := embedding.NewArtifact(privateIdentity, embedding.EmbedText(privateIssue.Title, privateIssue.Body), [][]float32{{0.625123, 0.87123}})
				require.NoError(t, err)
				retain(node, privateArtifact)
				privateCanaries = append(privateCanaries, privateProject.UID, privateIssue.UID, privateArtifact.Digest, base64.StdEncoding.EncodeToString(privateArtifact.Chunks[0].VectorBytes))
			}
			assertPrivateWire := func(frames []artifactWire) {
				for _, frame := range frames {
					contents := string(frame.request) + string(frame.response)
					for _, raw := range [][]byte{frame.request, frame.response} {
						var batch db.RelayBatch
						_ = json.Unmarshal(raw, &batch)
						for _, envelope := range batch.Envelopes {
							contents += string(envelope.Body)
						}
					}
					for _, canary := range privateCanaries {
						require.NotContains(t, contents, canary)
					}
				}
			}
			have := makeArtifact([][]float32{{1, 0}, {0, 1}})
			retain(root, have)
			retain(personal, have)
			syncRelayMatrixNode(t, personal)
			wire := capture.take()
			assertPrivateWire(wire)
			require.GreaterOrEqual(t, len(wire), 2, "manifest offers must actually travel in both directions")
			pushOffers, pullOffers := 0, 0
			for _, frame := range wire {
				if frame.method == http.MethodPost {
					var batch db.RelayBatch
					require.NoError(t, json.Unmarshal(frame.request, &batch))
					if len(batch.Envelopes) > 0 {
						pushOffers++
					}
				}
				if frame.method == http.MethodGet {
					pullOffers++
				}
				require.Empty(t, frame.digests, "durable hits never request payloads")
				require.NotContains(t, string(frame.request), `"vector_bytes"`)
				require.NotContains(t, string(frame.response), `"vector_bytes"`)
			}
			require.Positive(t, pushOffers, "metadata push actually traverses the wire")
			require.Positive(t, pullOffers, "metadata pull actually traverses the wire")
			fromRoot := makeArtifact([][]float32{{0, 1}, {1, 0}})
			retain(root, fromRoot)
			syncRelayMatrixNode(t, personal)
			wire = capture.take()
			assertPrivateWire(wire)
			stored, err := personal.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, fromRoot.Digest)
			require.NoError(t, err)
			require.Equal(t, fromRoot, stored)
			var requested []string
			for _, frame := range wire {
				requested = append(requested, frame.digests...)
				if len(frame.digests) > 0 {
					var response map[string]any
					require.NoError(t, json.Unmarshal(frame.response, &response))
					require.Equal(t, []any{}, response["envelopes"], "artifact-only responses honor the published required array contract")
				}
			}
			require.Equal(t, []string{fromRoot.Digest}, requested, "only the exact missing digest downloads complete chunks")
			identity.ProducerInstanceUID = personal.store.InstanceUID()
			fromRelay := makeArtifact([][]float32{{-1, 0}, {0, -1}})
			retain(personal, fromRelay)
			syncRelayMatrixNode(t, personal)
			wire = capture.take()
			assertPrivateWire(wire)
			stored, err = root.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, fromRelay.Digest)
			require.NoError(t, err)
			require.Equal(t, fromRelay, stored)
			var uploaded []string
			for _, frame := range wire {
				if len(frame.request) == 0 {
					continue
				}
				var batch db.RelayBatch
				require.NoError(t, json.Unmarshal(frame.request, &batch))
				for _, artifact := range batch.Artifacts {
					uploaded = append(uploaded, artifact.Digest)
				}
			}
			require.Equal(t, []string{fromRelay.Digest}, uploaded)
			syncRelayMatrixNode(t, personal)
			wire = capture.take()
			assertPrivateWire(wire)
			for _, frame := range wire {
				require.NotContains(t, string(frame.request), `"vector_bytes"`)
				require.NotContains(t, string(frame.response), `"vector_bytes"`)
			}
		})
	}
}

type artifactWireCapture struct {
	mu     sync.Mutex
	frames []artifactWire
}

func (c *artifactWireCapture) add(frame artifactWire) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, frame)
}
func (c *artifactWireCapture) take() []artifactWire {
	c.mu.Lock()
	defer c.mu.Unlock()
	frames := c.frames
	c.frames = nil
	return frames
}

// Inspect decoded protocol bytes as well as request identities; compression
// must not make canary or vector-byte assertions vacuously pass.
func decodeArtifactWire(t *testing.T, raw []byte, encoding string) []byte {
	t.Helper()
	if encoding == "" {
		return bytes.Clone(raw)
	}
	require.Equal(t, "gzip", encoding)
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	decoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	return decoded
}
