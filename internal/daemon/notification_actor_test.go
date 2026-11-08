package daemon_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestProjectNotificationActorMatchesNativeNotificationMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		binding    bool
		enabled    bool
		push       bool
		relay      bool
		blankActor bool
		boundActor string
		want       string
	}{
		{name: "standalone", want: "caller"},
		{name: "direct push spoke", binding: true, enabled: true, push: true, boundActor: " bound-agent ", want: "bound-agent"},
		{name: "relay spoke", binding: true, enabled: true, push: true, relay: true, boundActor: "bound-agent", want: "caller"},
		{name: "disabled spoke", binding: true, enabled: false, push: true, boundActor: "bound-agent", want: "caller"},
		{name: "pull-only spoke", binding: true, enabled: true, push: false, boundActor: "bound-agent", want: "caller"},
		{name: "blank bound actor", binding: true, enabled: true, push: true, blankActor: true, boundActor: "bound-agent", want: "caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			project, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			if tc.binding {
				binding, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
					ProjectID: project.ID, Role: db.FederationRoleSpoke,
					HubURL: "https://hub.example", HubProjectID: 7, HubProjectUID: project.UID,
					Actor: tc.boundActor, Enabled: tc.enabled, PushEnabled: tc.push,
				})
				require.NoError(t, err)
				if tc.blankActor {
					// Legacy persisted rows can be blank even though current writes
					// reject them; keep this fixture local to the test database.
					_, err := env.DB.ExecContext(t.Context(), `UPDATE federation_bindings SET bound_actor='' WHERE project_id=?`, project.ID)
					require.NoError(t, err)
				}
				if tc.relay {
					rootUID := "00000000000000000000000002"
					publicKey, _, err := ed25519.GenerateKey(rand.Reader)
					require.NoError(t, err)
					require.NoError(t, env.DB.PinRootAuthority(t.Context(), db.RootKeyPin{
						ProjectUID: project.UID, AuthorityUID: rootUID,
						KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
					}))
					_, err = env.DB.SetRelayBindingConfig(t.Context(), project.ID, db.RelayBindingConfig{
						ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000007",
						UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
						HubPath: []string{rootUID, env.DB.InstanceUID()}, LocalActor: binding.Actor, ResetEpoch: 1,
					})
					require.NoError(t, err)
				}
			}

			resp, raw := envDoRaw(t, env, http.MethodGet,
				projectPath(project.ID)+"/notifications/actor?requested_actor="+url.QueryEscape("caller"), nil, nil)
			require.Equalf(t, http.StatusOK, resp.StatusCode, "response: %s", raw)
			var body struct {
				EffectiveActor string `json:"effective_actor"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			assert.Equal(t, tc.want, body.EffectiveActor)
		})
	}
}

func TestProjectNotificationActorAuthenticatedIdentityOverridesFallback(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "actor-token", Actor: "authenticated-actor", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/notifications/actor?requested_actor=untrusted-fallback", nil,
		map[string]string{"Authorization": "Bearer actor-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "response: %s", raw)
	var body struct {
		EffectiveActor string `json:"effective_actor"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "authenticated-actor", body.EffectiveActor)
}

func TestProjectNotificationActorUsesFallbackForStaticToken(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("static-token"))
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/notifications/actor?requested_actor=untrusted-fallback", nil,
		map[string]string{"Authorization": "Bearer static-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "response: %s", raw)
	var body struct {
		EffectiveActor string `json:"effective_actor"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "untrusted-fallback", body.EffectiveActor)
}

func TestProjectNotificationActorHidesProjectsOutsideTokenScope(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	visible, err := env.DB.CreateProject(t.Context(), "visible-project")
	require.NoError(t, err)
	hidden, err := env.DB.CreateProject(t.Context(), "hidden-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: visible.ID, Title: "Visible root", Author: "worker",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "scoped-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: visible.UID, RootIssueUID: root.UID},
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(visible.ID)+"/notifications/actor?requested_actor=worker", nil,
		map[string]string{"Authorization": "Bearer scoped-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "response: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodGet,
		projectPath(hidden.ID)+"/notifications/actor?requested_actor=worker", nil,
		map[string]string{"Authorization": "Bearer scoped-token"})
	require.Equalf(t, http.StatusNotFound, resp.StatusCode, "response: %s", raw)
}

func TestProjectNotificationActorRequiresUnauthenticatedFallback(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/notifications/actor", nil, nil)
	require.Equalf(t, http.StatusBadRequest, resp.StatusCode, "response: %s", raw)
	assert.Contains(t, string(raw), "actor is required")
}
