package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// RootSigning holds this daemon's owner-only root signing key. A missing or
// mismatched key never stops the daemon: projects whose pin the loaded key
// cannot satisfy refuse attributed writes until the key file is restored or
// the project is repinned. Public backups and upstream pins never restore it.
type RootSigning struct {
	path string
	mu   sync.RWMutex
	// signer always names this instance; PrivateKey is nil when no key loaded.
	signer db.RootAttributionSigner
	// problem explains why no key is loaded.
	problem string
}

// NewRootSigning wraps fixed signing material for an embedded server. It has
// no key file, so repinning cannot create a key for it.
func NewRootSigning(signer db.RootAttributionSigner) *RootSigning {
	return &RootSigning{signer: signer}
}

// LoadRootSigning reads the key at path. It creates a key only when the file
// is missing and no project pins one of this instance's keys, so a lost key
// never silently becomes a new identity. Key file problems are reported
// through Health instead of failing startup.
func LoadRootSigning(ctx context.Context, store db.Storage, path string) (*RootSigning, error) {
	pins, err := localRootPins(ctx, store)
	if err != nil {
		return nil, err
	}
	signing := &RootSigning{path: path, signer: db.RootAttributionSigner{AuthorityUID: store.InstanceUID()}}
	key, err := config.ReadRootSigningKey(path)
	if errors.Is(err, os.ErrNotExist) && len(pins) == 0 {
		key, err = config.LoadOrCreateRootSigningKey(path)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		signing.problem = "key file is missing"
	case err != nil:
		signing.problem = err.Error()
	default:
		signing.signer.PrivateKey = key
	}
	return signing, nil
}

// Signer returns the current signing material. Its PrivateKey is nil when no
// key is loaded; stores then refuse writes that need a root receipt.
func (r *RootSigning) Signer() db.RootAttributionSigner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.signer
}

// ensureKey returns the loaded key or reads the key file, creating it only
// when missing. created reports a new key file. A preview (apply false) never
// creates the file or changes the loaded key; it returns no key when the
// file would be created.
func (r *RootSigning) ensureKey(authorityUID string, apply bool) (signer db.RootAttributionSigner, created bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if db.RootSignerKeyID(r.signer) != "" {
		return r.signer, false, nil
	}
	if r.path == "" {
		return db.RootAttributionSigner{}, false, errors.New("this server has no root signing key file")
	}
	key, err := config.ReadRootSigningKey(r.path)
	if errors.Is(err, os.ErrNotExist) {
		if !apply {
			return db.RootAttributionSigner{AuthorityUID: authorityUID}, true, nil
		}
		key, err = config.LoadOrCreateRootSigningKey(r.path)
		created = true
	}
	if err != nil {
		return db.RootAttributionSigner{}, false, fmt.Errorf("root signing key %s: %w", r.path, err)
	}
	signer = db.RootAttributionSigner{AuthorityUID: authorityUID, PrivateKey: key}
	if apply {
		r.signer, r.problem = signer, ""
	}
	return signer, created, nil
}

// Health reports projects whose active local pin the loaded key cannot sign
// for. It returns nil when the key is loaded and every local pin matches.
func (r *RootSigning) Health(ctx context.Context, store db.Storage) (*api.RootSigningHealth, error) {
	pins, err := localRootPins(ctx, store)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	signer, problem, path := r.signer, r.problem, r.path
	r.mu.RUnlock()
	keyID := db.RootSignerKeyID(signer)
	health := &api.RootSigningHealth{KeyFile: path, KeyLoaded: keyID != "", KeyProblem: problem, Projects: []string{}}
	for _, pin := range pins {
		if pin.KeyID == keyID {
			continue
		}
		project, err := store.ProjectByUID(ctx, pin.ProjectUID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		health.Projects = append(health.Projects, project.Name)
	}
	if health.KeyLoaded && len(health.Projects) == 0 {
		return nil, nil
	}
	slices.Sort(health.Projects)
	return health, nil
}

// localRootPins lists the active pins rooted on this instance, including those
// of archived projects, which can be restored.
func localRootPins(ctx context.Context, store db.Storage) ([]db.RootKeyPin, error) {
	var pins []db.RootKeyPin
	for record, err := range store.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		if err != nil {
			return nil, fmt.Errorf("read root key pins: %w", err)
		}
		pin, ok := record.(*db.RootKeyPin)
		if !ok {
			break
		}
		if !pin.Retired && pin.AuthorityUID == store.InstanceUID() {
			pins = append(pins, *pin)
		}
	}
	return pins, nil
}

// addFederationRootSigningStatus marks hub projects rooted on this daemon as
// available or unavailable for signed writes with the loaded key.
func addFederationRootSigningStatus(ctx context.Context, cfg ServerConfig, body *api.FederationStatusBody) error {
	if cfg.RootSigning == nil {
		return nil
	}
	keyID := db.RootSignerKeyID(cfg.RootSigning.Signer())
	for i := range body.Statuses {
		status := &body.Statuses[i]
		if status.Role != string(db.FederationRoleHub) {
			continue
		}
		pin, err := cfg.DB.RootAuthority(ctx, status.ProjectUID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			pin.KeyID = keyID
		case err != nil:
			return internalAPIError(err)
		case pin.AuthorityUID != cfg.DB.InstanceUID():
			continue
		}
		status.RootSigningKey = "unavailable"
		if keyID != "" && pin.KeyID == keyID {
			status.RootSigningKey = "available"
		}
	}
	return nil
}
