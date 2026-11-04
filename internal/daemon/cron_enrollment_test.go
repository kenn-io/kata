package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func nativeCronEnrollmentFixture(t *testing.T, options ...testenv.Option) (*testenv.Env, db.Project, db.Issue, cron.JobDefinition) {
	t.Helper()
	env := testenv.New(t, options...)
	project := seedProject(t, env, "hub-project")
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review service"}, Overlap: "allow", Catchup: "all"}
	return env, project, parent, definition
}

// The push enrollment must reach the real job transaction and retain the
// original progress anchor through replacement, archive, and restore.
func TestCronPushEnrollmentLifecycle(t *testing.T) {
	env, project, parent, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("owner-token"))
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	headers := map[string]string{"Authorization": "Bearer " + enrollment.Token}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
	var current db.CronJob
	var anchor string
	for _, operation := range []string{"create", "replace", "archive", "restore"} {
		method, target := http.MethodPost, path
		body := map[string]any{"actor": "tester", "name": "Review service", "definition": definition}
		if operation != "create" {
			target += "/" + current.UID
			body["expected_event_uid"] = current.DefinitionEventUID
		}
		switch operation {
		case "replace":
			method = http.MethodPut
			body["actor"] = ""
			body["name"] = "Updated review"
		case "archive":
			method = http.MethodDelete
		case "restore":
			target += "/restore"
		}
		if operation == "archive" || operation == "restore" {
			delete(body, "name")
			delete(body, "definition")
		}
		response, raw := envDoRaw(t, env, method, target, body, headers)
		want := http.StatusOK
		if operation == "create" {
			want = http.StatusCreated
		}
		require.Equalf(t, want, response.StatusCode, "%s: %s", operation, raw)
		var result struct {
			Job    db.CronJob `json:"job"`
			Events []db.Event `json:"events"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		current = result.Job
		require.Equal(t, "tester", current.Author)
		require.NotEmpty(t, result.Events)
		for _, event := range result.Events {
			require.Equal(t, "tester", event.Actor)
			require.Equal(t, env.DB.InstanceUID(), event.OriginInstanceUID)
		}
		updated, err := env.DB.IssueByUID(t.Context(), parent.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		var metadata map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(updated.Metadata), &metadata))
		require.Len(t, metadata, 1)
		progress := string(metadata["cron.progress."+current.UID])
		require.NotEmpty(t, progress)
		if anchor == "" {
			anchor = progress
		} else {
			require.JSONEq(t, anchor, progress, "lifecycle must not reset progress")
		}
	}
	jobs, err := env.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID, IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, jobs, 1)
}

// Cron accepts direct scoped push authority only. Every rejection must leave
// the project history untouched, including on an otherwise keyless hub.
func TestCronEnrollmentWriteBoundaries(t *testing.T) {
	for _, kind := range []string{"missing-push", "revoked", "membership", "wrong-project", "global", "actor-mismatch", "relay", "adapter", "workflow", "disabled-hub", "spoke", "invalid-keyless", "validate"} {
		t.Run(kind, func(t *testing.T) {
			options := []testenv.Option{testenv.WithAuthToken("owner-token")}
			if kind == "invalid-keyless" {
				options = nil
			}
			env, project, _, definition := nativeCronEnrollmentFixture(t, options...)
			caps := "push"
			if kind == "missing-push" {
				caps = "pull,claim"
			}
			enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), caps)
			token := enrollment.Token
			actor := "tester"
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
			var document any = definition
			switch kind {
			case "validate":
				path += "/validate"
			case "membership":
				_, _, err := env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams"}, "admin")
				require.NoError(t, err)
			case "revoked":
				require.NoError(t, env.DB.RevokeFederationEnrollment(t.Context(), enrollment.Enrollment.ID))
			case "wrong-project":
				// A federation-enabled target keeps the rejection on
				// cross-project scoping alone. Without the enabled hub binding,
				// AuthorizeFederationToken would reject for the missing binding
				// and mask a scoping regression.
				other := seedProject(t, env, "other-project")
				otherParent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Other parent", Author: "tester"})
				require.NoError(t, err)
				_, err = env.DB.EnableProjectFederation(t.Context(), other.ID, "tester")
				require.NoError(t, err)
				path = fmt.Sprintf("/api/v1/projects/%d/cron/jobs", other.ID)
				otherDefinition := definition
				otherDefinition.Issue.ParentUID = otherParent.UID
				document = otherDefinition
			case "global":
				grant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "global-push-token", SpokeInstanceUID: cronHTTPUID(t), Capabilities: "push", Actor: "tester"})
				require.NoError(t, err)
				token = grant.Token
			case "actor-mismatch":
				actor = "someone-else"
			case "relay":
				public, _, err := ed25519.GenerateKey(nil)
				require.NoError(t, err)
				require.NoError(t, env.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				parent, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "relay-parent", Actor: "tester", AdminActor: "admin"})
				require.NoError(t, err)
				grant, err := env.DB.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: cronHTTPUID(t), ProtocolVersion: db.RelayProtocolVersion})
				require.NoError(t, err)
				token = grant.Token
			case "adapter":
				document = cronHTTPDefinition(t)
			case "workflow":
				path = fmt.Sprintf("/api/v1/projects/%d/cron/workflows", project.ID)
				document = cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}
			case "disabled-hub", "spoke":
				binding, err := env.DB.FederationBindingByProject(t.Context(), project.ID)
				require.NoError(t, err)
				if kind == "disabled-hub" {
					binding.Enabled = false
				} else {
					binding.Role = db.FederationRoleSpoke
					binding.HubURL = "https://daemon.example"
					binding.HubProjectID = 42
					binding.HubProjectUID = project.UID
				}
				_, err = env.DB.UpsertFederationBinding(t.Context(), binding)
				require.NoError(t, err)
			case "invalid-keyless":
				token = "invalid-enrollment"
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": actor, "name": "Review", "definition": document}, map[string]string{"Authorization": "Bearer " + token})
			want := http.StatusForbidden
			if kind == "membership" {
				want = http.StatusNotFound
			}
			require.Equalf(t, want, response.StatusCode, "%s", raw)
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestCronEnrollmentMetadataCapability(t *testing.T) {
	for _, kind := range []string{"enabled-hub", "disabled-hub", "spoke"} {
		t.Run(kind, func(t *testing.T) {
			env, project, _, _ := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("owner-token"))
			if kind != "enabled-hub" {
				binding, err := env.DB.FederationBindingByProject(t.Context(), project.ID)
				require.NoError(t, err)
				if kind == "disabled-hub" {
					binding.Enabled = false
				} else {
					binding.Role = db.FederationRoleSpoke
					binding.HubURL = "https://daemon.example"
					binding.HubProjectID = 42
					binding.HubProjectUID = project.UID
				}
				_, err = env.DB.UpsertFederationBinding(t.Context(), binding)
				require.NoError(t, err)
			}
			enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
			response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/metadata", project.ID), nil, map[string]string{"Authorization": "Bearer " + enrollment.Token})
			if kind != "enabled-hub" {
				require.Equalf(t, http.StatusForbidden, response.StatusCode, "%s", raw)
				response, raw = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation", project.ID), nil, map[string]string{"Authorization": "Bearer owner-token"})
			}
			require.Equalf(t, http.StatusOK, response.StatusCode, "%s", raw)
			var metadata struct {
				NativeCronWrites bool `json:"native_cron_writes"`
			}
			require.NoError(t, json.Unmarshal(raw, &metadata))
			require.Equal(t, kind == "enabled-hub", metadata.NativeCronWrites)
		})
	}
}

type beforeCronJobStore struct {
	db.Storage
	before func() error
}

func (s beforeCronJobStore) PutCronJob(ctx context.Context, in db.PutCronJob) (db.CronJob, []db.Event, error) {
	if err := s.before(); err != nil {
		return db.CronJob{}, nil, err
	}
	return s.Storage.PutCronJob(ctx, in)
}

// Authentication before dispatch cannot outlive a revoked credential or
// removed project membership by the time the job transaction commits.
func TestCronJobWriteRevalidatesAuthority(t *testing.T) {
	for _, kind := range []string{"enrollment", "identity", "membership"} {
		t.Run(kind, func(t *testing.T) {
			env, project, _, definition := nativeCronEnrollmentFixture(t)
			enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
			actorToken, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "actor-token", Actor: "tester", AdminActor: "admin"})
			require.NoError(t, err)
			token := enrollment.Token
			if kind == "identity" {
				token = "actor-token"
			}
			store := beforeCronJobStore{Storage: env.DB, before: func() error {
				switch kind {
				case "enrollment":
					return env.DB.RevokeFederationEnrollment(t.Context(), enrollment.Enrollment.ID)
				case "identity":
					_, _, err := env.DB.RevokeAPIToken(t.Context(), actorToken.ID, "admin")
					return err
				default:
					_, _, err := env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams"}, "admin")
					return err
				}
			}}
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "owner-token", RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			endpoint := httptest.NewServer(server.Handler())
			t.Cleanup(endpoint.Close)
			target := &testenv.Env{URL: endpoint.URL, HTTP: endpoint.Client()}
			response, raw := envDoRaw(t, target, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID), map[string]any{"actor": "tester", "name": "Review", "definition": definition}, map[string]string{"Authorization": "Bearer " + token})
			want := http.StatusForbidden
			if kind == "identity" {
				want = http.StatusUnauthorized
			}
			require.Equalf(t, want, response.StatusCode, "%s", raw)
			jobs, err := env.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID})
			require.NoError(t, err)
			require.Empty(t, jobs)
		})
	}
}

func TestCronKeylessOrdinaryWrite(t *testing.T) {
	env, project, _, definition := nativeCronEnrollmentFixture(t)
	response, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID), map[string]any{"actor": "tester", "name": "Review", "definition": definition}, nil)
	require.Equalf(t, http.StatusCreated, response.StatusCode, "%s", raw)
}

// Push enrollment manages native recurring jobs only. An adapter job must
// never be convertible in either direction: replacing it with a native
// definition would irreversibly discard the adapter configuration, and
// replacing a native job with an adapter definition would abandon native
// ownership. Every rejection must leave the definition and history untouched.
func TestCronEnrollmentCannotConvertAdapterJob(t *testing.T) {
	for _, kind := range []string{"adapter-target", "adapter-submission"} {
		t.Run(kind, func(t *testing.T) {
			env, project, _, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("owner-token"))
			enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
			headers := map[string]string{"Authorization": "Bearer " + enrollment.Token}
			owner := map[string]string{"Authorization": "Bearer owner-token"}
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
			if kind == "adapter-target" {
				response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "name": "Inspection", "definition": cronHTTPDefinition(t)}, owner)
				require.Equalf(t, http.StatusCreated, response.StatusCode, "%s", raw)
			} else {
				response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "name": "Review service", "definition": definition}, headers)
				require.Equalf(t, http.StatusCreated, response.StatusCode, "%s", raw)
			}
			jobs, err := env.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID})
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			current := jobs[0]
			// adapter-target: the enrollment submits a native definition over an
			// adapter job. adapter-submission: the enrollment submits an adapter
			// definition over a native job.
			submission := definition
			if kind == "adapter-submission" {
				submission = cronHTTPDefinition(t)
			}
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			body := map[string]any{"actor": "tester", "name": current.Name, "definition": submission, "expected_event_uid": current.DefinitionEventUID}
			response, raw := envDoRaw(t, env, http.MethodPut, path+"/"+current.UID, body, headers)
			// adapter-target must fail authorization; adapter-submission already
			// fails validation, because native ownership is immutable in storage.
			want := http.StatusForbidden
			if kind == "adapter-submission" {
				want = http.StatusBadRequest
			}
			require.Equalf(t, want, response.StatusCode, "%s", raw)
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "rejected conversion must not change history")
			kept, err := env.DB.CronJob(t.Context(), project.ID, current.UID)
			require.NoError(t, err)
			require.Equal(t, current.DefinitionEventUID, kept.DefinitionEventUID)
			require.Equal(t, current.Definition.Action.Kind, kept.Definition.Action.Kind)
			require.Equal(t, string(current.Definition.Options), string(kept.Definition.Options))
		})
	}
}

// Cron routes resolve credentials inside the handler, after the root
// attribution middleware has already run. Remote enrollment push writes and
// static-token writes (forwarding headers mark the request as proxied, so it
// is not owner-local) must still commit root receipts, or the hub loses
// certified acceptance for its cron history.
func TestCronEnrollmentWriteRootReceipts(t *testing.T) {
	for _, kind := range []string{"enrollment", "static-token"} {
		t.Run(kind, func(t *testing.T) {
			public, key, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			env, project, _, definition := nativeCronEnrollmentFixture(t,
				testenv.WithAuthToken("owner-token"), testenv.WithRootSigning(key))
			headers := map[string]string{"Authorization": "Bearer owner-token", "X-Forwarded-For": "203.0.113.10"}
			if kind == "enrollment" {
				enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
				headers = map[string]string{"Authorization": "Bearer " + enrollment.Token, "X-Forwarded-For": "203.0.113.10"}
			}
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID)
			response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "name": "Review service", "definition": definition}, headers)
			require.Equalf(t, http.StatusCreated, response.StatusCode, "%s", raw)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			receipts, err := env.DB.AttributionReceiptsAfter(t.Context(), project.UID, 1, 0, 100)
			require.NoError(t, err)
			require.NotEmpty(t, receipts, "cron write committed without a root receipt")
			events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Types: []string{"cron.job.created"}, Limit: 10})
			require.NoError(t, err)
			require.NotEmpty(t, events)
			eventUID := events[0].UID
			matched := false
			for _, receipt := range receipts {
				if receipt.EventUID != eventUID {
					continue
				}
				matched = true
				require.NoError(t, db.VerifyRootReceipt(pin, receipt))
				require.Equal(t, "tester", receipt.AccountableActor)
				require.Equal(t, "tester", receipt.SourceActor)
			}
			require.True(t, matched, "no receipt for the cron definition event")
		})
	}
}
