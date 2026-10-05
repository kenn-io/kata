package federation

import (
	"context"
	"slices"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/httpurl"
)

// EmbeddingProducerConnected uses the existing enrollment connection immediately
// before local fill. A cached designation cannot authorize a disconnected relay.
func EmbeddingProducerConnected(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectUID string, recipe embedding.ArtifactIdentity, onPulled ...func(int64, []db.Event)) (bool, error) {
	return embeddingProducerConnected(ctx, store, credentials, projectUID, recipe, true, onPulled...)
}

// EmbeddingProducerAuthorized rechecks current authority after this fill has
// already reconciled artifacts. It does not repeat the full project sync.
func EmbeddingProducerAuthorized(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectUID string, recipe embedding.ArtifactIdentity) (bool, error) {
	return embeddingProducerConnected(ctx, store, credentials, projectUID, recipe, false)
}

func embeddingProducerConnected(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectUID string, recipe embedding.ArtifactIdentity, syncArtifacts bool, onPulled ...func(int64, []db.Event)) (bool, error) {
	project, err := store.ProjectByUID(ctx, projectUID)
	if err != nil || project.DeletedAt != nil {
		return false, ctx.Err()
	}
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil || binding.Role != db.FederationRoleSpoke || !binding.Enabled || !binding.PushEnabled || binding.RelayConfig == nil {
		return false, ctx.Err()
	}
	c := binding.RelayConfig
	if c.Validate(store.InstanceUID()) != nil || !c.ServeDownstream || c.UpstreamRevoked {
		return false, nil
	}
	selected, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	if err != nil || selected == nil || selected.ProducerInstanceUID != store.InstanceUID() {
		return false, nil
	}
	recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
	if selected.Recipe != recipe || credentials == nil {
		return false, nil
	}
	allowed, err := store.AccessibleProjectUIDs(ctx, c.LocalActor)
	if err != nil || !slices.Contains(allowed, projectUID) {
		return false, ctx.Err()
	}
	credential, found, err := credentials.FederationCredential(ctx, projectUID)
	if err != nil || !found || credential.Token == "" || credential.LeavePending || credential.RelayEnrollmentPending || credential.Actor != binding.Actor || credential.HubProjectID != binding.HubProjectID {
		return false, ctx.Err()
	}
	bindingOrigin, err := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if err != nil {
		return false, nil
	}
	credentialOrigin, err := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
	if err != nil || bindingOrigin != credentialOrigin {
		return false, nil
	}
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	upstream, err := NewClient(ctx, bindingOrigin, credential.Token, clientOptsForCredential(client.Opts{Timeout: client.DefaultHTTPTimeout}, credential))
	if err != nil {
		return false, ctx.Err()
	}
	metadata, err := upstream.ProjectFederation(ctx, binding.HubProjectID)
	if err != nil {
		return false, ctx.Err()
	}
	h := metadata.Relay
	if metadata.ProjectID != binding.HubProjectID || metadata.ProjectUID != projectUID || h == nil || h.ProtocolVersion != db.RelayProtocolVersion || h.BindingUID != c.BindingUID || h.UpstreamInstanceUID != c.UpstreamInstanceUID || h.Root.AuthorityUID != c.AuthorityUID || h.Root.ProjectUID != projectUID || h.ResetEpoch != c.ResetEpoch || h.ResetRequired || !slices.Equal(h.HubPath, c.HubPath) || h.EmbeddingProducer == nil || *h.EmbeddingProducer != *selected {
		return false, nil
	}
	pin, err := store.RootAuthority(ctx, projectUID)
	if err != nil {
		return false, ctx.Err()
	}
	if _, err := relayRootTransitionPath(pin, h.Root, h.RootKeyTransitions); err != nil {
		return false, nil
	}
	// Reconnect imports existing portable artifacts through the same coordinated
	// sync before the local worker considers paid fill. No separate download path.
	var events func(int64, []db.Event)
	if len(onPulled) > 0 {
		events = onPulled[0]
	}
	if syncArtifacts {
		if err := SyncFederationOnceWithPulledEvents(ctx, store, binding, credential, clientOptsForCredential(client.Opts{Timeout: client.DefaultHTTPTimeout}, credential), events); err != nil {
			return false, ctx.Err()
		}
	}
	// Check local policy and binding again after the authenticated network read.
	current, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil || !current.Enabled || !current.PushEnabled || current.RelayConfig == nil || current.RelayConfig.UpstreamRevoked || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.Actor != binding.Actor {
		return false, ctx.Err()
	}
	before, err := db.EncodeRelayBindingConfig(c)
	if err != nil {
		return false, err
	}
	after, err := db.EncodeRelayBindingConfig(current.RelayConfig)
	if err != nil {
		return false, err
	}
	if before == nil || after == nil || *before != *after {
		return false, nil
	}
	latest, err := store.ProjectByUID(ctx, projectUID)
	if err != nil {
		return false, ctx.Err()
	}
	latestProducer, err := db.ProjectEmbeddingProducerFromMetadata(latest.Metadata)
	if err != nil || latestProducer == nil || *latestProducer != *selected {
		return false, nil
	}
	allowed, err = store.AccessibleProjectUIDs(ctx, c.LocalActor)
	if err != nil || !slices.Contains(allowed, projectUID) {
		return false, ctx.Err()
	}
	return true, nil
}
