package sqlitestore

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

var _ db.ScopedRelayEnrollmentStore = (*Store)(nil)

func TestScopedAuthorityEnrollment(t *testing.T) {
	dbtest.RunScopedAuthority(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedAuthorityReadAndLockedChecks(t *testing.T) {
	store := openUIStore(t)
	f := dbtest.NewScopedAuthorityFixture(t, store)
	_, err := store.CreateScopedRelayEnrollment(t.Context(), f.Params)
	require.NoError(t, err)
	tx, err := store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	got, err := store.scopedAuthorityTx(t.Context(), tx, f.Params.View.Key, "pull")
	require.NoError(t, err)
	require.Equal(t, f.Params.View, got)
	require.NoError(t, tx.Rollback())
	require.NoError(t, store.relayTx(t.Context(), func(tx *sql.Tx) error {
		got, err := store.fenceScopedAuthorityTx(t.Context(), tx, f.Params.View.Key, "push")
		require.Equal(t, f.Params.View, got)
		return err
	}))
	_, err = store.SetTeamMembership(t.Context(), f.Teams[1].UID, "member", false, "admin")
	require.NoError(t, err)
	tx, err = store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	_, err = store.scopedAuthorityTx(t.Context(), tx, f.Params.View.Key, "pull")
	require.Error(t, err)
	require.NoError(t, tx.Rollback())
	require.Error(t, store.relayTx(t.Context(), func(tx *sql.Tx) error {
		_, err := store.fenceScopedAuthorityTx(t.Context(), tx, f.Params.View.Key, "push")
		return err
	}))
}

func TestScopedAuthorityIncomingDistinctAccounts(t *testing.T) {
	store := openUIStore(t)
	v, projects := dbtest.NewScopedIncomingAuthorityFixture(t, store)
	dbtest.InstallScopedIncomingAuthority(t, store, v, projects)
	for _, pushEnabled := range []bool{true, false} {
		for _, p := range projects {
			b, err := store.FederationBindingByProject(t.Context(), p.ID)
			require.NoError(t, err)
			b.PushEnabled = pushEnabled
			_, err = store.UpsertFederationBinding(t.Context(), b)
			require.NoError(t, err)
		}
		tx, err := store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		_, err = store.scopedAuthorityTx(t.Context(), tx, v.Key, "pull")
		require.NoError(t, err, "upstream member has no local-team membership; local-member does")
		_, writeErr := store.scopedAuthorityTx(t.Context(), tx, v.Key, "push")
		if pushEnabled {
			require.NoError(t, writeErr)
		} else {
			require.Error(t, writeErr)
		}
		require.NoError(t, tx.Rollback())
		require.NoError(t, store.relayTx(t.Context(), func(tx *sql.Tx) error {
			_, err := store.fenceScopedAuthorityTx(t.Context(), tx, v.Key, "pull")
			return err
		}))
	}
}

func TestScopedAuthorityIncomingMixedAccounts(t *testing.T) {
	store := openUIStore(t)
	v, projects := dbtest.NewScopedIncomingAuthorityFixture(t, store, 1)
	dbtest.InstallScopedIncomingAuthority(t, store, v, projects)
	tx, err := store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	_, err = store.scopedAuthorityTx(t.Context(), tx, v.Key, "pull")
	require.NoError(t, err, "indexed earlier-hop account may differ from this incoming account")
	require.NoError(t, tx.Rollback())
	dbtest.RestoreScopedIncomingMixedAccounts(t, store, projects[1].ID)
	tx, err = store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	_, err = store.scopedAuthorityTx(t.Context(), tx, v.Key, "pull")
	require.Error(t, err)
	require.NoError(t, tx.Rollback())
	require.Error(t, store.relayTx(t.Context(), func(tx *sql.Tx) error {
		_, err := store.fenceScopedAuthorityTx(t.Context(), tx, v.Key, "pull")
		return err
	}))
}

func TestScopedAuthorityLocalForwardingPrefix(t *testing.T) {
	dbtest.RunScopedAuthorityLocalForwardingPrefix(t, openUIStore(t))
}

func TestScopedAuthorityRestoredOutgoingGrantRole(t *testing.T) {
	for _, tc := range []struct {
		name, kind          string
		downstream, allowed bool
	}{
		{"continuing_route_leaf_only_refused", "continuing", false, false},
		{"adjacent_forwarder_over_hub_prefix_refused", "last_leaf", true, false},
		{"adjacent_final_leaf_at_limit_allowed", "last_leaf", false, true},
		{"early_adjacent_forwarder_full_route_at_limit_allowed", "early_forwarder", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openUIStore(t)
			view := dbtest.NewScopedOutgoingGrantFixture(t, store, tc.kind)
			tx, err := store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			_, err = store.scopedAuthorityTx(t.Context(), tx, view.Key, "pull")
			require.NoError(t, err, "factory-created live authority is positive before restore")
			require.NoError(t, tx.Rollback())
			require.NoError(t, store.relayTx(t.Context(), func(tx *sql.Tx) error {
				_, err := store.fenceScopedAuthorityTx(t.Context(), tx, view.Key, "pull")
				return err
			}))
			dbtest.RestoreScopedOutgoingGrantFlags(t, store, view, tc.downstream)
			for _, capability := range []string{"pull", "push", "claim"} {
				tx, err := store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				_, err = store.scopedAuthorityTx(t.Context(), tx, view.Key, capability)
				if tc.allowed {
					assert.NoError(t, err, "SELECT %s", capability)
				} else {
					assert.Error(t, err, "SELECT %s", capability)
				}
				require.NoError(t, tx.Rollback())
				err = store.relayTx(t.Context(), func(tx *sql.Tx) error {
					_, err := store.fenceScopedAuthorityTx(t.Context(), tx, view.Key, capability)
					return err
				})
				if tc.allowed {
					assert.NoError(t, err, "locked %s", capability)
				} else {
					assert.Error(t, err, "locked %s", capability)
				}
			}
		})
	}
}

func TestScopedAuthorityMissingCurrentContext(t *testing.T) {
	t.Run("native_root_never_cutover", func(t *testing.T) {
		store := openUIStore(t)
		f := dbtest.ScopedAuthorityWithoutCutover(t, store)
		_, err := store.CreateScopedRelayEnrollment(t.Context(), f.Params)
		require.ErrorIs(t, err, db.ErrNotFound, "missing live scoped floor is a canonical authority denial")
	})
	for _, name := range []string{"missing_view", "missing_generation"} {
		t.Run(name, func(t *testing.T) {
			store := openUIStore(t)
			f := dbtest.NewScopedAuthorityFixture(t, store)
			_, err := store.CreateScopedRelayEnrollment(t.Context(), f.Params)
			require.NoError(t, err)
			key := f.Params.View.Key
			if name == "missing_view" {
				key.ViewUID = "00000000000000000000000999"
			} else {
				key.Generation++
			}
			tx, err := store.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			_, err = store.scopedAuthorityTx(t.Context(), tx, key, "pull")
			assert.ErrorIs(t, err, db.ErrNotFound)
			require.NoError(t, tx.Rollback())
			err = store.relayTx(t.Context(), func(tx *sql.Tx) error {
				_, err := store.fenceScopedAuthorityTx(t.Context(), tx, key, "pull")
				return err
			})
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}
