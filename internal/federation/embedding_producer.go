package federation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/httpurl"
)

// EmbeddingProducerConnected probes the existing enrollment connection once
// immediately before local fill. A cached designation cannot authorize a
// disconnected relay, and a hub that still holds artifact offers for this
// relay defers paid fill until the federation runner has synced them. The
// probe never synchronizes the project itself.
func EmbeddingProducerConnected(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectUID string, recipe embedding.ArtifactIdentity) (bool, error) {
	return embeddingProducerConnected(ctx, store, credentials, projectUID, recipe, true)
}

// EmbeddingProducerAuthorized rechecks current authority after this fill has
// already reconciled artifacts. Offers still pending upstream do not defer it.
func EmbeddingProducerAuthorized(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectUID string, recipe embedding.ArtifactIdentity) (bool, error) {
	return embeddingProducerConnected(ctx, store, credentials, projectUID, recipe, false)
}

// EmbeddingProducerPendingArtifactWait bounds how long a selected relay defers
// local fill while its hub still holds artifact offers for it. The runner
// normally delivers offers within a few sync turns; an offer that stays
// pending this long, for example because the hub cannot serve its bytes, must
// not keep the relay from embedding locally.
const EmbeddingProducerPendingArtifactWait = 10 * time.Minute

// producerNow is the clock behind the pending-offer wait.
var producerNow = time.Now

// pendingArtifactWaits records when this process first saw each relay
// binding's hub report pending artifact offers. The outbox keeps no offer
// timestamps, so the wait runs from the relay's first observation and
// restarts with the process.
var pendingArtifactWaits = struct {
	sync.Mutex
	since map[string]time.Time
}{since: map[string]time.Time{}}

// pendingArtifactWaitExceeded records the current pending state for a binding
// and reports whether offers have stayed pending past the wait.
func pendingArtifactWaitExceeded(bindingUID string, pending bool) bool {
	pendingArtifactWaits.Lock()
	defer pendingArtifactWaits.Unlock()
	if !pending {
		delete(pendingArtifactWaits.since, bindingUID)
		return false
	}
	now := producerNow()
	first, ok := pendingArtifactWaits.since[bindingUID]
	if !ok {
		pendingArtifactWaits.since[bindingUID] = now
		return false
	}
	return now.Sub(first) >= EmbeddingProducerPendingArtifactWait
}

// clearPendingArtifactWait ends the wait for a binding once its upstream
// offers are drained. Sync calls it when an artifact pull finds nothing more
// to offer, which also covers the case where reuse then fills every pending
// document and eligibility never runs again before the next episode.
func clearPendingArtifactWait(bindingUID string) {
	pendingArtifactWaits.Lock()
	defer pendingArtifactWaits.Unlock()
	delete(pendingArtifactWaits.since, bindingUID)
}

// producerUnavailable reports an operational failure, as opposed to a policy
// decision, so the worker can log it and status can show it. Cancellation
// still wins.
func producerUnavailable(ctx context.Context, stage string, err error) (bool, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if err == nil {
		return false, fmt.Errorf("%w: %s", db.ErrEmbeddingProducerUnavailable, stage)
	}
	return false, fmt.Errorf("%w: %s: %w", db.ErrEmbeddingProducerUnavailable, stage, err)
}

func embeddingProducerConnected(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectUID string, recipe embedding.ArtifactIdentity, requireSynced bool) (bool, error) {
	project, err := store.ProjectByUID(ctx, projectUID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && project.DeletedAt != nil) {
		return false, ctx.Err()
	}
	if err != nil {
		return producerUnavailable(ctx, "read project", err)
	}
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		return false, ctx.Err()
	}
	if err != nil {
		return producerUnavailable(ctx, "read federation binding", err)
	}
	if binding.Role != db.FederationRoleSpoke || !binding.Enabled || !binding.PushEnabled || binding.RelayConfig == nil {
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
	if err != nil {
		return producerUnavailable(ctx, "read local project access", err)
	}
	if !slices.Contains(allowed, projectUID) {
		return false, ctx.Err()
	}
	credential, found, err := credentials.FederationCredential(ctx, projectUID)
	if err != nil {
		return producerUnavailable(ctx, "read federation credential", err)
	}
	if found && (credential.LeavePending || credential.RelayEnrollmentPending) {
		return false, ctx.Err()
	}
	if !found || credential.Token == "" {
		return producerUnavailable(ctx, "no federation credential for this project", nil)
	}
	if credential.Actor != binding.Actor || credential.HubProjectID != binding.HubProjectID {
		return producerUnavailable(ctx, "federation credential does not match the binding", nil)
	}
	// The token never leaves for another origin. Report the mismatch without
	// either URL, which could carry credentials.
	bindingOrigin, err := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if err != nil {
		return producerUnavailable(ctx, "federation binding hub URL is invalid", nil)
	}
	credentialOrigin, err := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
	if err != nil || bindingOrigin != credentialOrigin {
		return producerUnavailable(ctx, "federation credential hub origin does not match the binding", nil)
	}
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	check := producerCheck{store: store, project: project, binding: binding, credential: credential, selected: selected}
	connected, err := check.upstreamCurrent(ctx, requireSynced)
	if err != nil || !connected {
		return false, err
	}
	return check.stillLocal(ctx)
}

// producerCheck carries one producer authorization through its network and
// local recheck phases.
type producerCheck struct {
	store      db.Storage
	project    db.Project
	binding    db.FederationBinding
	credential config.FederationCredential
	selected   *db.ProjectEmbeddingProducer
}

// upstreamCurrent confirms the upstream hub's relay state and producer
// selection with one metadata read. With requireSynced it also defers fill
// while the hub still holds artifact offers for this relay.
func (p producerCheck) upstreamCurrent(ctx context.Context, requireSynced bool) (bool, error) {
	store, binding, credential, selected := p.store, p.binding, p.credential, p.selected
	c := binding.RelayConfig
	projectUID := p.project.UID
	opts := clientOptsForCredential(client.Opts{Timeout: client.DefaultHTTPTimeout}, credential)
	bindingOrigin, err := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if err != nil {
		return false, nil
	}
	upstream, err := NewClient(ctx, bindingOrigin, credential.Token, opts)
	if err != nil {
		return producerUnavailable(ctx, "connect to upstream hub", err)
	}
	metadata, err := upstream.ProjectFederation(ctx, binding.HubProjectID)
	if err != nil {
		return producerUnavailable(ctx, "fetch hub project metadata", err)
	}
	h := metadata.Relay
	if metadata.ProjectID != binding.HubProjectID || metadata.ProjectUID != projectUID || h == nil || h.ProtocolVersion != db.RelayProtocolVersion || h.BindingUID != c.BindingUID || h.UpstreamInstanceUID != c.UpstreamInstanceUID || h.Root.AuthorityUID != c.AuthorityUID || h.Root.ProjectUID != projectUID || h.ResetEpoch != c.ResetEpoch || h.ResetRequired || !slices.Equal(h.HubPath, c.HubPath) || h.EmbeddingProducer == nil || *h.EmbeddingProducer != *selected {
		return false, nil
	}
	pin, err := store.RootAuthority(ctx, projectUID)
	if err != nil {
		return producerUnavailable(ctx, "read pinned root authority", err)
	}
	if _, err := relayRootTransitionPath(pin, h.Root, h.RootKeyTransitions); err != nil {
		return false, nil
	}
	if requireSynced && h.PendingArtifactOffers && !pendingArtifactWaitExceeded(c.BindingUID, true) {
		// The federation runner imports pending portable artifacts through the
		// ordinary sync turns; eligibility only observes that they are owed.
		return producerUnavailable(ctx, "waiting for federation sync to deliver pending artifacts", nil)
	}
	if requireSynced && !h.PendingArtifactOffers {
		pendingArtifactWaitExceeded(c.BindingUID, false)
	}
	return true, nil
}

// stillLocal checks local policy and binding again after the authenticated
// network read.
func (p producerCheck) stillLocal(ctx context.Context) (bool, error) {
	store, project, binding, selected := p.store, p.project, p.binding, p.selected
	c := binding.RelayConfig
	current, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return producerUnavailable(ctx, "reread federation binding", err)
	}
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
	latest, err := store.ProjectByUID(ctx, project.UID)
	if err != nil {
		return producerUnavailable(ctx, "reread project", err)
	}
	latestProducer, err := db.ProjectEmbeddingProducerFromMetadata(latest.Metadata)
	if err != nil || latestProducer == nil || *latestProducer != *selected {
		return false, nil
	}
	allowed, err := store.AccessibleProjectUIDs(ctx, c.LocalActor)
	if err != nil {
		return producerUnavailable(ctx, "reread local project access", err)
	}
	if !slices.Contains(allowed, project.UID) {
		return false, ctx.Err()
	}
	return true, nil
}
