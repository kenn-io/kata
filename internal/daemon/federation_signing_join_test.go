package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

func TestReplicaJoinRetainsSigningReferencesBeforeSync(t *testing.T) {
	t.Setenv("TEST_REPLICA_SIGNING_KEY", strings.Repeat("k", 64))
	credentials := newReplicaCredentialStore()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", json.RawMessage(`{"hub_url":"https://HUB.EXAMPLE:443/mount","hub_project_id":1,"hub_project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","project_name":"spoke-project","replay_horizon_event_id":1,"token":"enrollment","capabilities":"pull","actor":"example-actor","signing_key_id":"key-a","signing_key_env":"TEST_REPLICA_SIGNING_KEY"}`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	stored, found, err := credentials.FederationCredential(t.Context(), "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, stored.Signing)
	require.Equal(t, "key-a", stored.Signing.KeyID)
	require.Equal(t, "https://hub.example/mount", stored.Signing.HubURL)
	// Repeating native join without new signing flags must retain the source.
	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", json.RawMessage(`{"hub_url":"https://hub.example/mount","hub_project_id":1,"hub_project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","project_name":"spoke-project","replay_horizon_event_id":1,"token":"enrollment","capabilities":"pull","actor":"example-actor"}`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	retained, _, err := credentials.FederationCredential(t.Context(), "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	require.Equal(t, stored.Signing, retained.Signing)

}

// pausingCredentialStore pauses the first credential write after arm, so a
// test can act while a replica operation is between its read and its write.
type pausingCredentialStore struct {
	*replicaCredentialStore
	armed  atomic.Bool
	paused chan struct{}
	resume chan struct{}
}

func (s *pausingCredentialStore) StoreFederationCredential(
	ctx context.Context, projectUID string, credential config.FederationCredential,
) error {
	if s.armed.CompareAndSwap(true, false) {
		close(s.paused)
		<-s.resume
	}
	return s.replicaCredentialStore.StoreFederationCredential(ctx, projectUID, credential)
}

// Contract: a signing selection saved while a rejoin is in flight is not
// overwritten by the rejoin's retained, older selection.
func TestConfigureSigningIsNotLostToConcurrentRejoin(t *testing.T) {
	t.Setenv("TEST_OLD_SIGNING_KEY", strings.Repeat("o", 64))
	t.Setenv("TEST_NEW_SIGNING_KEY", strings.Repeat("n", 64))
	credentials := &pausingCredentialStore{
		replicaCredentialStore: newReplicaCredentialStore(),
		paused:                 make(chan struct{}), resume: make(chan struct{}),
	}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
	join := `{"hub_url":"https://hub.example","hub_project_id":1,"hub_project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","project_name":"spoke-project","replay_horizon_event_id":1,"token":"enrollment","capabilities":"pull","actor":"example-actor"`
	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas",
		json.RawMessage(join+`,"signing_key_id":"key-old","signing_key_env":"TEST_OLD_SIGNING_KEY"}`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	post := func(path, body string) <-chan int {
		status := make(chan int, 1)
		go func() {
			res, err := env.HTTP.Post(env.URL+path, "application/json", strings.NewReader(body))
			if err != nil {
				status <- 0
				return
			}
			_ = res.Body.Close()
			status <- res.StatusCode
		}()
		return status
	}
	credentials.armed.Store(true)
	rejoin := post("/api/v1/federation/replicas", join+"}")
	<-credentials.paused
	configure := post(configureSigningPath, `{"key_id":"key-new","key_env":"TEST_NEW_SIGNING_KEY"}`)
	// Give an unserialized configure time to finish inside the rejoin window.
	configureStatus := 0
	select {
	case configureStatus = <-configure:
	case <-time.After(200 * time.Millisecond): //nolint:kennlint // absence window; the paused rejoin holds the credential store, so a serialized configure cannot finish
	}
	close(credentials.resume)
	require.Equal(t, http.StatusOK, <-rejoin)
	if configureStatus == 0 {
		configureStatus = <-configure
	}
	require.Equal(t, http.StatusNoContent, configureStatus)
	got, found, err := credentials.FederationCredential(t.Context(), replicaHubProjectUID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "key-new", got.Signing.KeyID)
}
