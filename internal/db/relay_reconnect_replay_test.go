package db_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestValidateImportAllowsDetachedRelayHistoryAfterReconnect(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	instanceUID := "00000000000000000000000009"
	projectUID := "00000000000000000000000001"
	rootUID := "00000000000000000000000002"
	currentBindingUID := "00000000000000000000000006"
	detachedBindingUID := "00000000000000000000000005"
	project := &db.ProjectExport{ID: 42, UID: projectUID, Name: "spoke-project"}
	pin := &db.RootKeyPin{
		ProjectUID: projectUID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	currentConfig := &db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: currentBindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, instanceUID}, LocalActor: "example-actor", ResetEpoch: 1,
	}
	records := []db.ImportRecord{
		&db.MetaKV{Key: "instance_uid", Value: instanceUID},
		project,
		pin,
		&db.FederationBindingExport{
			ProjectID: project.ID, Role: string(db.FederationRoleSpoke),
			HubProjectUID: projectUID, Actor: "example-actor", RelayConfig: currentConfig,
		},
		&db.RelayCursorExport{
			ProjectUID: projectUID, BindingUID: detachedBindingUID,
			Stream: db.RelayStreamEvent, ResetEpoch: 1,
		},
	}
	require.NoError(t, db.ValidateImportRecords(records), "a reconnect uses a new binding UID while backup history retains the detached namespace")
}
