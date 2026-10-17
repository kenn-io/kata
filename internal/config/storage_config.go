package config

// readStorageConfig decodes the whole TOML file so syntax errors and unknown
// keys remain fatal, without validating unrelated daemon settings.
func readStorageConfig() (StorageConfig, error) {
	cfg, err := ReadConfigSections("storage")
	if err != nil {
		return StorageConfig{}, err
	}
	return cfg.Storage, nil
}
