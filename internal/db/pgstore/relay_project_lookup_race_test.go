package pgstore

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// A relay transaction that cannot read the project row because another
// session holds the table must fail with the lock error, which the relay
// transaction helpers retry. Only a missing project is a denial.
func TestRelayProjectLookupKeepsLockErrorsRetryable(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	rootUID := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	spoke, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: spoke.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: spoke.UID, Actor: "company-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	upstreamBinding, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, spoke.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: upstreamBinding, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "company-member", ResetEpoch: 1})
	require.NoError(t, err)

	hub, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: hub.ID, Role: db.FederationRoleHub, HubProjectID: hub.ID, HubProjectUID: hub.UID, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: hub.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "company-member", AdminActor: "admin", PlaintextToken: "lookup-parent-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: hub.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "lookup-relay-test-token"})
	require.NoError(t, err)

	holder, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	_, err = holder.ExecContext(ctx, `LOCK TABLE projects IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	probe := func(t *testing.T, op func(context.Context, *sql.Tx) error) error {
		t.Helper()
		tx, err := store.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout = '200ms'`)
		require.NoError(t, err)
		return op(ctx, tx)
	}
	for name, op := range map[string]func(context.Context, *sql.Tx) error{
		"upstream grant": func(ctx context.Context, tx *sql.Tx) error {
			_, err := store.upstreamRelayGrantTx(ctx, tx, upstreamBinding)
			return err
		},
		"parent credential": func(ctx context.Context, tx *sql.Tx) error {
			return store.checkRelayParentTx(ctx, tx, grant.Enrollment, hub.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := probe(t, op)
			require.Error(t, err)
			require.NotErrorIs(t, err, db.ErrNotFound, "a lock wait is not a missing project")
			require.True(t, IsTransient(err), "the lock error must stay retryable: %v", err)
		})
	}
	require.NoError(t, holder.Rollback())
	require.NoError(t, probe(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.upstreamRelayGrantTx(ctx, tx, upstreamBinding)
		return err
	}), "the same lookup succeeds once the table is free")
}
