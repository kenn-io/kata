package client

import (
	"strings"

	"go.kenn.io/kata/internal/config"
)

type namedDaemonTarget struct {
	Catalog    config.CatalogDaemonConfig
	Auth       config.AuthConfig
	SourcePath string
}

func selectedNamedDaemonTarget(workspaceStart string) (namedDaemonTarget, bool, error) {
	server, _, ok, err := workspaceServerSelection(workspaceStart)
	if err != nil {
		return namedDaemonTarget{}, false, err
	}
	if !ok || server.Daemon == "" {
		return namedDaemonTarget{}, false, nil
	}
	return lookupNamedDaemonTarget(server.Daemon)
}

func lookupNamedDaemonTarget(name string) (namedDaemonTarget, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return namedDaemonTarget{}, false, nil
	}
	globalPath, err := config.DaemonConfigPath()
	if err != nil {
		return namedDaemonTarget{}, false, err
	}
	global, err := config.ReadDaemonConfig()
	if err != nil {
		return namedDaemonTarget{}, false, err
	}
	for _, target := range global.Daemons {
		if target.Name != name {
			continue
		}
		return namedDaemonTarget{
			Catalog:    target,
			SourcePath: globalPath,
		}, true, nil
	}
	return namedDaemonTarget{}, false, nil
}
