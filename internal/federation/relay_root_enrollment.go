package federation

import (
	"context"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// PinRelayRootAuthority validates and installs the trusted enrollment pin using
// the same helper as local replica setup, without coupling the sync and daemon.
func PinRelayRootAuthority(ctx context.Context, store db.Storage, handshake api.RelayHandshake) error {
	return federationcoord.PinRelayRootAuthority(ctx, store, handshake)
}
