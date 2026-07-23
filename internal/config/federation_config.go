package config

import (
	"errors"
	"fmt"
	"strings"
)

// FederationConfig is the [federation] block of <KATA_HOME>/config.toml.
type FederationConfig struct {
	Projects []FederationProjectConfig `toml:"project"`
}

// FederationProjectConfig declares one spoke-to-hub project mapping.
// Connection and authentication policy come from the named daemon catalog
// entry rather than being duplicated here.
type FederationProjectConfig struct {
	Hub          string `toml:"hub"`
	SpokeProject string `toml:"spoke_project"`
	HubProject   string `toml:"hub_project"`
	Actor        string `toml:"actor"`
}

// CatalogDaemon returns a copy of the named daemon catalog entry.
func (c *DaemonConfig) CatalogDaemon(name string) (CatalogDaemonConfig, bool) {
	for _, daemon := range c.Daemons {
		if daemon.Name == name {
			return daemon, true
		}
	}
	return CatalogDaemonConfig{}, false
}

func trimFederationConfig(cfg *DaemonConfig) {
	for i := range cfg.Federation.Projects {
		mapping := &cfg.Federation.Projects[i]
		mapping.Hub = strings.TrimSpace(mapping.Hub)
		mapping.SpokeProject = strings.TrimSpace(mapping.SpokeProject)
		mapping.HubProject = strings.TrimSpace(mapping.HubProject)
		mapping.Actor = strings.TrimSpace(mapping.Actor)
	}
}

func validateFederationConfig(cfg *DaemonConfig) error {
	spokeProjects := make(map[string]struct{}, len(cfg.Federation.Projects))
	hubTargets := make(map[string]struct{}, len(cfg.Federation.Projects))
	for i, mapping := range cfg.Federation.Projects {
		prefix := fmt.Sprintf("federation.project[%d]", i)
		if mapping.Hub == "" {
			return errors.New(prefix + ".hub is required")
		}
		if mapping.SpokeProject == "" {
			return errors.New(prefix + ".spoke_project is required")
		}
		if mapping.HubProject == "" {
			return errors.New(prefix + ".hub_project is required")
		}
		if mapping.Actor == "" {
			return errors.New(prefix + ".actor is required")
		}
		if err := ValidateProjectName(mapping.SpokeProject); err != nil {
			return fmt.Errorf("%s.spoke_project: %w", prefix, err)
		}
		if err := ValidateProjectName(mapping.HubProject); err != nil {
			return fmt.Errorf("%s.hub_project: %w", prefix, err)
		}

		catalog, ok := cfg.CatalogDaemon(mapping.Hub)
		if !ok {
			return fmt.Errorf("%s.hub %q is not in daemon catalog", prefix, mapping.Hub)
		}
		if catalog.Local || catalog.URL == "" {
			return fmt.Errorf("%s.hub %q must reference a remote daemon with url", prefix, mapping.Hub)
		}
		origin, err := CanonicalHTTPOrigin(catalog.URL)
		if err != nil {
			return fmt.Errorf("%s.hub %q url: %w", prefix, mapping.Hub, err)
		}

		if _, ok := spokeProjects[mapping.SpokeProject]; ok {
			return fmt.Errorf("%s: duplicate spoke_project %q", prefix, mapping.SpokeProject)
		}
		spokeProjects[mapping.SpokeProject] = struct{}{}

		targetKey := origin + "\x00" + mapping.HubProject
		if _, ok := hubTargets[targetKey]; ok {
			return fmt.Errorf("%s: duplicate hub target %q/%q", prefix, origin, mapping.HubProject)
		}
		hubTargets[targetKey] = struct{}{}
	}
	return nil
}
