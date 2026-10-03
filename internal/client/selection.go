package client

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
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
}

// InspectSelection snapshots target configuration without probing or starting it.
func InspectSelection(ctx context.Context, start, name string) (DaemonSelection, error) {
	return inspectSelection(ctx, start, name, false)
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
		for _, entry := range cfg.Daemons {
			if entry.Name != name {
				continue
			}
			if entry.Home == "" {
				if source == DaemonSourceLocalConfig {
					return DaemonSelection{}, fmt.Errorf("server.daemon requires an explicit local profile with home and instance_uid")
				}
				if entry.Local && source == DaemonSourceActiveDaemon {
					if !keepActiveLocalCatalogCredential {
						return DaemonSelection{Resolved: ResolvedDaemon{
							Source: DaemonSourceLocalRuntime,
						}.withGlobalAuth()}, nil
					}
					token := entry.Token
					if entry.TokenEnv != "" {
						token = strings.TrimSpace(os.Getenv(entry.TokenEnv))
						if token == "" {
							return DaemonSelection{}, fmt.Errorf("daemon %q: configured token_env is empty", name)
						}
					}
					return DaemonSelection{
						Resolved:           (ResolvedDaemon{Source: DaemonSourceLocalRuntime}).withLocalTargetAuth(token),
						activeLocalCatalog: true,
					}, nil
				}
				resolved := ResolvedDaemon{Source: source, Name: name, SourcePath: sourcePath}
				token := entry.Token
				if entry.TokenEnv != "" {
					token = strings.TrimSpace(os.Getenv(entry.TokenEnv))
				}
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
				return DaemonSelection{Resolved: resolved}, nil
			}
			profile, err := config.ResolveLocalProfile(entry)
			if err != nil {
				return DaemonSelection{}, err
			}
			ns, err := daemon.NewNamespaceForHome(profile.Home, profile.StorageID)
			if err != nil {
				return DaemonSelection{}, err
			}
			resolved := ResolvedDaemon{Source: source, Name: name, SourcePath: sourcePath, profileConfig: &profile, LocalProfile: &LocalProfileIdentity{Name: name, Home: profile.Home, DataDir: ns.DataDir, StorageID: profile.StorageID, InstanceUID: profile.InstanceUID}, Token: profile.Config.Auth.Token, TrustPrivateNetwork: profile.Config.Auth.TrustPrivateNetwork}
			if entry.Token != "" {
				resolved.Token = entry.Token
			}
			if entry.TokenEnv != "" {
				resolved.Token = strings.TrimSpace(os.Getenv(entry.TokenEnv))
				if resolved.Token == "" {
					return DaemonSelection{}, fmt.Errorf("daemon %q: configured token_env is empty", name)
				}
			}
			if _, err := config.LocalProfileEnvironment(profile, false); err != nil {
				return DaemonSelection{}, err
			}
			return DaemonSelection{Resolved: resolved, Profile: &profile}, nil
		}
		return DaemonSelection{}, fmt.Errorf("%w: %q", ErrNamedDaemonNotFound, name)
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
