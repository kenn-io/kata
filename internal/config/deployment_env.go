package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// applyDeploymentEnv overlays container settings before normal validation.
// Empty values leave TOML settings intact, including empty Compose substitutions.
func applyDeploymentEnv(cfg *DaemonConfig) error {
	for name, field := range map[string]*string{
		"KATA_LISTEN":                         &cfg.Listen,
		"KATA_WEB_LISTEN":                     &cfg.Web.Listen,
		"KATA_WEB_PUBLIC_ORIGIN":              &cfg.Web.PublicOrigin,
		"KATA_BACKUP_DIR":                     &cfg.Backup.Dir,
		"KATA_BACKUP_INTERVAL":                &cfg.Backup.Interval,
		"KATA_BACKUP_RETAIN":                  &cfg.Backup.Retain,
		"KATA_SEARCH_EMBEDDINGS_BASE_URL":     &cfg.Search.Embeddings.BaseURL,
		"KATA_SEARCH_EMBEDDINGS_MODEL":        &cfg.Search.Embeddings.Model,
		"KATA_SEARCH_EMBEDDINGS_API_KEY_FILE": &cfg.Search.Embeddings.APIKeyFile,
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			*field = value
			cfg.Sources[deploymentSourceKey(name)] = name
		}
	}
	if value := strings.TrimSpace(os.Getenv("KATA_SEARCH_EMBEDDINGS_DIMS")); value != "" {
		dims, err := strconv.Atoi(value)
		if err != nil || dims < 0 {
			return fmt.Errorf("KATA_SEARCH_EMBEDDINGS_DIMS must be a non-negative integer")
		}
		cfg.Search.Embeddings.Dims = dims
		cfg.Sources["search_embeddings_dims"] = "KATA_SEARCH_EMBEDDINGS_DIMS"
	}
	return nil
}

func deploymentSourceKey(name string) string {
	return strings.ToLower(strings.TrimPrefix(name, "KATA_"))
}

func initializeDeploymentSources(cfg *DaemonConfig) {
	cfg.Sources = map[string]string{
		"listen": "platform default", "web_listen": "default", "web_public_origin": "default",
	}
	for key, value := range map[string]string{
		"listen":                         cfg.Listen,
		"web_listen":                     cfg.Web.Listen,
		"web_public_origin":              cfg.Web.PublicOrigin,
		"search_embeddings_base_url":     cfg.Search.Embeddings.BaseURL,
		"search_embeddings_model":        cfg.Search.Embeddings.Model,
		"search_embeddings_api_key_file": cfg.Search.Embeddings.APIKeyFile,
	} {
		if value != "" {
			cfg.Sources[key] = "config.toml"
		}
	}
}
