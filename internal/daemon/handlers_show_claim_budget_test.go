package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

// Inject an I/O failure at the binding lookup to exercise deadline races
// without timing a real database or replacing the refresh implementation.
type showRefreshFailureStore struct {
	db.Storage
	lookup func(context.Context) (db.FederationBinding, error)
}

type showRefreshCredentialFailure struct {
	config.FederationCredentialStore
	delay time.Duration
}

type showRefreshProjectFailure struct {
	db.Storage
	delay time.Duration
}

func (s showRefreshProjectFailure) ProjectByID(context.Context, int64) (db.Project, error) {
	//nolint:kennlint // sleeptest: every caller runs inside synctest; advance fake time past the refresh deadlines.
	time.Sleep(s.delay)
	return db.Project{}, errors.New("fixture project read failed")
}

func TestShowClaimRefreshSetupFailureTimingProperty(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, Enabled: true,
		HubURL: "https://daemon.example", HubProjectID: 1,
		HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4AA",
	})
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		// Cover both sides of the remote and outer deadlines with fake time.
		delay := time.Duration(hegel.Draw(ht, hegel.Integers(0, 1000))) * time.Millisecond
		projectFails := hegel.Draw(ht, hegel.Booleans())
		var got error
		synctest.Test(t, func(t *testing.T) {
			cfg := ServerConfig{DB: store, FederationCredentials: showRefreshCredentialFailure{delay: delay}}
			if projectFails {
				cfg.DB = showRefreshProjectFailure{Storage: store, delay: delay}
			}
			_, got = refreshShowClaimStatus(t.Context(), cfg, issue)
		})
		if projectFails {
			require.ErrorContains(ht, got, "fixture project read failed")
		} else {
			require.ErrorContains(ht, got, "fixture credential read failed")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func (s showRefreshCredentialFailure) FederationCredential(context.Context, string) (config.FederationCredential, bool, error) {
	//nolint:kennlint // sleeptest: every caller runs inside synctest; advance fake time past the refresh deadlines.
	time.Sleep(s.delay)
	return config.FederationCredential{}, false, errors.New("fixture credential read failed")
}

func (showRefreshFailureStore) InstanceUID() string { return "show-refresh-fixture" }

func (s showRefreshFailureStore) FederationBindingByProject(ctx context.Context, _ int64) (db.FederationBinding, error) {
	return s.lookup(ctx)
}

func TestShowClaimRefreshBudgetErrorProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.Integers(0, 3))
		wraps := hegel.Draw(ht, hegel.Integers(0, 8))
		callerCanceled := hegel.Draw(ht, hegel.Booleans())
		var got error
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := showRefreshFailureStore{lookup: func(refreshCtx context.Context) (db.FederationBinding, error) {
				if callerCanceled {
					cancel()
				}
				<-refreshCtx.Done()
				failure := refreshCtx.Err()
				switch kind {
				case 1:
					failure = errors.New("fixture storage failure")
				case 2:
					failure = errors.Join(failure, ErrHostAccessDenied)
				case 3:
					failure = errors.Join(failure, db.ErrTransactionFinalizationFailed)
				}
				for range wraps {
					failure = fmt.Errorf("lookup: %w", failure)
				}
				return db.FederationBinding{}, failure
			}}
			_, got = refreshShowClaimStatus(ctx, ServerConfig{DB: store}, db.Issue{ProjectID: 1})
		})
		if kind == 0 && !callerCanceled {
			require.NoError(ht, got)
			return
		}
		var apiErr *api.APIError
		require.ErrorAs(ht, got, &apiErr)
		if !callerCanceled {
			switch kind {
			case 1:
				require.Contains(ht, apiErr.Message, "fixture storage failure")
			case 2:
				require.Equal(ht, http.StatusForbidden, apiErr.Status)
			case 3:
				require.Equal(ht, http.StatusServiceUnavailable, apiErr.Status)
			}
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
