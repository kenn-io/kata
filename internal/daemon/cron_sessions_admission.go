package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// admitCronSessions holds the federation admission until the local write ends.
func admitCronSessions(ctx context.Context, cfg ServerConfig, projectID int64) (func(), error) {
	release, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), projectID), cfg.DB, projectID)
	if err != nil {
		return nil, cronForwardUnavailable("federation endpoint is busy; retry shortly")
	}
	fail := func(err error) (func(), error) { release(); return nil, err }
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && binding.Role != db.FederationRoleSpoke) {
		return release, nil
	}
	if err != nil {
		return fail(cronForwardUnavailable("federation binding is unavailable"))
	}
	if !binding.Enabled || !binding.PushEnabled || binding.RelayConfig != nil {
		return fail(api.NewError(409, "federated_read_only", "run sessions cannot be recorded on this spoke: its federation binding is disabled, has push turned off, or uses a relay, so Kata cannot confirm the hub supports them", "Record the run without sessions, or run the command against the project's owning hub.", nil))
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _, client, bindingOrigin, err := cronFederationTransport(probeCtx, cfg, projectID, binding)
	if err != nil {
		return fail(err)
	}
	metadata, features, err := readFederationHubMetadata(probeCtx, client, bindingOrigin, binding.HubProjectID)
	if err != nil || metadata.ProjectID != binding.HubProjectID || metadata.ProjectUID != binding.HubProjectUID {
		return fail(cronForwardUnavailable("hub metadata is unavailable"))
	}
	if err := db.RequireEventFeatures(features, db.CronRunSessionsEventFeature); err != nil {
		return fail(federationFeatureError(err))
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil || !current.Enabled || !current.PushEnabled || current.Role != binding.Role || current.RelayConfig != nil || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.HubProjectUID != binding.HubProjectUID || current.AllowInsecure != binding.AllowInsecure {
		return fail(cronForwardUnavailable("federation binding changed; retry with fresh credentials"))
	}
	return release, nil
}
