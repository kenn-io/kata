package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// LoadRootAttributionSigner binds a namespace's owner-only secret to its stored
// local authority. Public backups and upstream pins never restore a private key.
func LoadRootAttributionSigner(ctx context.Context, store db.Storage, path string) (db.RootAttributionSigner, error) {
	expected := ""
	for record, err := range store.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		if err != nil {
			return db.RootAttributionSigner{}, err
		}
		pin, ok := record.(*db.RootKeyPin)
		if !ok {
			break
		}
		if pin.Retired || pin.AuthorityUID != store.InstanceUID() {
			continue
		}
		if expected != "" && expected != pin.KeyID {
			return db.RootAttributionSigner{}, errors.New("local root projects have different active signing keys; explicit recovery is required")
		}
		expected = pin.KeyID
	}
	key, err := config.LoadOrCreateRootSigningKey(path, expected)
	if err != nil {
		return db.RootAttributionSigner{}, err
	}
	return db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}, nil
}
