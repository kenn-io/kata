package daemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationsigning"
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
		if errors.Is(err, federationcoord.ErrBusy) {
			return ctx, func() {}, api.NewError(http.StatusServiceUnavailable, "federation_busy", "federation endpoint is busy; retry shortly", "retry this observation when the federation endpoint is available", nil)
		}
		return ctx, func() {}, internalAPIError(err)
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
	client, _, origin, err := resolveCronHubTransport(ctx, cfg, project.UID, binding)
	if err != nil {
		return fail()
	}
	apiClient, err := generated.NewDefaultClient(origin, runtime.WithHTTPClient(boundedProbeDoer{
		client: client,
		limit:  federationsigning.MaxBodyBytes + 1,
	}))
	if err != nil {
		return fail()
	}
	features := db.SupportedEventFeatures
	response, err := apiClient.GetFederationProjectMetadataWithResponse(ctx, &generated.GetFederationProjectMetadataRequestOptions{
		PathParams: &generated.GetFederationProjectMetadataPath{ProjectID: binding.HubProjectID},
		Header:     &generated.GetFederationProjectMetadataHeaders{XKataEventFeatures: &features},
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Content-Type", "application/json")
		return nil
	})
	if err != nil || response == nil || response.HTTPResponse == nil || response.StatusCode != http.StatusOK || len(response.Body) == 0 || len(response.Body) > federationsigning.MaxBodyBytes ||
		db.RequireEventFeatures(response.HTTPResponse.Header.Get(db.EventFeaturesHeader), db.CronRunSummaryV2Feature) != nil {
		return fail()
	}
	return db.WithCronSummaryBinding(ctx, &binding), cleanup, nil
}
