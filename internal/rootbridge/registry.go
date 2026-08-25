package rootbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

var (
	// ErrConnectorInstanceNotFound reports an unknown configured instance ID.
	ErrConnectorInstanceNotFound = errors.New("connector instance not found")
	// ErrConnectorIdentityChanged reports drift from the accepted connector identity.
	ErrConnectorIdentityChanged = errors.New("connector identity changed")
)

// ClientFactory constructs one process client from validated configuration.
type ClientFactory func(config.ConnectorConfig) connectorclient.Client

// Instance is the live internal representation of one configured connector.
// Callers serving API responses must use InstanceSnapshot instead.
type Instance struct {
	Config                 config.ConnectorConfig
	Client                 connectorclient.Client
	Description            connector.Description
	descriptionHealthError string
	fieldHealthError       string
}

// InstanceSnapshot is the API-safe connector health and identity view.
type InstanceSnapshot struct {
	ID              string                 `json:"id"`
	ConnectorID     string                 `json:"connector_id,omitempty"`
	DisplayName     string                 `json:"display_name,omitempty"`
	Protocol        string                 `json:"protocol,omitempty"`
	AccountIdentity string                 `json:"account_identity,omitempty"`
	Capabilities    []connector.Capability `json:"capabilities,omitempty"`
	HealthError     string                 `json:"health_error,omitempty"`
}

// Registry owns validated connector instances and their last known health.
type Registry struct {
	mu        sync.RWMutex
	instances map[string]Instance
}

// NewRegistry validates all configuration before constructing clients. It does
// not contact external processes; callers refresh instances after startup.
func NewRegistry(_ context.Context, configs []config.ConnectorConfig, factory ClientFactory) (*Registry, error) {
	normalized, err := config.NormalizeConnectorConfigs(configs)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		factory = connectorclient.NewProcessClient
	}
	registry := &Registry{instances: make(map[string]Instance, len(normalized))}
	for _, cfg := range normalized {
		registry.instances[cfg.ID] = Instance{Config: cfg, Client: wrapConnectorClient(factory(cfg))}
	}
	return registry, nil
}

// Refresh probes one live connector and records either its validated
// description or a credential-scrubbed health error.
func (r *Registry) Refresh(ctx context.Context, id string) (InstanceSnapshot, error) {
	instance, ok := r.instance(id)
	if !ok {
		return InstanceSnapshot{}, fmt.Errorf("%w: %s", ErrConnectorInstanceNotFound, strings.TrimSpace(id))
	}
	description, err := instance.Client.Describe(ctx)
	if err != nil {
		if !parentContextEnded(ctx, err) {
			r.recordDescribeHealthError(instance.Config.ID, err)
		}
		snapshot, _ := r.Snapshot(instance.Config.ID)
		return snapshot, err
	}
	if err := r.acceptDescription(instance.Config.ID, description); err != nil {
		snapshot, _ := r.Snapshot(instance.Config.ID)
		return snapshot, err
	}
	snapshot, _ := r.Snapshot(instance.Config.ID)
	return snapshot, nil
}

// Snapshot returns one API-safe connector view.
func (r *Registry) Snapshot(id string) (InstanceSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	instance, ok := r.instances[strings.TrimSpace(id)]
	if !ok {
		return InstanceSnapshot{}, false
	}
	return snapshotOf(instance), true
}

// Snapshots returns API-safe connector views ordered by configured ID.
func (r *Registry) Snapshots() []InstanceSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.instances))
	for id := range r.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snapshots := make([]InstanceSnapshot, 0, len(ids))
	for _, id := range ids {
		snapshots = append(snapshots, snapshotOf(r.instances[id]))
	}
	return snapshots
}

// RefreshSnapshots probes every configured connector concurrently and returns
// the resulting safe snapshots in configured-ID order. One unavailable
// connector records its own health without failing the complete list.
func (r *Registry) RefreshSnapshots(ctx context.Context) []InstanceSnapshot {
	snapshots := r.Snapshots()
	var wait sync.WaitGroup
	for index := range snapshots {
		wait.Go(func() {
			refreshed, err := r.Refresh(ctx, snapshots[index].ID)
			if err == nil {
				snapshots[index] = refreshed
				return
			}
			if current, ok := r.Snapshot(snapshots[index].ID); ok {
				snapshots[index] = current
			}
		})
	}
	wait.Wait()
	return snapshots
}

// Fields returns the live provider-neutral descriptor list and updates the
// instance health snapshot without exposing executable configuration.
func (r *Registry) Fields(ctx context.Context, id string) ([]connector.FieldDescriptor, error) {
	instance, ok := r.instance(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrConnectorInstanceNotFound, strings.TrimSpace(id))
	}
	if instance.Description.ConnectorID == "" {
		if _, err := r.Refresh(ctx, id); err != nil {
			return nil, err
		}
		instance, _ = r.instance(id)
	}
	if !descriptionSupportsFields(instance.Description) {
		return nil, ErrFieldSynchronizationUnavailable
	}
	listed, err := instance.Client.ListFields(ctx)
	if err != nil {
		if !parentContextEnded(ctx, err) {
			r.recordFieldHealthError(instance.Config.ID, err)
		}
		return nil, err
	}
	r.clearFieldHealthError(instance.Config.ID)
	fields := append([]connector.FieldDescriptor(nil), listed.Fields...)
	return fields, nil
}

func (r *Registry) instance(id string) (Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	instance, ok := r.instances[strings.TrimSpace(id)]
	if !ok {
		return Instance{}, false
	}
	instance.Description = cloneDescription(instance.Description)
	return instance, true
}

func (r *Registry) acceptDescription(id string, description connector.Description) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance := r.instances[id]
	if instance.Description.ConnectorID != "" &&
		(description.ConnectorID != instance.Description.ConnectorID || description.Protocol != instance.Description.Protocol) {
		instance.descriptionHealthError = db.SafeExternalRootError(ErrConnectorIdentityChanged.Error())
		r.instances[id] = instance
		return fmt.Errorf("%w: connector ID or protocol does not match", ErrConnectorIdentityChanged)
	}
	if err := validateDescription(description); err != nil {
		instance.descriptionHealthError = db.SafeExternalRootError(err.Error())
		r.instances[id] = instance
		return err
	}
	instance.Description = cloneDescription(description)
	instance.descriptionHealthError = ""
	r.instances[id] = instance
	return nil
}

func (r *Registry) recordDescribeHealthError(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[id]
	if !ok {
		return
	}
	instance.descriptionHealthError = db.SafeExternalRootError(err.Error())
	r.instances[id] = instance
}

func (r *Registry) recordFieldHealthError(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[id]
	if !ok {
		return
	}
	instance.fieldHealthError = db.SafeExternalRootError(err.Error())
	r.instances[id] = instance
}

func (r *Registry) clearFieldHealthError(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[id]
	if !ok {
		return
	}
	instance.fieldHealthError = ""
	r.instances[id] = instance
}

func snapshotOf(instance Instance) InstanceSnapshot {
	healthError := instance.descriptionHealthError
	if healthError == "" {
		healthError = instance.fieldHealthError
	}
	return InstanceSnapshot{
		ID: instance.Config.ID, ConnectorID: instance.Description.ConnectorID,
		DisplayName: instance.Description.DisplayName, Protocol: instance.Description.Protocol,
		AccountIdentity: instance.Description.AccountIdentity,
		Capabilities:    append([]connector.Capability(nil), instance.Description.Capabilities...),
		HealthError:     healthError,
	}
}

func cloneDescription(description connector.Description) connector.Description {
	description.Capabilities = append([]connector.Capability(nil), description.Capabilities...)
	description.ConfigSchema = append(json.RawMessage(nil), description.ConfigSchema...)
	return description
}

func validateDescription(description connector.Description) error {
	if strings.TrimSpace(description.ConnectorID) == "" || strings.TrimSpace(description.AccountIdentity) == "" {
		return errors.New("connector description is invalid")
	}
	if description.Protocol != connector.ProtocolVersion {
		return errors.New("connector description is invalid")
	}
	if len(description.ConfigSchema) != 0 && !json.Valid(description.ConfigSchema) {
		return errors.New("connector description is invalid")
	}
	return nil
}
