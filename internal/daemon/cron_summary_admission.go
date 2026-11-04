package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	fail := func(err error) (context.Context, func(), error) { cleanup(); return ctx, func() {}, err }
	binding, err = cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fail(internalAPIError(err))
	}
	if !binding.Enabled || !binding.PushEnabled || binding.Role != db.FederationRoleSpoke {
		return fail(cronSummaryUnsupported())
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return fail(err)
	}
	client, _, origin, err := resolveCronHubTransport(ctx, cfg, project.UID, binding)
	if err != nil {
		return fail(cronSummaryUnsupported())
	}
	if !cronHubAdvertisesSummary(ctx, client, origin, binding.HubProjectID) {
		return fail(cronSummaryUnsupported())
	}
	return db.WithCronSummaryBinding(ctx, &binding), cleanup, nil
}

// cronHubAdvertisesSummary probes hub metadata over the pinned, redirect-refusing
// client with the gate the hand-rolled request used: any 2xx whose body is valid
// JSON within the signed-body bound and advertises cron_run_summary_v2. Typed
// decoding is ignored, and truncation alone could accept padded JSON.
func cronHubAdvertisesSummary(ctx context.Context, client *http.Client, origin string, hubProjectID int64) bool {
	apiClient, err := generated.NewDefaultClient(origin, runtime.WithHTTPClient(boundedProbeDoer{client: client, limit: federationsigning.MaxBodyBytes + 1}))
	if err != nil {
		return false
	}
	features := db.SupportedEventFeatures
	result, _ := apiClient.GetFederationProjectMetadataWithResponse(ctx, &generated.GetFederationProjectMetadataRequestOptions{PathParams: &generated.GetFederationProjectMetadataPath{ProjectID: hubProjectID}, Header: &generated.GetFederationProjectMetadataHeaders{XKataEventFeatures: &features}})
	return result != nil && result.StatusCode >= 200 && result.StatusCode < 300 && len(result.Body) <= federationsigning.MaxBodyBytes &&
		json.Unmarshal(result.Body, new(jsontext.Value)) == nil &&
		db.RequireEventFeatures(result.HTTPResponse.Header.Get(db.EventFeaturesHeader), db.CronRunSummaryV2Feature) == nil
}
