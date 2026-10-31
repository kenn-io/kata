package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/testenv"
)

type cronForwardFixture struct {
	hub, spoke, target *testenv.Env
	hp, sp             db.Project
	parent             db.Issue
	definition         cron.JobDefinition
	credential         config.FederationCredential
	enrollmentID       int64
	writes, polls      *atomic.Int32
}

func newCronForwardFixture(t *testing.T, configure func(*cronForwardFixture, *daemon.ServerConfig)) *cronForwardFixture {
	t.Helper()
	hub, hp, _, definition := nativeCronEnrollmentFixture(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	_, err := spoke.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	sp, err := spoke.DB.CreateProjectWithUID(t.Context(), "spoke-project", hp.UID)
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, hub, hp.ID, spoke.DB.InstanceUID(), "push,pull")
	upstream, err := url.Parse(hub.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var writes, polls atomic.Int32
	hubEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/cron/jobs") && r.Method != http.MethodGet {
			writes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/federation/events") && r.Method == http.MethodGet {
			polls.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(hubEndpoint.Close)

	_, err = spoke.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: sp.ID, Role: db.FederationRoleSpoke, HubURL: hubEndpoint.URL, HubProjectID: hp.ID, HubProjectUID: hp.UID, Actor: "tester", Enabled: true, PushEnabled: true, ReplayHorizonEventID: 1})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(sp.UID, config.FederationCredential{HubURL: hubEndpoint.URL, HubProjectID: hp.ID, Token: enrollment.Token, Actor: "tester", Capabilities: "push,pull"}))
	parent, _, err := spoke.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sp.ID, Title: "Local parent", Author: "tester"})
	require.NoError(t, err)
	definition.Issue.ParentUID = parent.UID

	f := &cronForwardFixture{hub: hub, spoke: spoke, hp: hp, sp: sp, parent: parent, definition: definition, enrollmentID: enrollment.Enrollment.ID, writes: &writes, polls: &polls,
		credential: config.FederationCredential{HubURL: hubEndpoint.URL, HubProjectID: hp.ID, Token: enrollment.Token, Actor: "tester", Capabilities: "push,pull"}}
	cfg := forwardingTestConfig(daemon.ServerConfig{DB: spoke.DB})
	if configure != nil {
		configure(f, &cfg)
	}
	srv := daemon.NewServer(cfg)
	handler := srv.Handler()
	if cfg.HostAccess != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.Handler().ServeHTTP(w, r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "tester"})))
		})
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	f.target = &testenv.Env{DB: spoke.DB, URL: server.URL, HTTP: server.Client()}
	return f
}

func TestCronSpokeForwardLifecycle(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	hub, hp, spoke, sp, parent, definition, target := f.hub, f.hp, f.spoke, f.sp, f.parent, f.definition, f.target
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", sp.ID, cronHTTPUID(t))
	var current api.CronJob
	for _, op := range []string{"create", "replace", "archive", "restore"} {
		method, endpoint := http.MethodPut, path
		body := map[string]any{"actor": "local-author", "name": "Review", "definition": definition}
		if op != "create" {
			body["expected_event_uid"] = current.DefinitionEventUID
		}
		if op == "archive" || op == "restore" {
			delete(body, "name")
			delete(body, "definition")
			method = http.MethodDelete
		}
		if op == "restore" {
			method = http.MethodPost
			endpoint += "/restore"
		}
		response, raw := envDoRaw(t, target, method, endpoint, body, nil)
		require.Equalf(t, 200, response.StatusCode, "%s: %s", op, raw)
		var result api.CronJobResponse
		require.NoError(t, json.Unmarshal(raw, &result.Body))
		current = result.Body.Job
		require.Equal(t, "tester", current.Author)
		require.Equal(t, sp.ID, current.ProjectID)
		require.Zero(t, current.ID)
		require.Zero(t, current.Revision)
		require.Empty(t, result.Body.Events)
		_, err := spoke.DB.CronJob(t.Context(), sp.ID, current.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	_, err := hub.DB.IssueByUID(context.Background(), parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	binding, err := spoke.DB.FederationBindingByProject(t.Context(), sp.ID)
	require.NoError(t, err)
	require.NoError(t, federation.SyncFederationOnce(t.Context(), spoke.DB, binding, f.credential))
	local, err := spoke.DB.CronJob(t.Context(), sp.ID, current.UID)
	require.NoError(t, err)
	require.Equal(t, current.DefinitionEventUID, local.DefinitionEventUID)
	candidates, err := spoke.DB.ListRecurringCronJobs(t.Context(), "", 100)
	require.NoError(t, err)
	require.Empty(t, candidates)
	candidates, err = hub.DB.ListRecurringCronJobs(t.Context(), "", 100)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	delivered, err := hub.DB.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: hp.ID, JobUID: current.UID, ExpectedEventUID: current.DefinitionEventUID, Now: time.Now().Add(3 * time.Hour)})
	require.NoError(t, err)
	require.NotEmpty(t, delivered.Events)
}

func forwardingTestConfig(cfg daemon.ServerConfig) daemon.ServerConfig {
	cfg.FederationPushPending = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential) error {
		client, err := federation.NewClient(ctx, c.HubURL, c.Token, clientpkg.Opts{AllowInsecure: c.AllowInsecure, FederationSigning: c.Signing})
		if err != nil {
			return err
		}
		return federation.PushDirectSpoke(ctx, cfg.DB, b, c.HubProjectID, client)
	}
	return cfg
}

func (f *cronForwardFixture) put(t *testing.T, uid, expected string) (*http.Response, []byte) {
	t.Helper()
	return envDoRaw(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", f.sp.ID, url.PathEscape(uid)), map[string]any{"actor": "tester", "name": "Review", "definition": f.definition, "expected_event_uid": expected}, nil)
}

func TestCronSpokeForwardRefusals(t *testing.T) {
	for _, kind := range []string{"disabled", "push-disabled", "relay", "no-credentials", "credential-target", "credential-project", "unavailable", "old-hub", "revoked-enrollment", "push-failure", "changed-binding", "busy"} {
		t.Run(kind, func(t *testing.T) {
			var release func()
			f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
				b, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
				require.NoError(t, err)
				switch kind {
				case "disabled":
					b.Enabled = false
				case "push-disabled":
					b.PushEnabled = false
				case "relay":
					configureClaimRelayBinding(t, f.hub, f.spoke, f.sp)
				case "no-credentials":
					require.NoError(t, config.DefaultFederationCredentialStore().DeleteFederationCredential(t.Context(), f.sp.UID))
				case "credential-target":
					c := f.credential
					c.HubURL = "https://other.example"
					require.NoError(t, config.WriteFederationCredential(f.sp.UID, c))
				case "credential-project":
					c := f.credential
					c.HubProjectID++
					require.NoError(t, config.WriteFederationCredential(f.sp.UID, c))
				case "unavailable":
					cfg.FederationPushPending = nil
				case "revoked-enrollment":
					require.NoError(t, f.hub.DB.RevokeFederationEnrollment(t.Context(), f.enrollmentID))
				case "old-hub":
					endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						require.Equal(t, http.MethodGet, r.Method)
						_, _ = w.Write([]byte(`{}`))
					}))
					t.Cleanup(endpoint.Close)
					b.HubURL = endpoint.URL
					c := f.credential
					c.HubURL = endpoint.URL
					require.NoError(t, config.WriteFederationCredential(f.sp.UID, c))
				case "push-failure":
					cfg.FederationPushPending = func(context.Context, db.FederationBinding, config.FederationCredential) error {
						return errors.New("push failed")
					}
				case "changed-binding":
					cfg.FederationPushPending = func(ctx context.Context, b db.FederationBinding, _ config.FederationCredential) error {
						b.HubURL = "https://other.example"
						_, err := f.spoke.DB.UpsertFederationBinding(ctx, b)
						return err
					}
				case "busy":
					release, err = federationcoord.BeginRebind(t.Context(), federationcoord.Key(f.spoke.DB.InstanceUID(), f.sp.ID), f.spoke.DB, f.sp.ID)
					require.NoError(t, err)
				}
				if kind == "disabled" || kind == "push-disabled" || kind == "old-hub" {
					_, err = f.spoke.DB.UpsertFederationBinding(t.Context(), b)
					require.NoError(t, err)
				}
			})
			if release != nil {
				defer release()
			}
			response, raw := f.put(t, cronHTTPUID(t), "")
			want := 503
			if kind == "disabled" || kind == "push-disabled" || kind == "relay" || kind == "old-hub" {
				want = 409
			}
			if kind == "revoked-enrollment" {
				want = 403
			}
			require.Equalf(t, want, response.StatusCode, "%s", raw)
			require.NotContains(t, string(raw), f.credential.Token)
			jobs, err := f.hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: f.hp.ID})
			require.NoError(t, err)
			require.Empty(t, jobs)
			require.Zero(t, f.writes.Load(), "denial must send no outbound job mutation")
		})
	}
}

func TestCronSpokeForwardConflictAndUncertainResponse(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	f.definition.Issue.Body = strings.Repeat("body", 2000)
	uid := cronHTTPUID(t)
	response, raw := f.put(t, uid, "")
	require.Equalf(t, 200, response.StatusCode, "%s", raw)
	var created api.CronJobResponse
	require.NoError(t, json.Unmarshal(raw, &created.Body))
	authoritative, err := f.hub.DB.CronJob(t.Context(), f.hp.ID, uid)
	require.NoError(t, err)
	normalized := api.CronJobFrom(authoritative)
	normalized.ID = 0
	normalized.Revision = 0
	normalized.ProjectID = f.sp.ID
	require.Equal(t, normalized, created.Body.Job)

	response, raw = f.put(t, uid, "")
	require.Equalf(t, 409, response.StatusCode, "%s", raw)
	require.Greater(t, len(raw), 4096)
	var conflict struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
			Data struct {
				Current api.CronJob `json:"current"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &conflict))
	require.Equal(t, "cron_conflict", conflict.Error.Code)
	require.NotEmpty(t, conflict.Error.Hint)
	require.Equal(t, created.Body.Job.DefinitionEventUID, conflict.Error.Data.Current.DefinitionEventUID)
	require.Equal(t, f.definition.Issue.Body, conflict.Error.Data.Current.Definition.Issue.Body)
	require.Equal(t, f.sp.ID, conflict.Error.Data.Current.ProjectID)
	require.Zero(t, conflict.Error.Data.Current.ID)
	require.Zero(t, conflict.Error.Data.Current.Revision)
	require.Equal(t, normalized, conflict.Error.Data.Current)
	for _, mode := range []string{"redirect", "same-origin-redirect", "metadata-redirect", "malformed-success", "lost"} {
		t.Run(mode, func(t *testing.T) {
			var writes atomic.Int32
			trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { writes.Add(1) }))
			defer trap.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/trap" {
					writes.Add(1)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/metadata") {
					if mode == "metadata-redirect" {
						http.Redirect(w, r, "/trap", http.StatusTemporaryRedirect)
						return
					}
					_, _ = w.Write([]byte(`{"native_cron_writes":true}`))
					return
				}
				if mode == "malformed-success" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				if mode == "same-origin-redirect" {
					http.Redirect(w, r, "/trap", http.StatusTemporaryRedirect)
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, trap.URL, http.StatusTemporaryRedirect)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				require.NoError(t, err)
				_ = conn.Close()
			}))
			defer proxy.Close()
			b, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
			require.NoError(t, err)
			b.HubURL = proxy.URL
			_, err = f.spoke.DB.UpsertFederationBinding(t.Context(), b)
			require.NoError(t, err)
			c := f.credential
			c.HubURL = proxy.URL
			require.NoError(t, config.WriteFederationCredential(f.sp.UID, c))
			// No pending events remain; the shared push phase sends no request.
			response, raw := f.put(t, cronHTTPUID(t), "")
			require.Equal(t, 503, response.StatusCode)
			require.Contains(t, string(raw), "outcome may be unknown")
			require.Zero(t, writes.Load())
		})
	}
}

type cronForwardHost struct{ denied *atomic.Bool }

func (h cronForwardHost) Authorize(context.Context, daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	return daemon.HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error {
		if h.denied.Load() {
			return daemon.ErrHostAccessDenied
		}
		return nil
	}}, nil
}

func TestCronSpokeForwardPresendAuthority(t *testing.T) {
	for _, kind := range []string{"identity", "membership", "host"} {
		for _, operation := range []string{"create", "replace", "archive", "restore"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				var denied atomic.Bool
				f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
					token, _, err := f.spoke.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "incoming-token", Actor: "tester", AdminActor: "admin"})
					require.NoError(t, err)
					cfg.Auth = config.AuthConfig{Token: "owner-token", RequireTokenIdentity: true}
					if kind == "host" {
						cfg.Auth = config.AuthConfig{}
						cfg.HostAccess = cronForwardHost{&denied}
					}
					push := cfg.FederationPushPending
					cfg.FederationPushPending = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential) error {
						if err := push(ctx, b, c); err != nil {
							return err
						}
						switch kind {
						case "identity":
							_, _, err = f.spoke.DB.RevokeAPIToken(t.Context(), token.ID, "admin")
							return err
						case "membership":
							_, _, err = f.spoke.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: f.sp.UID, Visibility: "teams"}, "admin")
							return err
						default:
							denied.Store(true)
							return nil
						}
					}
				})
				method, path, body := http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", f.sp.ID), map[string]any{"actor": "tester", "name": "Review", "definition": f.definition}
				if operation == "create" {
					method = http.MethodPost
				} else {
					path += "/" + cronHTTPUID(t)
				}
				if operation == "archive" || operation == "restore" {
					method = http.MethodDelete
					delete(body, "name")
					delete(body, "definition")
					body["expected_event_uid"] = cronHTTPUID(t)
				}
				if operation == "restore" {
					method = http.MethodPost
					path += "/restore"
				}
				response, raw := envDoRaw(t, f.target, method, path, body, map[string]string{"Authorization": "Bearer incoming-token"})
				want := 404
				if kind == "identity" {
					want = 401
				}
				if kind == "host" {
					want = 404
				}
				require.Equalf(t, want, response.StatusCode, "%s", raw)
				jobs, err := f.hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: f.hp.ID})
				require.NoError(t, err)
				require.Empty(t, jobs)
				require.Zero(t, f.writes.Load(), "denial must send no outbound job mutation")
			})
		}
	}
}

// The state-machine contract is one authoritative definition and its original
// progress anchor, independent of which HTTP endpoint performs a lifecycle CAS.
func TestCronSpokeForwardCASProperty(t *testing.T)        { cronSpokeForwardSequences(t, "cas") }
func TestCronSpokeForwardAnchorProperty(t *testing.T)     { cronSpokeForwardSequences(t, "anchor") }
func TestCronSpokeForwardProvenanceProperty(t *testing.T) { cronSpokeForwardSequences(t, "provenance") }
func cronSpokeForwardSequences(t *testing.T, law string) {
	f := newCronForwardFixture(t, nil)
	hegel.Test(t, func(ht *hegel.T) {
		// Draw the complete byte-action domain; bound only the materialized sequence.
		actions := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, math.MaxUint8)))
		if len(actions) > 6 {
			actions = actions[:6]
		}
		uid := cronHTTPUID(t)
		response, raw := f.put(t, uid, "")
		require.Equalf(ht, 200, response.StatusCode, "%s", raw)
		var created api.CronJobResponse
		require.NoError(ht, json.Unmarshal(raw, &created.Body))
		current := created.Body.Job
		modelDeleted, modelName := false, "Review"
		modelDefinition := f.definition
		progress := func() string {
			parent, err := f.hub.DB.IssueByUID(t.Context(), f.parent.UID, db.IncludeDeletedNo)
			require.NoError(ht, err)
			var metadata map[string]jsontext.Value
			require.NoError(ht, json.Unmarshal([]byte(parent.Metadata), &metadata))
			return string(metadata["cron.progress."+uid])
		}
		anchor := progress()
		require.NotEmpty(ht, anchor)
		for _, step := range actions {
			target, projectID, headers := f.target, f.sp.ID, map[string]string(nil)
			if step&8 != 0 {
				target = f.hub
				projectID = f.hp.ID
			}
			method, path := http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", projectID, uid)
			expected := current.DefinitionEventUID
			stale := step&4 != 0
			if stale {
				expected = cronHTTPUID(t)
			}
			definition := f.definition
			definition.Enabled = step&64 == 0
			definition.Trigger.IntervalSeconds = 60 + int64(step)*60
			body := map[string]any{"actor": "tester", "name": fmt.Sprintf("Review %d", step), "definition": definition, "expected_event_uid": expected}
			want := 200
			deleted := modelDeleted
			switch step & 3 {
			case 0:
				delete(body, "expected_event_uid")
				want = 409
			case 1:
				if stale {
					want = 409
				}
			case 2:
				method = http.MethodDelete
				delete(body, "name")
				delete(body, "definition")
				if stale || deleted {
					want = 409
				}
			case 3:
				method = http.MethodPost
				path += "/restore"
				delete(body, "name")
				delete(body, "definition")
				if stale || !deleted {
					want = 409
				}
			}
			response, raw := envDoRaw(t, target, method, path, body, headers)
			require.Equalf(ht, want, response.StatusCode, "step=%d response=%s", step, raw)
			if want == 200 {
				var result api.CronJobResponse
				require.NoError(ht, json.Unmarshal(raw, &result.Body))
				current = result.Body.Job
				switch step & 3 {
				case 1:
					modelDeleted = false
					modelName = fmt.Sprintf("Review %d", step)
					modelDefinition = definition
				case 2:
					modelDeleted = true
				case 3:
					modelDeleted = false
				}
				require.Equal(ht, "tester", current.Author)
				if target == f.target {
					require.Zero(ht, current.ID)
					require.Zero(ht, current.Revision)
					require.Equal(ht, f.sp.ID, current.ProjectID)
					require.Empty(ht, result.Body.Events)
				}
			} else {
				var conflict api.ErrorEnvelope
				require.NoError(ht, json.Unmarshal(raw, &conflict))
				require.Equal(ht, "cron_conflict", conflict.Error.Code)
				encoded, err := json.Marshal(conflict.Error.Data["current"])
				require.NoError(ht, err)
				var actual api.CronJob
				require.NoError(ht, json.Unmarshal(encoded, &actual))
				require.Equal(ht, current.DefinitionEventUID, actual.DefinitionEventUID)
			}
			stored, err := f.hub.DB.CronJob(t.Context(), f.hp.ID, uid)
			require.NoError(ht, err)
			require.Equal(ht, current.DefinitionEventUID, stored.DefinitionEventUID)
			if law == "cas" {
				require.Equal(ht, modelDeleted, stored.DeletedAt != nil)
				require.Equal(ht, modelName, stored.Name)
				require.Equal(ht, modelDefinition, stored.Definition)
			}
			if law == "anchor" {
				require.Equal(ht, anchor, progress())
			}
			if step&16 != 0 {
				binding, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
				require.NoError(ht, err)
				require.NoError(ht, federation.SyncFederationOnce(t.Context(), f.spoke.DB, binding, f.credential))
				local, err := f.spoke.DB.CronJob(t.Context(), f.sp.ID, uid)
				require.NoError(ht, err)
				require.Equal(ht, current.DefinitionEventUID, local.DefinitionEventUID)
			}
		}
		jobs, err := f.hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: f.hp.ID, IncludeDeleted: true})
		require.NoError(ht, err)
		count := 0
		for _, job := range jobs {
			if job.UID == uid {
				count++
			}
		}
		require.Equal(ht, 1, count)
		if law != "provenance" {
			return
		}
		events, err := f.hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.hp.ID, Limit: 10000})
		require.NoError(ht, err)
		initializations := 0
		for _, event := range events {
			if strings.Contains(event.Payload, uid) {
				require.Equal(ht, f.hub.DB.InstanceUID(), event.OriginInstanceUID)
				require.Equal(ht, "tester", event.Actor)
				if event.Type == "issue.metadata_updated" && strings.Contains(event.Payload, "cron.progress."+uid) {
					initializations++
				}
			}
		}
		require.Equal(ht, 1, initializations)
		localEvents, err := f.spoke.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.sp.ID, Limit: 10000})
		require.NoError(ht, err)
		for _, event := range localEvents {
			if strings.Contains(event.Payload, uid) {
				require.Equal(ht, f.hub.DB.InstanceUID(), event.OriginInstanceUID, "no spoke-origin definition or progress event")
			}
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

type overlappingPushStore struct {
	db.Storage
	arrivals chan struct{}
	proceed  chan struct{}
	reads    atomic.Int32
}

func (s *overlappingPushStore) PendingFederationPushEvents(ctx context.Context, projectID int64, origin string, after int64, limit int) ([]db.Event, error) {
	events, err := s.Storage.PendingFederationPushEvents(ctx, projectID, origin, after, limit)
	if len(events) > 0 && s.reads.Add(1) <= 2 {
		s.arrivals <- struct{}{}
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return events, err
}

func TestCronSpokeForwardPushOnlyOverlap(t *testing.T) {
	f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
		enrollment := createClaimEnrollment(t, f.hub, f.hp.ID, f.spoke.DB.InstanceUID(), "push")
		f.credential.Token = enrollment.Token
		f.credential.Capabilities = "push"
		require.NoError(t, config.WriteFederationCredential(f.sp.UID, f.credential))
		cfg.FederationPushPending = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential) error {
			client, err := federation.NewClient(ctx, c.HubURL, c.Token, clientpkg.Opts{})
			if err != nil {
				return err
			}
			store := &overlappingPushStore{Storage: f.spoke.DB, arrivals: make(chan struct{}, 2), proceed: make(chan struct{})}
			results := make(chan error, 2)
			for range 2 {
				go func() { results <- federation.PushDirectSpoke(ctx, store, b, c.HubProjectID, client) }()
			}
			for range 2 {
				select {
				case <-store.arrivals:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			close(store.proceed)
			for range 2 {
				if err := <-results; err != nil {
					return err
				}
			}
			return nil
		}
	})
	last, err := f.spoke.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	response, raw := f.put(t, cronHTTPUID(t), "")
	require.Equalf(t, 200, response.StatusCode, "%s", raw)
	require.Equal(t, int32(1), f.writes.Load())
	require.Zero(t, f.polls.Load(), "push-only authoring must not poll")
	events, err := f.hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.hp.ID, IssueUID: f.parent.UID, Types: []string{"issue.created"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1, "overlapping ingest is idempotent")
	require.NoError(t, f.spoke.DB.AdvanceFederationPushCursor(t.Context(), f.sp.ID, last-1))
	binding, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
	require.NoError(t, err)
	require.Equal(t, last, binding.PushCursorEventID, "late acknowledgments must not regress the cursor")
}

// A native conversion request can conflict with a valid adapter definition.
// Its opaque options remain exact in the hub's authoritative current object.
func TestCronSpokeForwardAdapterConflictPreservesNumbers(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	hegel.Test(t, func(ht *hegel.T) {
		number := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		adapter := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspect"}, Overlap: "forbid", Catchup: "all", Options: jsontext.Value(fmt.Sprintf(`{"nested":[{"integer":%d}]}`, number))}
		uid := cronHTTPUID(t)
		current, _, err := f.hub.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.hp.ID, UID: uid, Name: "Adapter", Definition: adapter, Actor: "tester"})
		require.NoError(ht, err)
		response, raw := f.put(t, uid, "")
		require.Equalf(ht, 409, response.StatusCode, "%s", raw)
		var conflict struct {
			Error struct {
				Data struct {
					Current api.CronJob `json:"current"`
				} `json:"data"`
			} `json:"error"`
		}
		require.NoError(ht, json.Unmarshal(raw, &conflict))
		expected := api.CronJobFrom(current)
		expected.ID = 0
		expected.Revision = 0
		expected.ProjectID = f.sp.ID
		require.Equal(ht, expected, conflict.Error.Data.Current)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestCronSpokeForwardPOSTCreate(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	response, raw := envDoRaw(t, f.target, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", f.sp.ID), map[string]any{"actor": "local-author", "name": "Review", "definition": f.definition}, nil)
	require.Equalf(t, 201, response.StatusCode, "%s", raw)
	var result api.CronJobResponse
	require.NoError(t, json.Unmarshal(raw, &result.Body))
	require.NotEmpty(t, result.Body.Job.UID)
	require.Equal(t, "tester", result.Body.Job.Author)
	require.Equal(t, f.sp.ID, result.Body.Job.ProjectID)
	require.Zero(t, result.Body.Job.ID)
	require.Zero(t, result.Body.Job.Revision)
	require.Empty(t, result.Body.Events)
}

func TestCronSpokeForwardPreservesUIDPath(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	uid := cronHTTPUID(t)
	response, raw := f.put(t, uid, "")
	require.Equal(t, 200, response.StatusCode)
	var initial api.CronJobResponse
	require.NoError(t, json.Unmarshal(raw, &initial.Body))
	response, raw = f.put(t, uid+"?unexpected=query", initial.Body.Job.DefinitionEventUID)
	require.Equalf(t, 400, response.StatusCode, "%s", raw)
	current, err := f.hub.DB.CronJob(t.Context(), f.hp.ID, uid)
	require.NoError(t, err)
	require.Equal(t, initial.Body.Job.DefinitionEventUID, current.DefinitionEventUID)
}
