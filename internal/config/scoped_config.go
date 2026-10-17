package config

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// ReadConfigSections parses the whole file but validates only the named
// sections. Callers must name every section they consume. Daemon startup and
// doctor use ReadDaemonConfig instead to validate the complete configuration.
// Sections without semantic constraints (such as tui and storage) still get
// global TOML decoding and unknown-key checks.
func ReadConfigSections(sections ...string) (*DaemonConfig, error) {
	path, err := DaemonConfigPath()
	if err != nil {
		return nil, err
	}
	return readConfigSections(path, true, slices.Contains(sections, "auth"), sections, nil)
}

// CheckConfigFile rejects global parse errors and warns about invalid sections.
// Commands subsequently validate the sections they use through their loaders.
// This preflight never resolves credentials or reads daemon-only secret files.
func CheckConfigFile(warnings io.Writer) error {
	path, err := DaemonConfigPath()
	if err != nil {
		return err
	}
	_, err = readConfigSections(path, true, false, nil, warnings)
	return err
}

func validateConfigSections(cfg *DaemonConfig, home string, credentials bool, sections []string, warnings io.Writer, deploymentErr error) error {
	validators := []struct {
		section  string
		validate func() error
	}{
		{"auth", func() error {
			if credentials {
				if err := resolveAuthCredential(&cfg.Auth); err != nil {
					return err
				}
				if cfg.Sources["auth"] != "KATA_AUTH_TOKEN" {
					cfg.Sources["auth"], _, _ = strings.Cut(cfg.Auth.Source, ":")
				}
			}
			return nil
		}},
		{"auth.proxy", func() error { return validateAuthProxy(cfg.Auth.Proxy) }},
		{"autostart_idle_timeout", func() error { _, err := cfg.AutostartIdleTimeoutDuration(); return err }},
		{"backup", func() error {
			cfg.Backup.Dir = strings.TrimSpace(cfg.Backup.Dir)
			_, _, err := cfg.Backup.Durations()
			return err
		}},
		{"timezone", func() error {
			if cfg.Timezone == "" {
				return nil
			}
			if _, err := time.LoadLocation(cfg.Timezone); err != nil {
				return fmt.Errorf("timezone %q is not a valid IANA timezone: %w", cfg.Timezone, err)
			}
			return nil
		}},
		{"web", func() error { return normalizeWebConfig(&cfg.Web, cfg.Auth.TrustPrivateNetwork) }},
		{"close", func() error { _, err := cfg.Close.Throttle.ThrottleWindow(); return err }},
		{"search", func() error {
			if deploymentErr != nil {
				return deploymentErr
			}
			return validateEmbeddings(cfg.Search.Embeddings)
		}},
		{"github_sync", func() error { return validateGitHubSync(cfg.GitHubSync) }},
		{"notion_sync", func() (err error) { cfg.NotionSync, err = NormalizeNotionSyncConfig(cfg.NotionSync); return }},
		{"plane_sync", func() (err error) { cfg.PlaneSync, err = NormalizePlaneSyncConfig(cfg.PlaneSync); return }},
		{"linear_sync", func() (err error) { cfg.LinearSync, err = NormalizeLinearSyncConfig(cfg.LinearSync); return }},
		{"twenty_sync", func() (err error) { cfg.TwentySync, err = NormalizeTwentySyncConfig(cfg.TwentySync); return }},
		{"todoist_sync", func() (err error) { cfg.TodoistSync, err = NormalizeTodoistSyncConfig(cfg.TodoistSync); return }},
		{"ticktick_sync", func() (err error) { cfg.TickTickSync, err = NormalizeTickTickSyncConfig(cfg.TickTickSync); return }},
		{"connector", func() (err error) { cfg.Connectors, err = NormalizeConnectorConfigs(cfg.Connectors); return }},
		{"daemon", func() error { return normalizeDaemonCatalog(cfg) }},
		{"federation", func() error {
			if err := normalizeFederationSigning(cfg, home); err != nil {
				return err
			}
			return validateFederationConfig(cfg)
		}},
	}
	for _, v := range validators {
		required := slices.Contains(sections, "*") || slices.Contains(sections, v.section)
		if !required && warnings == nil {
			continue
		}
		if err := v.validate(); err != nil {
			if required {
				return err
			}
			// Print only the section name: validation errors may contain URLs or
			// credential-bearing operator input. The complete error remains available
			// from doctor and from any command that consumes this section.
			if _, err := fmt.Fprintf(warnings, "kata: warning: invalid config section [%s]; run kata doctor for details\n", v.section); err != nil {
				return err
			}
		}
	}
	return nil
}
