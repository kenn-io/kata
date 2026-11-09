package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/tracker"
)

// Embed the concrete store to preserve native optional capabilities, including
// enrollment reset checks. Only the receiving handler's assertion-scoped
// binding read fails; earlier authentication reads retain their real behavior.
type trackerReceiverDeadlineStore struct {
	*sqlitestore.Store
	projectID       int64
	assertionExpiry time.Time
	bindingReadHits atomic.Int32
	controlBegins   atomic.Int32
}

func (s *trackerReceiverDeadlineStore) FederationBindingByProject(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	if deadline, ok := ctx.Deadline(); ok && projectID == s.projectID && deadline.Equal(s.assertionExpiry) {
		s.bindingReadHits.Add(1)
		return db.FederationBinding{}, fmt.Errorf("fixture-private-binding-detail: %w", context.DeadlineExceeded)
	}
	return s.Store.FederationBindingByProject(ctx, projectID)
}

func (s *trackerReceiverDeadlineStore) BeginTrackerControl(ctx context.Context, receipt tracker.ControlReceipt) (tracker.ControlReceipt, bool, error) {
	s.controlBegins.Add(1)
	return s.Store.BeginTrackerControl(ctx, receipt)
}

// Contract/review counterexample: a binding read that returns DeadlineExceeded
// after the original assertion deadline is installed must yield sanitized
// 504 tracker_timeout. Collapsing that I/O failure into capability-unavailable
// would yield 503 and fail this real signed receiving-handler regression.
func TestTrackerRelayReceiverBindingReadDeadline(t *testing.T) {
	for _, action := range []string{"read", "control"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				home := t.TempDir()
				t.Setenv("KATA_HOME", home)
				t.Setenv("KATA_AUTH_TOKEN", "fixture-local-daemon-secret")
				t.Setenv("KATA_AUTH_TOKEN_FILE", "")
				t.Setenv("TRACKER_RECEIVER_SIGNING_KEY", strings.Repeat("r", 64))
				store, err := sqlitestore.Open(ctx, filepath.Join(home, "receiver.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				project, err := store.CreateProject(ctx, "hub-project")
				require.NoError(t, err)
				_, err = store.EnableProjectFederation(ctx, project.ID, "member")
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
				require.NoError(t, store.PinRootAuthority(ctx, pin))
				parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "fixture-parent-secret", Actor: "member", AdminActor: "admin"})
				require.NoError(t, err)
				const peerUID = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
				grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peerUID, ProtocolVersion: db.RelayProtocolVersion, RootKeyID: pin.KeyID, Token: "fixture-enrollment-secret"})
				require.NoError(t, err)
				needsReset, err := store.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
				require.NoError(t, err)
				require.False(t, needsReset, "fixture must admit ordinary relay traffic before injecting the later binding read")
				wrapped := &trackerReceiverDeadlineStore{Store: store, projectID: project.ID}
				runtime, err := NewTrackerRuntime(ctx, wrapped, nil, nil)
				require.NoError(t, err)
				source := signingpolicy.Source{KeyID: "fixture-receiver-key", KeyEnv: "TRACKER_RECEIVER_SIGNING_KEY"}
				statePath := filepath.Join(home, "replay.state")
				require.NoError(t, federationsigning.InitializeReplayState(statePath))
				const origin = "https://hub.example"
				verifier, err := federationsigning.NewVerifier(origin, []signingpolicy.Key{{Source: source, EnrollmentID: grant.Enrollment.ID}}, statePath)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, verifier.Close()) })
				var transportCalls atomic.Int32
				server := NewServer(ServerConfig{
					DB: wrapped, TrackerRuntime: runtime, FederationSigning: verifier, FederationSigningRequired: true,
					Auth: config.AuthConfig{Token: "fixture-local-daemon-secret"},
					FederationHubClient: func(context.Context, config.FederationCredential) (*http.Client, error) {
						transportCalls.Add(1)
						return nil, fmt.Errorf("unexpected fixture outbound transport")
					},
				})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				handler, err := server.HandlerFor(ListenerPolicy{Kind: ListenerFederation})
				require.NoError(t, err)
				// Let the actual verifier finish its mandatory quarantine on the
				// synctest clock, rather than bypassing authentication or its state.
				time.Sleep(federationsigning.Quarantine)
				const operationUID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
				payload := api.TrackerRelayPayload{Kind: "discover", UID: operationUID}
				if action == "control" {
					payload.Kind = "execute"
					payload.Operation = &api.TrackerOperationBody{UID: operationUID, Kind: tracker.ControlDisable, Provider: "notion", Resource: "notion:source", Selection: &tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", Generation: "g1"}}
				}
				digest, err := trackerRelayPayloadDigest(payload, action == "control")
				require.NoError(t, err)
				// A shorter original budget also detects replacement with a fresh
				// default 30-second assertion instead of preserving this deadline.
				expiresAt := time.Now().Add(12 * time.Second).Unix()
				wrapped.assertionExpiry = time.Unix(expiresAt, 0)
				body := api.TrackerRelayBody{Payload: payload, Assertion: tracker.RelayAssertion{
					Version: tracker.ControlProtocolVersion, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID,
					Source:            tracker.Principal{AuthorityInstanceUID: peerUID, Kind: tracker.SubjectAccount, Subject: "member"},
					IssuerInstanceUID: peerUID, AudienceInstanceUID: store.InstanceUID(), OperationUID: operationUID, RequestDigest: digest, ExpiresAt: expiresAt,
					Path: []tracker.RelayHop{{InstanceUID: peerUID, BindingUID: grant.Enrollment.RelayBindingUID, ResetEpoch: grant.Enrollment.RelayResetEpoch}},
				}}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				request := httptest.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/federation/tracker/%s", origin, project.ID, action), bytes.NewReader(raw))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+grant.Token)
				request.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
				require.NoError(t, federationsigning.Sign(request, source))
				before, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				require.EqualValues(t, 1, wrapped.bindingReadHits.Load(), "signature, enrollment, reset, project, assertion and digest checks must reach the original-deadline binding read")
				require.Equal(t, http.StatusGatewayTimeout, response.Code, response.Body.String())
				var envelope api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
				require.Equal(t, "tracker_timeout", envelope.Error.Code)
				for _, secret := range []string{"fixture-private-binding-detail", "fixture-local-daemon-secret", "fixture-parent-secret", grant.Token, strings.Repeat("r", 64)} {
					require.NotContains(t, response.Body.String(), secret)
				}
				require.Zero(t, wrapped.controlBegins.Load(), "timeout must not admit a downstream control")
				require.Zero(t, transportCalls.Load(), "timeout must not open an upstream or provider transport")
				_, err = store.ReadTrackerControl(ctx, project.UID, operationUID)
				require.ErrorIs(t, err, db.ErrNotFound, "a receiving lookup timeout must not manufacture a receipt")
				after, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after, "the rejected receive must not commit project events")
			})
		})
	}
}
