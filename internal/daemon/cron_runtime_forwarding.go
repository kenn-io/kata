package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
)

// cronForwardTransport binds the client and request destination to one credential
// snapshot. It deliberately requires neither a sync lock nor a pending push.
func cronForwardTransport(ctx context.Context, cfg ServerConfig, project db.Project, binding db.FederationBinding) (config.FederationCredential, *http.Client, string, error) {
	fail := func(message string) (config.FederationCredential, *http.Client, string, error) {
		return config.FederationCredential{}, nil, "", cronForwardUnavailable(message)
	}
	cred, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found || cred.Token == "" || cred.RelayEnrollmentPending || cred.LeavePending {
		return fail("federation push credentials are unavailable")
	}
	credentialOrigin, e1 := httpurl.CanonicalHTTPBaseURL(cred.HubURL)
	bindingOrigin, e2 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if e1 != nil || e2 != nil || credentialOrigin != bindingOrigin || cred.HubProjectID != binding.HubProjectID {
		return fail("federation credential target differs from the binding; reconnect before retrying")
	}
	cred = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, cred)
	if cfg.FederationHubClient == nil {
		return fail("federation push transport is unavailable")
	}
	client, err := cfg.FederationHubClient(ctx, cred)
	if err != nil || client == nil {
		return fail("federation transport is unavailable")
	}
	// Factories may share a client; never race a mutation of its redirect policy.
	owned := *client
	owned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return cred, &owned, bindingOrigin, nil
}

// forwardCronRuntime is best effort: local evidence has already been accepted.
// One immutable target snapshot is retained across preflight and dispatch. A
// dispatched beat can reach the old root after rebind; it expires there normally.
func forwardCronRuntime(ctx context.Context, cfg ServerConfig, projectID int64, id string, body map[string]jsontext.Value, enrollment bool) string {
	if enrollment {
		return "not_applicable"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return "not_applicable"
	}
	if err != nil {
		return "unavailable"
	}
	if binding.Role != db.FederationRoleSpoke || !binding.Enabled || !binding.PushEnabled || binding.RelayConfig != nil {
		return "not_applicable"
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return "unavailable"
	}
	cred, client, origin, err := cronForwardTransport(ctx, cfg, project, binding)
	if err != nil {
		return "unavailable"
	}
	base := fmt.Sprintf("%s/api/v1/projects/%d", origin, binding.HubProjectID)
	var metadata api.ProjectFederationBody
	if cronHubRequest(ctx, client, http.MethodGet, base+"/federation/metadata", nil, &metadata, projectID) != nil {
		return "unavailable"
	}
	if !metadata.CronRuntimeBeats {
		return "unsupported"
	}
	if metadata.ProjectUID != binding.HubProjectUID || metadata.ProjectUID != project.UID {
		return "unavailable"
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil || !current.Enabled || !current.PushEnabled || current.Role != binding.Role || current.RelayConfig != nil || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.HubProjectUID != binding.HubProjectUID || current.AllowInsecure != binding.AllowInsecure || current.Actor != binding.Actor {
		return "unavailable"
	}
	fresh, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found {
		return "unavailable"
	}
	freshOrigin, originErr := httpurl.CanonicalHTTPBaseURL(fresh.HubURL)
	if originErr != nil || freshOrigin != origin || fresh.HubProjectID != binding.HubProjectID {
		return "unavailable"
	}
	fresh = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, fresh)
	if !reflect.DeepEqual(fresh, cred) {
		return "unavailable"
	}
	if cronForwardAdmission(ctx, cfg.DB, project.ID, project.UID) != nil {
		return "unavailable"
	}
	// Forward admitted members without adding normalized defaults or an actor.
	// The root derives attribution from the enrollment. Compaction and member
	// removal keep the payload within the original ingress limit.
	body = maps.Clone(body)
	delete(body, "actor")
	if !slices.Contains(metadata.Features, cron.RuntimeChoicesFeature) {
		delete(body, "choices")
	}
	var out api.BeatCronRuntimeResponse
	if cronHubRequest(ctx, client, http.MethodPut, base+"/cron/runtimes/"+url.PathEscape(id), body, &out.Body, projectID) != nil || out.Body.Runtime.UID != id {
		return "unavailable"
	}
	return "recorded"
}

func cronRuntimeMetadataFeatures(binding db.FederationBinding) []string {
	if binding.Role == db.FederationRoleHub && binding.Enabled {
		return []string{cron.RuntimeChoicesFeature}
	}
	return nil
}
