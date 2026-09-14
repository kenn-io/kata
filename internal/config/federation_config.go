package config

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/httpurl"

	"go.kenn.io/kata/pkg/federationprovider"
)

// FederationConfig is the [federation] block of <KATA_HOME>/config.toml.
type FederationConfig struct {
	Projects []FederationProjectConfig `toml:"project"`
}

// FederationProjectConfig declares one spoke-to-hub project mapping.
// The named daemon catalog entry supplies the endpoint. Authentication uses
// that catalog entry or the mapping's credential provider, never both.
type FederationProjectConfig struct {
	Hub                string                    `toml:"hub"`
	SpokeProject       string                    `toml:"spoke_project"`
	HubProject         string                    `toml:"hub_project"`
	Actor              string                    `toml:"actor"`
	Intent             federationprovider.Intent `toml:"intent"`
	CredentialProvider []string                  `toml:"credential_provider"`
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
		mapping.Intent = federationprovider.Intent(strings.TrimSpace(string(mapping.Intent)))
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
		if mapping.CredentialProvider == nil && mapping.Intent == "" && mapping.Actor == "" {
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
		if err := ValidateFederationAuthentication(mapping, catalog); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		baseURL, err := httpurl.CanonicalHTTPBaseURL(catalog.URL)
		if err != nil {
			return fmt.Errorf("%s.hub %q url: %w", prefix, mapping.Hub, err)
		}

		if _, ok := spokeProjects[mapping.SpokeProject]; ok {
			return fmt.Errorf("%s: duplicate spoke_project %q", prefix, mapping.SpokeProject)
		}
		spokeProjects[mapping.SpokeProject] = struct{}{}

		targetKey := baseURL + "\x00" + mapping.HubProject
		if _, ok := hubTargets[targetKey]; ok {
			return fmt.Errorf("%s: duplicate hub target %q/%q", prefix, baseURL, mapping.HubProject)
		}
		hubTargets[targetKey] = struct{}{}
	}
	return nil
}

// ValidateFederationAuthentication keeps provider and catalog authority
// mutually exclusive. Runtime callers use the same checks as the TOML loader.
func ValidateFederationAuthentication(mapping FederationProjectConfig, catalog CatalogDaemonConfig) error {
	if mapping.CredentialProvider == nil {
		if mapping.Intent != "" {
			return errors.New("intent requires credential_provider")
		}
		return nil
	}
	if len(mapping.CredentialProvider) == 0 || strings.TrimSpace(mapping.CredentialProvider[0]) == "" {
		return errors.New("credential_provider requires an executable and optional arguments")
	}
	for _, arg := range mapping.CredentialProvider {
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("credential_provider arguments cannot contain NUL")
		}
	}
	if mapping.Intent != federationprovider.IntentReadOnly && mapping.Intent != federationprovider.IntentCollaborate && mapping.Intent != federationprovider.IntentMigrate {
		return errors.New("intent must be read_only, collaborate, or migrate")
	}
	if mapping.Actor != "" {
		return errors.New("actor must be supplied by the credential_provider")
	}
	if catalog.Token != "" || catalog.TokenEnv != "" {
		return errors.New("credential_provider cannot use a catalog token or token_env")
	}
	if !strings.HasPrefix(catalog.URL, "https://") {
		return errors.New("credential_provider requires an HTTPS hub URL")
	}
	return nil
}
