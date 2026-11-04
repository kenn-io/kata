package daemon_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
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
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/testenv"
)

type runtimeRoundTrip func(*http.Request) (*http.Response, error)

func (f runtimeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runtimeForwardBeat(t *testing.T, f *runtimeForwardFixture, id string) api.BeatCronRuntimeResponse {
	t.Helper()
	status, raw := runtimeHTTPRequest(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", f.sp.ID, id), runtimeHTTPBody())
	require.Equalf(t, 200, status, "%s", raw)
	var out api.BeatCronRuntimeResponse
	require.NoError(t, json.Unmarshal(raw, &out.Body))
	return out
}
func TestCronRuntimeForwardingEnrollment(t *testing.T) {
	f := newRuntimeForwardFixture(t, func(_ *runtimeForwardFixture, cfg *daemon.ServerConfig) {
		cfg.FederationPushPending = func(context.Context, db.FederationBinding, config.FederationCredential) error {
			t.Error("heartbeat flushed pending events")
			return errors.New("unexpected push")
		}
	})
	release, err := federationcoord.TryBeginSync(t.Context(), federationcoord.Key(f.spoke.DB.InstanceUID(), f.sp.ID), f.spoke.DB, f.sp.ID)
	require.NoError(t, err)
	defer release()
	id := cronHTTPUID(t)
	out := runtimeForwardBeat(t, f, id)
	require.Equal(t, "recorded", out.Body.Forwarded)
	require.Equal(t, "direct", out.Body.Runtime.ReportedVia)
	status, raw := runtimeHubHTTPRequest(t, f.hub, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.hp.ID), nil)
	require.Equalf(t, 200, status, "%s", raw)
	var inventory api.CronRuntimesResponse
	require.NoError(t, json.Unmarshal(raw, &inventory.Body))
	require.Len(t, inventory.Body.Runtimes, 1)
	r := inventory.Body.Runtimes[0]
	require.Equal(t, id, r.UID)
	require.Equal(t, "forwarded", r.ReportedVia)
	require.Equal(t, "tester", r.Actor)
	require.Equal(t, "tester", r.ForwarderActor)
}
func TestCronRuntimeForwardingOutcomes(t *testing.T) {
	for _, mode := range []string{"unavailable", "timeout", "unsupported", "disabled", "push-disabled", "relay", "no-credentials", "credential-target", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimeForwardFixture(t, func(f *runtimeForwardFixture, cfg *daemon.ServerConfig) {
				b, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
				require.NoError(t, err)
				switch mode {
				case "disabled":
					b.Enabled = false
				case "push-disabled":
					b.PushEnabled = false
				case "relay":
					runtimeConfigureRelay(t, f.hub, f.spoke, f.sp)
				case "no-credentials":
					require.NoError(t, config.DefaultFederationCredentialStore().DeleteFederationCredential(t.Context(), f.sp.UID))
				case "credential-target":
					c := f.credential
					c.HubURL = "https://other.example"
					require.NoError(t, config.WriteFederationCredential(f.sp.UID, c))
				case "revoked":
					require.NoError(t, f.hub.DB.RevokeFederationEnrollment(t.Context(), f.enrollmentID))
				case "unavailable":
					cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
						return nil, errors.New("offline")
					}
				case "timeout":
					cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
						return &http.Client{Transport: runtimeRoundTrip(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}, nil
					}
				case "unsupported":
					cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
						return &http.Client{Transport: runtimeRoundTrip(func(*http.Request) (*http.Response, error) {
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"project_uid":"` + f.sp.UID + `"}`))}, nil
						})}, nil
					}
				}
				if mode == "disabled" || mode == "push-disabled" {
					_, err = f.spoke.DB.UpsertFederationBinding(t.Context(), b)
					require.NoError(t, err)
				}
			})
			id := cronHTTPUID(t)
			out := runtimeForwardBeat(t, f, id)
			want := "unavailable"
			if mode == "disabled" || mode == "push-disabled" || mode == "relay" {
				want = "not_applicable"
			}
			if mode == "unsupported" {
				want = "unsupported"
			}
			require.Equal(t, want, out.Body.Forwarded)
			status, raw := runtimeHTTPRequest(t, f.target, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.sp.ID), nil)
			require.Equal(t, 200, status)
			require.Contains(t, string(raw), id)
		})
	}
}
func TestCronRuntimeForwardingRebindDuringPreflight(t *testing.T) {
	var trapCalls atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { trapCalls.Add(1) }))
	t.Cleanup(trap.Close)
	f := newRuntimeForwardFixture(t, func(f *runtimeForwardFixture, cfg *daemon.ServerConfig) {
		factory := cfg.FederationHubClient
		cfg.FederationHubClient = func(ctx context.Context, c config.FederationCredential) (*http.Client, error) {
			client, err := factory(ctx, c)
			if err != nil {
				return nil, err
			}
			transport := client.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			client.Transport = runtimeRoundTrip(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
					b, e := f.spoke.DB.FederationBindingByProject(r.Context(), f.sp.ID)
					require.NoError(t, e)
					b.HubURL = trap.URL
					_, e = f.spoke.DB.UpsertFederationBinding(r.Context(), b)
					require.NoError(t, e)
				}
				return response, err
			})
			return client, nil
		}
	})
	out := runtimeForwardBeat(t, f, cronHTTPUID(t))
	require.Equal(t, "unavailable", out.Body.Forwarded)
	require.Zero(t, trapCalls.Load(), "old credential reached replacement origin")
	status, raw := runtimeHubHTTPRequest(t, f.hub, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.hp.ID), nil)
	require.Equal(t, 200, status)
	require.NotContains(t, string(raw), out.Body.Runtime.UID)
}
func TestCronRuntimeEnrollmentScope(t *testing.T) {
	f := newRuntimeForwardFixture(t, nil)
	other := seedProject(t, f.hub, "another-project")
	_, err := f.hub.DB.EnableProjectFederation(t.Context(), other.ID, "tester")
	require.NoError(t, err)
	id := cronHTTPUID(t)
	body := runtimeHTTPBody()
	body["actor"] = "tester"
	headers := map[string]string{"Authorization": "Bearer " + f.credential.Token}
	resp, raw := envDoRaw(t, f.hub, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", other.ID, id), body, headers)
	require.Equalf(t, http.StatusForbidden, resp.StatusCode, "%s", raw)
	require.NoError(t, f.hub.DB.RevokeFederationEnrollment(t.Context(), f.enrollmentID))
	resp, raw = envDoRaw(t, f.hub, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", f.hp.ID, id), body, headers)
	require.GreaterOrEqualf(t, resp.StatusCode, 400, "%s", raw)
	status, raw := runtimeHubHTTPRequest(t, f.hub, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.hp.ID), nil)
	require.Equal(t, 200, status)
	require.NotContains(t, string(raw), id)
}

type runtimeForwardFixture struct {
	hub, spoke, target *testenv.Env
	hp, sp             db.Project
	credential         config.FederationCredential
	enrollmentID       int64
}

func newRuntimeForwardFixture(t *testing.T, configure func(*runtimeForwardFixture, *daemon.ServerConfig)) *runtimeForwardFixture {
	t.Helper()
	hub := testenv.New(t, testenv.WithAuthToken("tok"))
	hp := seedProject(t, hub, "hub-project")
	_, err := hub.DB.EnableProjectFederation(t.Context(), hp.ID, "tester")
	require.NoError(t, err)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	sp, err := spoke.DB.CreateProjectWithUID(t.Context(), "spoke-project", hp.UID)
	require.NoError(t, err)
	enrollment, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: cronHTTPUID(t) + "-token", SpokeInstanceUID: spoke.DB.InstanceUID(), ProjectID: &hp.ID, Capabilities: "push,pull", Actor: "tester"})
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: sp.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: hp.ID, HubProjectUID: hp.UID, Actor: "tester", Enabled: true, PushEnabled: true, ReplayHorizonEventID: 1})
	require.NoError(t, err)
	credential := config.FederationCredential{HubURL: hub.URL, HubProjectID: hp.ID, Token: enrollment.Token, Actor: "tester", Capabilities: "push,pull"}
	require.NoError(t, config.WriteFederationCredential(sp.UID, credential))
	f := &runtimeForwardFixture{hub: hub, spoke: spoke, hp: hp, sp: sp, credential: credential, enrollmentID: enrollment.Enrollment.ID}
	cfg := daemon.ServerConfig{DB: spoke.DB, FederationHubClient: federation.CredentialHTTPClient}
	if configure != nil {
		configure(f, &cfg)
	}
	srv := daemon.NewServer(cfg)
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	f.target = &testenv.Env{DB: spoke.DB, URL: server.URL, HTTP: server.Client()}
	return f
}
func runtimeHubHTTPRequest(t *testing.T, env *testenv.Env, method, path string, body any) (int, []byte) {
	t.Helper()
	response, raw := envDoRaw(t, env, method, path, body, map[string]string{"Authorization": "Bearer tok"})
	return response.StatusCode, raw
}
func runtimeConfigureRelay(t *testing.T, hub, spoke *testenv.Env, sp db.Project) {
	t.Helper()
	rootUID := hub.DB.InstanceUID()
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, spoke.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: sp.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	_, err = spoke.DB.SetRelayBindingConfig(t.Context(), sp.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: cronHTTPUID(t), UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, spoke.DB.InstanceUID()}, LocalActor: "tester", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
}

func TestCronRuntimeForwardingCredentialChanged(t *testing.T) {
	for _, mode := range []string{"target", "project", "leave-pending", "rotated-token"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimeForwardFixture(t, func(f *runtimeForwardFixture, cfg *daemon.ServerConfig) {
				factory := cfg.FederationHubClient
				cfg.FederationHubClient = func(ctx context.Context, c config.FederationCredential) (*http.Client, error) {
					client, err := factory(ctx, c)
					if err != nil {
						return nil, err
					}
					transport := client.Transport
					if transport == nil {
						transport = http.DefaultTransport
					}
					client.Transport = runtimeRoundTrip(func(r *http.Request) (*http.Response, error) {
						response, err := transport.RoundTrip(r)
						if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
							changed := f.credential
							switch mode {
							case "target":
								changed.HubURL = "https://other.example"
							case "project":
								changed.HubProjectID++
							case "leave-pending":
								changed.LeavePending = true
							case "rotated-token":
								changed.Token = "replacement-token"
							}
							require.NoError(t, config.WriteFederationCredential(f.sp.UID, changed))
						}
						return response, err
					})
					return client, nil
				}
			})
			out := runtimeForwardBeat(t, f, cronHTTPUID(t))
			require.Equal(t, "unavailable", out.Body.Forwarded)
		})
	}
}
func TestCronRuntimeSignedIngress(t *testing.T) {
	f := newRuntimeForwardFixture(t, nil)
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("TEST_RUNTIME_SIGNING_KEY", strings.Repeat("r", 64))
		source := signingpolicy.Source{KeyID: "runtime-key", KeyEnv: "TEST_RUNTIME_SIGNING_KEY"}
		state := filepath.Join(t.TempDir(), "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		verifier, err := federationsigning.NewVerifier("https://daemon.example", []signingpolicy.Key{{Source: source, EnrollmentID: f.enrollmentID}}, state)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, verifier.Close()) })
		server := daemon.NewServer(daemon.ServerConfig{DB: f.hub.DB, Auth: config.AuthConfig{Token: "tok"}, FederationSigning: verifier, FederationSigningRequired: true})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		ingress, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
		require.NoError(t, err)
		time.Sleep(federationsigning.Quarantine)
		send := func(handler http.Handler, token string, signed bool) *httptest.ResponseRecorder {
			body := runtimeHTTPBody()
			body["actor"] = "tester"
			raw, e := json.Marshal(body)
			require.NoError(t, e)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("https://daemon.example/api/v1/projects/%d/cron/runtimes/%s", f.hp.ID, cronHTTPUID(t)), bytes.NewReader(raw))
			request.RemoteAddr = "127.0.0.1:1234"
			request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 443}))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			if signed {
				require.NoError(t, federationsigning.Sign(request, source))
			}
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			return out
		}
		for _, handler := range []http.Handler{server.Handler(), ingress} {
			out := send(handler, f.credential.Token, false)
			require.Equal(t, 401, out.Code, out.Body.String())
			out = send(handler, f.credential.Token, true)
			require.Equal(t, 200, out.Code, out.Body.String())
			require.Contains(t, out.Body.String(), `"reported_via":"forwarded"`)
		}
		owner := send(server.Handler(), "tok", false)
		require.Equal(t, 200, owner.Code, owner.Body.String())
		owner = send(ingress, "tok", false)
		require.Equal(t, 403, owner.Code, owner.Body.String())
	})
}

func TestCronRuntimeChoicesForwarding(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "unrelated", "lookalike", "supported"} {
		capable := mode == "supported"
		t.Run(mode, func(t *testing.T) {
			var sent api.BeatCronRuntimeBody
			f := newRuntimeForwardFixture(t, func(f *runtimeForwardFixture, cfg *daemon.ServerConfig) {
				cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
					return &http.Client{Transport: runtimeRoundTrip(func(r *http.Request) (*http.Response, error) {
						var value any
						if r.Method == http.MethodGet {
							meta := api.ProjectFederationBody{ProjectUID: f.sp.UID, CronRuntimeBeats: true}
							switch mode {
							case "empty":
								meta.Features = []string{}
							case "unrelated":
								meta.Features = []string{"another_feature"}
							case "lookalike":
								meta.Features = []string{"cron_runtime_choices_v1_extra"}
							case "supported":
								meta.Features = []string{"cron_runtime_choices_v1"}
							}
							value = meta
						} else {
							require.NoError(t, json.UnmarshalRead(r.Body, &sent))
							out := api.BeatCronRuntimeResponse{}
							out.Body.Runtime.UID = strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/", f.hp.ID))
							value = out.Body
						}
						raw, err := json.Marshal(value)
						require.NoError(t, err)
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
					})}, nil
				}
			})
			body := runtimeHTTPBody()
			body["choices"] = map[string][]string{"options.agent": {"review"}}
			status, raw := runtimeHTTPRequest(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", f.sp.ID, cronHTTPUID(t)), body)
			require.Equalf(t, 200, status, "%s", raw)
			require.Contains(t, string(raw), `"forwarded":"recorded"`)
			require.Contains(t, string(raw), `"choices"`)
			if capable {
				require.Equal(t, []string{"review"}, sent.Choices["options.agent"])
			} else {
				require.Nil(t, sent.Choices)
			}
			status, raw = runtimeHTTPRequest(t, f.target, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.sp.ID), nil)
			require.Equal(t, 200, status)
			require.Contains(t, string(raw), `"options.agent":["review"]`)
		})
	}
}

// Contract: every admitted beat fits the same root ingress limit after forwarding.
// The outgoing body retains optional/null members and only removes actor and,
// for an older root, choices. Root attribution always comes from the enrollment.
func TestCronRuntimeForwardingPreservesAdmittedBody(t *testing.T) {
	for _, capable := range []bool{true, false} {
		for _, mode := range []string{"defaults", "stopping", "null_versions"} {
			t.Run(fmt.Sprintf("choices_%v/%s", capable, mode), func(t *testing.T) {
				var sent []byte
				f := newRuntimeForwardFixture(t, func(f *runtimeForwardFixture, cfg *daemon.ServerConfig) {
					factory := cfg.FederationHubClient
					cfg.FederationHubClient = func(ctx context.Context, cred config.FederationCredential) (*http.Client, error) {
						client, err := factory(ctx, cred)
						if err != nil {
							return nil, err
						}
						transport := client.Transport
						if transport == nil {
							transport = http.DefaultTransport
						}
						client.Transport = runtimeRoundTrip(func(r *http.Request) (*http.Response, error) {
							if r.Method == http.MethodGet {
								meta := api.ProjectFederationBody{ProjectUID: f.sp.UID, CronRuntimeBeats: true}
								if capable {
									meta.Features = []string{cron.RuntimeChoicesFeature}
								}
								raw, err := json.Marshal(meta)
								require.NoError(t, err)
								return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
							}
							var err error
							sent, err = io.ReadAll(r.Body)
							require.NoError(t, err)
							require.NoError(t, r.Body.Close())
							r.Body = io.NopCloser(bytes.NewReader(sent))
							return transport.RoundTrip(r)
						})
						return client, nil
					}
				})
				values := make([]string, 32)
				for i := range values {
					values[i] = fmt.Sprintf("%02d-%s", i, strings.Repeat("x", 120))
				}
				body := runtimeHTTPBody()
				body["choices"] = map[string][]string{"options.agent": values, "options.model": values[:31], "checkout_key": {}}
				if mode == "stopping" {
					body["interval_seconds"], body["state"] = 77, "stopping"
				}
				if mode == "null_versions" {
					body["adapter_version"], body["kata_version"] = nil, nil
				}
				// Exercise raw Unicode/HTML-sensitive text, JSON escapes, an escaped
				// member name and slash without spending the boundary on whitespace.
				label := "worker / \" \\ \n \u2028 <>&"
				encode := func() []byte {
					body["label"] = label
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					raw = bytes.Replace(raw, []byte(`"label":`), []byte(`"\u006cabel":`), 1)
					return bytes.Replace(raw, []byte(`/`), []byte(`\/`), 1)
				}
				raw := encode()
				require.Less(t, len(raw), cron.RuntimeBeatLimit)
				label += strings.Repeat("p", cron.RuntimeBeatLimit-len(raw))
				require.LessOrEqual(t, len(label), 256)
				raw = encode()
				require.Len(t, raw, cron.RuntimeBeatLimit)
				id := cronHTTPUID(t)
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/runtimes/%s", f.target.URL, f.sp.ID, id), bytes.NewReader(raw))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				response, err := f.target.HTTP.Do(request)
				require.NoError(t, err)
				responseBody := readClose(t, response)
				require.Equal(t, 200, response.StatusCode, string(responseBody))
				var local api.BeatCronRuntimeResponse
				require.NoError(t, json.Unmarshal(responseBody, &local.Body))
				require.Equal(t, "recorded", local.Body.Forwarded)
				require.LessOrEqual(t, len(sent), len(raw))
				delete(body, "actor")
				if !capable {
					delete(body, "choices")
				}
				want, err := json.Marshal(body)
				require.NoError(t, err)
				require.JSONEq(t, string(want), string(sent))
				status, rootRaw := runtimeHubHTTPRequest(t, f.hub, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.hp.ID), nil)
				require.Equal(t, 200, status)
				var root api.CronRuntimesResponse
				require.NoError(t, json.Unmarshal(rootRaw, &root.Body))
				require.Len(t, root.Body.Runtimes, 1)
				runtime := root.Body.Runtimes[0]
				require.Equal(t, "tester", runtime.Actor)
				require.Equal(t, "tester", runtime.ForwarderActor)
				require.Equal(t, "forwarded", runtime.ReportedVia)
				require.Equal(t, label, runtime.Label)
				if mode == "stopping" {
					require.Equal(t, 77, runtime.IntervalSeconds)
					require.Equal(t, "stopping", runtime.State)
				} else {
					require.Equal(t, 60, runtime.IntervalSeconds)
					require.Equal(t, "active", runtime.State)
				}
				require.Len(t, local.Body.Runtime.Choices["options.agent"], 32)
				if capable {
					require.Equal(t, local.Body.Runtime.RuntimeBeat, runtime.RuntimeBeat)
				} else {
					require.Empty(t, runtime.Choices)
				}
			})
		}
	}
}

// Enrollment attribution is derived when omitted and checked when supplied.
func TestCronRuntimeEnrollmentActor(t *testing.T) {
	f := newRuntimeForwardFixture(t, nil)
	for _, tc := range []struct {
		name, actor string
		status      int
	}{{"omitted", "", 200}, {"blank", " ", 200}, {"matching", "tester", 200}, {"mismatch", "another-actor", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			body := runtimeHTTPBody()
			delete(body, "actor")
			if tc.name != "omitted" {
				body["actor"] = tc.actor
			}
			id := cronHTTPUID(t)
			response, raw := envDoRaw(t, f.hub, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", f.hp.ID, id), body, map[string]string{"Authorization": "Bearer " + f.credential.Token})
			require.Equal(t, tc.status, response.StatusCode, string(raw))
			if tc.status == 200 {
				var beat api.BeatCronRuntimeResponse
				require.NoError(t, json.Unmarshal(raw, &beat.Body))
				require.Equal(t, "tester", beat.Body.Runtime.Actor)
				require.Equal(t, "tester", beat.Body.Runtime.ForwarderActor)
			} else {
				_, inventory := runtimeHubHTTPRequest(t, f.hub, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", f.hp.ID), nil)
				require.NotContains(t, string(inventory), id)
			}
		})
	}
}
