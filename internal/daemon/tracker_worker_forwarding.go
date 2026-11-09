package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/tracker"
)

// Reverse worker traffic follows the existing exact project enrollment origin.
// It does not hold the event-sync gate while waiting on a root call: a forwarded
// manual run can itself be waiting for this owner to poll. Pins/live routes are
// rechecked before every short request; native ingress fences handle revocation.
func forwardTrackerWorker(ctx context.Context, cfg ServerConfig, project db.Project, body trackerWorkerBody, action string) (trackerWorkerResponseBody, error) {
	empty := trackerWorkerResponseBody{}
	binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil || binding.Role != db.FederationRoleSpoke || !binding.Enabled || binding.RelayConfig == nil {
		return empty, errTrackerRelayUnavailable
	}
	route := binding.RelayConfig
	if route.Validate(cfg.DB.InstanceUID()) != nil || route.UpstreamRevoked || route.AuthorityUID != body.Presence.RootInstanceUID || project.UID != body.Presence.ProjectUID || len(body.Presence.Path) >= tracker.MaxControlHops || slices.ContainsFunc(body.Presence.Path, func(h tracker.RelayHop) bool { return h.InstanceUID == cfg.DB.InstanceUID() }) || (action != "poll" && !binding.PushEnabled) {
		return empty, errTrackerRelayUnavailable
	}
	credential, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found || credential.Token == "" || credential.Signing == nil || credential.RelayEnrollmentPending || credential.LeavePending || cfg.FederationHubClient == nil {
		return empty, errTrackerRelayUnavailable
	}
	origin, e1 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	credentialOrigin, e2 := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
	if e1 != nil || e2 != nil || origin != credentialOrigin || credential.HubProjectID != binding.HubProjectID {
		return empty, errTrackerRelayUnavailable
	}
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	client, err := cfg.FederationHubClient(ctx, credential)
	if err != nil || client == nil {
		return empty, errTrackerRelayUnavailable
	}
	pinnedClient := *client
	pinnedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := fmt.Sprintf("%s/api/v1/projects/%d", origin, binding.HubProjectID)
	var meta api.ProjectFederationBody
	if err := trackerHubRequest(ctx, &pinnedClient, http.MethodGet, base+"/federation/metadata", nil, &meta); err != nil {
		return empty, err
	}
	pin, err := cfg.DB.RootAuthority(ctx, project.UID)
	if err != nil || meta.TrackerWorkerVersion != tracker.WorkerProtocolVersion || meta.ProjectUID != project.UID || meta.ProjectID != binding.HubProjectID || meta.Relay == nil || meta.Relay.ProtocolVersion != db.RelayProtocolVersion || meta.Relay.UpstreamInstanceUID != route.UpstreamInstanceUID || meta.Relay.BindingUID != route.BindingUID || meta.Relay.ResetEpoch != route.ResetEpoch || meta.Relay.ResetRequired || !slices.Equal(meta.Relay.HubPath, route.HubPath) || meta.Relay.Root.AuthorityUID != route.AuthorityUID || meta.Relay.Root.ProjectUID != project.UID || meta.Relay.Root.KeyID != pin.KeyID || !bytes.Equal(meta.Relay.Root.PublicKey, pin.PublicKey) {
		return empty, errTrackerRelayUnavailable
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil || current.Role != binding.Role || !current.Enabled || current.PushEnabled != binding.PushEnabled || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.HubProjectUID != binding.HubProjectUID || current.AllowInsecure != binding.AllowInsecure || !reflect.DeepEqual(current.RelayConfig, route) {
		return empty, errTrackerRelayUnavailable
	}
	if err := cronForwardAdmission(ctx, cfg.DB, project.ID, project.UID); err != nil {
		return empty, tracker.ErrDenied
	}
	body.Presence.Path = append(slices.Clone(body.Presence.Path), tracker.RelayHop{InstanceUID: cfg.DB.InstanceUID(), BindingUID: route.BindingUID, ResetEpoch: route.ResetEpoch})
	limit := 64 << 10
	if action == "chunk" {
		limit = 1 << 20
	}
	var result trackerWorkerResponseBody
	if err := trackerHubRequestBounded(ctx, &pinnedClient, http.MethodPost, base+"/federation/tracker/worker/"+action, body, &result, limit); err != nil {
		return empty, err
	}
	if result.RootInstanceUID != route.AuthorityUID || result.ProjectUID != project.UID || len(result.Commands) > 8 {
		return empty, errTrackerRelayUnavailable
	}
	for _, command := range result.Commands {
		t := command.Ticket.Ticket
		if t.RootInstanceUID != route.AuthorityUID || t.ProjectUID != project.UID || t.Selection.OwnerInstanceUID != body.Presence.OwnerInstanceUID {
			return empty, errTrackerRelayUnavailable
		}
	}
	return result, nil
}
