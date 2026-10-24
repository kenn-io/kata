package config

import "fmt"

// CreateConfig holds CLI-only defaults from [create] in <KATA_HOME>/config.toml.
// A nil DefaultPriority leaves newly created issues without a priority.
type CreateConfig struct {
	DefaultPriority *int64 `toml:"default_priority"`
}

// ReadCreateConfig reads the invoking client's defaults without resolving
// daemon credentials or validating unrelated settings. Callers with an explicit
// priority flag need not read these defaults.
func ReadCreateConfig() (CreateConfig, error) {
	var file struct {
		Create CreateConfig `toml:"create"`
	}
	path, err := decodeConfigSection("create", &file)
	if err != nil {
		return CreateConfig{}, err
	}
	if priority := file.Create.DefaultPriority; priority != nil && (*priority < 0 || *priority > 4) {
		return CreateConfig{}, fmt.Errorf("parse %s: create.default_priority must be an integer between 0 and 4", path)
	}
	return file.Create, nil
}
