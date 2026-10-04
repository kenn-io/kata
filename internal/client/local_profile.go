package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/oklog/ulid/v2"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/safefileio"
)

// LocalProfileStorageIdentity retains observed identity even on a pin mismatch.
type LocalProfileStorageIdentity struct {
	InstanceUID   string
	SchemaVersion int
}

// OpenLocalProfileReadOnly opens an existing selected target without preparation.
func OpenLocalProfileReadOnly(ctx context.Context, profile config.LocalProfileConfig) (db.Storage, error) {
	if !strings.HasPrefix(profile.DSN, "postgres://") && !strings.HasPrefix(profile.DSN, "postgresql://") {
		return storeopen.OpenReadOnly(ctx, profile.DSN)
	}
	pg := profile.Config.Storage.Postgres
	// OpenWithConfig plus ReadOnly preserves the selected schema and cannot
	// bootstrap it. OpenReadOnly's default PostgreSQL dispatch uses current-home
	// configuration, so it is deliberately not used for a selected PG profile.
	return storeopen.OpenWithConfig(ctx, profile.DSN, storeopen.Config{Postgres: pgstore.ConfigFromValues(pg.Schema, pg.Mode, pg.SchemaOwner, pg.AllowInsecure)}, db.ReadOnly())
}

// InspectLocalProfileStorage reads existing metadata without bootstrap,
// migration, or cutover. Callers must check the schema before starting a daemon.
func InspectLocalProfileStorage(ctx context.Context, profile config.LocalProfileConfig) (LocalProfileStorageIdentity, error) {
	timeout, _ := ParseHTTPTimeout(os.Getenv(HTTPTimeoutEnvVar), DefaultHTTPTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if strings.HasPrefix(profile.DSN, "postgres://") || strings.HasPrefix(profile.DSN, "postgresql://") {
		pg := profile.Config.Storage.Postgres
		uid, version, err := pgstore.InspectMetadataWithConfig(ctx, profile.DSN, pgstore.ConfigFromValues(pg.Schema, pg.Mode, pg.SchemaOwner, pg.AllowInsecure))
		if err != nil {
			return LocalProfileStorageIdentity{}, fmt.Errorf("%w: %w", ErrProfileStorageUnavailable, err)
		}
		identity := LocalProfileStorageIdentity{InstanceUID: uid, SchemaVersion: version}
		if uid != profile.InstanceUID {
			return identity, &LocalProfileIdentityError{Expected: profile.InstanceUID, Observed: uid}
		}
		return identity, nil
	}
	store, err := OpenLocalProfileReadOnly(ctx, profile)
	if err != nil {
		return LocalProfileStorageIdentity{}, fmt.Errorf("%w: %w", ErrProfileStorageUnavailable, err)
	}
	defer func() { _ = store.Close() }()
	if err = store.RefreshInstanceUID(ctx); err != nil {
		return LocalProfileStorageIdentity{}, fmt.Errorf("%w: read instance identity: %w", ErrProfileStorageUnavailable, err)
	}
	identity := LocalProfileStorageIdentity{InstanceUID: store.InstanceUID()}
	identity.SchemaVersion, err = store.SchemaVersion(ctx)
	if err != nil {
		return identity, fmt.Errorf("%w: read schema version: %w", ErrProfileStorageUnavailable, err)
	}
	if identity.InstanceUID != profile.InstanceUID {
		return identity, &LocalProfileIdentityError{Expected: profile.InstanceUID, Observed: identity.InstanceUID}
	}
	return identity, nil
}

var (
	// ErrProfileStorageUnavailable means existing profile storage cannot be inspected.
	ErrProfileStorageUnavailable = errors.New("local profile storage unavailable")
	// ErrProfileIdentityMismatch means the observed database differs from its pin.
	ErrProfileIdentityMismatch = errors.New("local profile database identity mismatch")
	// ErrProfileSchemaMismatch prevents startup against unsupported storage.
	ErrProfileSchemaMismatch = errors.New("local profile schema is incompatible")
)

// LocalProfileIdentityError exposes only validated identity metadata.
type LocalProfileIdentityError struct{ Expected, Observed string }

func (e *LocalProfileIdentityError) Error() string {
	return "local profile database identity differs from its configured instance_uid"
}
func (e *LocalProfileIdentityError) Unwrap() error { return ErrProfileIdentityMismatch }

func discoverLocalProfile(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, bool, error) {
	return discoverLocalProfileUsing(ctx, selection, liveDaemons)
}

func discoverLocalProfileUsing(ctx context.Context, selection DaemonSelection, scan liveDaemonScanner) (ResolvedDaemon, bool, error) {
	return discoverLocalProfileWithVerifier(ctx, selection, scan, verifyLocalProfileInstance)
}

func discoverLocalProfileUsingWithoutIdentityProbe(ctx context.Context, selection DaemonSelection, scan liveDaemonScanner) (ResolvedDaemon, bool, error) {
	return discoverLocalProfileWithVerifier(ctx, selection, scan, nil)
}

func discoverLocalProfileWithVerifier(
	ctx context.Context,
	selection DaemonSelection,
	scan liveDaemonScanner,
	verify func(context.Context, ResolvedDaemon) error,
) (ResolvedDaemon, bool, error) {
	resolved := selection.Resolved
	if _, err := os.Stat(resolved.LocalProfile.DataDir); errors.Is(err, os.ErrNotExist) {
		return resolved, false, nil
	} else if err != nil {
		return resolved, false, err
	}
	if err := safefileio.ValidatePrivateDir(resolved.LocalProfile.DataDir); err != nil {
		return resolved, false, &PrivateDirError{Err: err}
	}
	var unreachable error
	for candidate, err := range scan(ctx, resolved.LocalProfile.DataDir) {
		if err != nil {
			if errors.Is(err, ErrLocalDaemonUnreachable) {
				unreachable = err
				continue
			}
			return resolved, false, err
		}
		resolved = resolved.WithRunning(runningDaemonForLive(candidate))
		resolved.UnixSocket = candidate.UnixSocket
		if verify != nil {
			if err := verify(ctx, resolved); err != nil {
				return resolved, false, err
			}
		}
		return resolved, true, nil
	}
	return resolved, false, unreachable
}

func verifyLocalProfileInstance(ctx context.Context, resolved ResolvedDaemon) error {
	timeout, _ := ParseHTTPTimeout(os.Getenv(HTTPTimeoutEnvVar), DefaultHTTPTimeout)
	hc, err := NewHTTPClientForResolved(ctx, resolved, Opts{Timeout: timeout})
	if err != nil {
		return err
	}
	apiClient, err := generated.NewDefaultClient(resolved.BaseURL, runtime.WithHTTPClient(probeRequestDoer{hc}))
	if err != nil {
		return err
	}
	resp, err := apiClient.InstanceWithResponse(ctx)
	if resp == nil {
		return fmt.Errorf("local profile instance is unavailable: %w", ErrLocalDaemonUnreachable)
	}
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("local profile instance authentication failed (HTTP %d); configure its client credential", resp.StatusCode)
		}
		return fmt.Errorf("local profile instance identity unavailable (HTTP %d)", resp.StatusCode)
	}
	if err != nil || resp.JSON200 == nil {
		return fmt.Errorf("local profile returned an unreadable instance identity")
	}
	if _, err := ulid.ParseStrict(resp.JSON200.InstanceUID); err != nil {
		return fmt.Errorf("local profile returned an unreadable instance identity")
	}
	if resp.JSON200.InstanceUID != resolved.LocalProfile.InstanceUID {
		return &LocalProfileIdentityError{Expected: resolved.LocalProfile.InstanceUID, Observed: resp.JSON200.InstanceUID}
	}
	return nil
}

func ensureLocalProfile(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, error) {
	profile := *selection.Profile
	identity, err := InspectLocalProfileStorage(ctx, profile)
	if err != nil {
		return selection.Resolved, err
	}
	// A process is never stopped or started against unsupported storage. This
	// check precedes version replacement, including a newer live daemon.
	if identity.SchemaVersion != db.CurrentSchemaVersion() {
		return selection.Resolved, ErrProfileSchemaMismatch
	}
	resolved := selection.Resolved
	if err := safefileio.ValidatePrivateDir(resolved.LocalProfile.DataDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	found, err := discoverForEnsure(ctx, resolved.LocalProfile.DataDir)
	if err != nil {
		return resolved, err
	}
	if found.Outcome != daemonScanNone {
		resolved = resolved.WithRunning(runningDaemonForLive(found.Daemon))
		resolved.UnixSocket = found.Daemon.UnixSocket
		if err := verifyLocalProfileInstance(ctx, resolved); err != nil {
			return resolved, err
		}
		if found.Outcome == daemonScanCompatible {
			return resolved, nil
		}
		if err := stopRunningDaemonsForEnsure(ctx, resolved.LocalProfile.DataDir, profile.StorageID); err != nil {
			return resolved, err
		}
	}
	env, err := config.LocalProfileEnvironment(profile, true)
	if err != nil {
		return resolved, err
	}
	running, err := autoStartWithEnvironment(ctx, resolved.LocalProfile.DataDir, env)
	if err != nil {
		return resolved, err
	}
	resolved = resolved.WithRunning(running)
	if err := verifyLocalProfileInstance(ctx, resolved); err != nil {
		return resolved, err
	}
	return resolved, nil
}

// DiscoverLocalProfileSelection probes the selected runtime and its pinned
// instance identity without opening storage or re-reading workspace routing.
func DiscoverLocalProfileSelection(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, bool, error) {
	if selection.Profile == nil || selection.Resolved.LocalProfile == nil {
		return selection.Resolved, false, errors.New("selection is not a registered local profile")
	}
	return discoverLocalProfile(ctx, selection)
}

// EnsureLocalProfileSelection starts only the home pinned by this snapshot.
func EnsureLocalProfileSelection(ctx context.Context, selection DaemonSelection) (ResolvedDaemon, error) {
	if selection.Profile == nil || selection.Resolved.LocalProfile == nil {
		return selection.Resolved, errors.New("selection is not a registered local profile")
	}
	return ensureLocalProfile(ctx, selection)
}

// EnsureSameLocalProfile refreshes the original profile snapshot without reading
// a changed workspace or catalog. Incomplete snapshots fail closed.
func EnsureSameLocalProfile(ctx context.Context, resolved ResolvedDaemon) (ResolvedDaemon, error) {
	if resolved.profileConfig == nil || resolved.LocalProfile == nil {
		return resolved, errors.New("local profile snapshot is unavailable")
	}
	return EnsureLocalProfileSelection(ctx, DaemonSelection{Resolved: resolved, Profile: resolved.profileConfig})
}
