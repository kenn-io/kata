package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/httpurl"
)

func cronSummaryUnsupported() error {
	return api.NewError(http.StatusConflict, "unsupported_event_features", "upstream support for cron_run_summary_v2 could not be confirmed", "send the summary without v2 members", nil)
}

// admitCronSummary holds endpoint coordination until the observation commits.
// Support is deliberately uncached: offline or downgraded hubs fail closed.
func admitCronSummary(ctx context.Context, cfg ServerConfig, projectID int64) (context.Context, func(), error) {
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return db.WithCronSummaryBinding(ctx, nil), func() {}, nil
	}
	if err != nil {
		return ctx, func() {}, internalAPIError(err)
	}
	if !binding.Enabled || binding.Role != db.FederationRoleSpoke {
		return db.WithCronSummaryBinding(ctx, &binding), func() {}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	release, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), projectID), cfg.DB, projectID)
	if err != nil {
		cancel()
		return ctx, func() {}, cronSummaryUnsupported()
	}
	cleanup := func() { release(); cancel() }
	fail := func() (context.Context, func(), error) { cleanup(); return ctx, func() {}, cronSummaryUnsupported() }
	binding, err = cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil || !binding.Enabled || !binding.PushEnabled || binding.Role != db.FederationRoleSpoke {
		return fail()
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		cleanup()
		return ctx, func() {}, err
	}
	cred, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found || cred.Token == "" || cred.LeavePending || cred.RelayEnrollmentPending {
		return fail()
	}
	origin, e1 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	credentialOrigin, e2 := httpurl.CanonicalHTTPBaseURL(cred.HubURL)
	if e1 != nil || e2 != nil || origin != credentialOrigin || cred.HubProjectID != binding.HubProjectID || cfg.FederationHubClient == nil {
		return fail()
	}
	cred = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, cred)
	client, err := cfg.FederationHubClient(ctx, cred)
	if err != nil {
		return fail()
	}
	// Do not mutate a client that another request may share.
	localClient := *client
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var meta api.ProjectFederationBody
	headers, err := cronHubRequestHeaders(ctx, &localClient, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/metadata", origin, binding.HubProjectID), nil, &meta, projectID)
	if err != nil || db.RequireEventFeatures(headers.Get(db.EventFeaturesHeader), db.CronRunSummaryV2Feature) != nil {
		return fail()
	}
	return db.WithCronSummaryBinding(ctx, &binding), cleanup, nil
}
