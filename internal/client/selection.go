package client

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
)

// LocalProfileIdentity carries the exact namespace and persisted identity.
type LocalProfileIdentity struct {
	Name, Home, DataDir, StorageID, InstanceUID string
}

// DaemonSelection describes the chosen input even when its daemon is stopped.
// InspectSelection neither opens storage nor starts or probes a process.
type DaemonSelection struct {
	Resolved           ResolvedDaemon
	Profile            *config.LocalProfileConfig
	activeLocalCatalog bool
	// credentialErr is a catalog credential that resolving this selection
	// needs but cannot read. Inspection itself sends no credential, so only
	// resolution reports it.
	credentialErr error
}

// InspectSelection snapshots target configuration without probing or starting it.
func InspectSelection(ctx context.Context, start, name string) (DaemonSelection, error) {
	return inspectSelection(ctx, start, name, false)
}

// InspectTelemetryTarget identifies the selected daemon without contacting it.
// An empty result leaves normal discovery responsible for choosing a target.
func InspectTelemetryTarget(ctx context.Context, start, name string) string {
	if value, _ := ctx.Value(BaseURLKey{}).(string); value != "" {
		return ""
	}
	selection, err := InspectSelection(ctx, start, name)
	if err != nil {
		return ""
	}
	return TelemetryTargetKey(selection.Resolved)
}

func inspectSelection(ctx context.Context, start, name string, keepActiveLocalCatalogCredential bool) (DaemonSelection, error) {
	source := DaemonSourceNamedCatalog
	sourcePath := ""
	name = strings.TrimSpace(name)
	if name == "" && os.Getenv(remoteServerEnvVar) == "" {
		root, path, ok, err := findLocalConfig(start)
		if err != nil {
			return DaemonSelection{}, err
		}
		workspaceURL := false
		if ok {
			local, err := config.ReadLocalConfig(root)
			if err != nil {
				return DaemonSelection{}, err
			}
			workspaceURL = local.Server.URL != ""
			if local.Server.Daemon != "" {
				name = local.Server.Daemon
				source = DaemonSourceLocalConfig
				sourcePath = path
			}
		}
		if name == "" && !workspaceURL {
			cfg, err := config.ReadDaemonCatalogAndAuthPolicy()
			if err != nil {
				return DaemonSelection{}, err
			}
			name = cfg.ActiveDaemon
			source = DaemonSourceActiveDaemon
		}
	}

	if name != "" {
		if sourcePath == "" {
			var err error
			sourcePath, err = config.DaemonConfigPath()
			if err != nil {
				return DaemonSelection{}, err
			}
		}
		cfg, err := config.ReadDaemonCatalogAndAuthPolicy()
		if err != nil {
			return DaemonSelection{}, err
		}
		entry, ok := cfg.CatalogDaemon(name)
		if !ok {
			return DaemonSelection{}, fmt.Errorf("%w: %q", ErrNamedDaemonNotFound, name)
		}
		if entry.Home == "" && source == DaemonSourceLocalConfig {
			return DaemonSelection{}, fmt.Errorf("server.daemon requires an explicit local profile with home and instance_uid")
		}
		if entry.Home == "" && entry.Local && source == DaemonSourceActiveDaemon && !keepActiveLocalCatalogCredential {
			return DaemonSelection{Resolved: ResolvedDaemon{Source: DaemonSourceLocalRuntime}.withGlobalAuth()}, nil
		}
		token, credentialErr := catalogClientToken(entry)
		if entry.Home == "" {
			if entry.Local && source == DaemonSourceActiveDaemon {
				if credentialErr != nil {
					return DaemonSelection{}, credentialErr
				}
				return DaemonSelection{
					Resolved:           (ResolvedDaemon{Source: DaemonSourceLocalRuntime}).withLocalTargetAuth(token),
					activeLocalCatalog: true,
				}, nil
			}
			resolved := ResolvedDaemon{Source: source, Name: name, SourcePath: sourcePath}
			if entry.Local {
				resolved = resolved.withLocalTargetAuth(token)
			} else {
				base, err := normalizeRemoteURL(entry.URL, entry.AllowInsecure)
				if err != nil {
					return DaemonSelection{}, err
				}
				resolved = resolvedForRunning(source, name, remoteRunningDaemon(base, true)).withRemoteTargetAuth(token, entry.AllowInsecure)
				resolved.SourcePath = sourcePath
			}
			return DaemonSelection{Resolved: resolved, credentialErr: credentialErr}, nil
		}
		if credentialErr != nil {
			// The profile's identity check sends this credential.
			return DaemonSelection{}, credentialErr
		}
		profile, err := config.ResolveLocalProfile(entry)
		if err != nil {
			return DaemonSelection{}, err
		}
		ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
		if err != nil {
			return DaemonSelection{}, err
		}
		resolved := ResolvedDaemon{Source: source, Name: name, SourcePath: sourcePath, profileConfig: &profile, LocalProfile: &LocalProfileIdentity{Name: name, Home: profile.Home, DataDir: ns.DataDir, StorageID: profile.StorageID, InstanceUID: profile.InstanceUID}, Token: profile.Config.Auth.Token, TrustPrivateNetwork: profile.Config.Auth.TrustPrivateNetwork}
		if token != "" {
			resolved.Token = token
		}
		if _, err := config.LocalProfileEnvironment(profile, false); err != nil {
			return DaemonSelection{}, err
		}
		return DaemonSelection{Resolved: resolved, Profile: &profile}, nil
	}
	resolved, ok, err := resolveRemoteSelection(ctx, start, remoteRequestTarget)
	if err != nil {
		return DaemonSelection{}, err
	}
	if !ok {
		resolved.Source = DaemonSourceLocalRuntime
	}
	return DaemonSelection{Resolved: resolved}, nil
}

// catalogClientToken is a catalog entry's credential as clients send it.
// KATA_AUTH_TOKEN replaces a remote entry's credential, so its token_env is
// not read then.
func catalogClientToken(entry config.CatalogDaemonConfig) (string, error) {
	if !entry.Local && globalAuthTokenOverrideSet() {
		return entry.Token, nil
	}
	return entry.ClientToken()
}
