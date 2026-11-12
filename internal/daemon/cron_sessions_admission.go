package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/httpurl"
)

// admitCronSessions holds the federation admission until the local write ends.
func admitCronSessions(ctx context.Context, cfg ServerConfig, projectID int64) (func(), error) {
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && binding.Role != db.FederationRoleSpoke) {
		return func() {}, nil
	}
	if err != nil {
		return nil, internalAPIError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	release, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), projectID), cfg.DB, projectID)
	if err != nil {
		cancel()
		return nil, cronForwardUnavailable("federation endpoint is busy; retry shortly")
	}
	done := func() { release(); cancel() }
	fail := func(err error) (func(), error) { done(); return nil, err }
	binding, err = cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil || binding.Role != db.FederationRoleSpoke || !binding.Enabled || !binding.PushEnabled || binding.RelayConfig != nil {
		return fail(cronForwardUnavailable("federation binding is unavailable"))
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return fail(err)
	}
	cred, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found || cred.Token == "" || cred.RelayEnrollmentPending || cred.LeavePending {
		return fail(cronForwardUnavailable("federation push credentials are unavailable"))
	}
	credentialOrigin, e1 := httpurl.CanonicalHTTPBaseURL(cred.HubURL)
	bindingOrigin, e2 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if e1 != nil || e2 != nil || credentialOrigin != bindingOrigin || cred.HubProjectID != binding.HubProjectID {
		return fail(cronForwardUnavailable("federation credential target differs from the binding; reconnect before retrying"))
	}
	if cfg.FederationHubClient == nil {
		return fail(cronForwardUnavailable("federation transport is unavailable"))
	}
	cred = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, cred)
	client, err := cfg.FederationHubClient(ctx, cred)
	if err != nil {
		return fail(cronForwardUnavailable("federation transport is unavailable"))
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/metadata", bindingOrigin, binding.HubProjectID), nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	resp, err := client.Do(req)
	if err != nil {
		return fail(cronForwardUnavailable("hub metadata is unavailable"))
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(cronForwardUnavailable("hub metadata is unavailable"))
	}
	if err := db.RequireEventFeatures(resp.Header.Get(db.EventFeaturesHeader), db.CronRunSessionsEventFeature); err != nil {
		return fail(federationFeatureError(err))
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil || !current.Enabled || !current.PushEnabled || current.Role != binding.Role || current.RelayConfig != nil || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.HubProjectUID != binding.HubProjectUID || current.AllowInsecure != binding.AllowInsecure {
		return fail(cronForwardUnavailable("federation binding changed; retry with fresh credentials"))
	}
	return done, nil
}
