package config

import (
	"fmt"
)

// TeammateConfig holds client-side attribution defaults from [teammate].
type TeammateConfig struct {
	Infer string `toml:"infer"`
}

// ReadTeammateConfig reads only the teammate section, defaulting an absent
// inference setting to model. An explicitly empty setting is invalid.
func ReadTeammateConfig() (TeammateConfig, error) {
	var file struct {
		Teammate TeammateConfig `toml:"teammate"`
	}
	file.Teammate.Infer = "model"
	path, err := decodeConfigSection("teammate", &file)
	if err != nil {
		return TeammateConfig{}, err
	}
	switch file.Teammate.Infer {
	case "model", "harness", "off":
	default:
		return TeammateConfig{}, fmt.Errorf("parse %s: teammate.infer: invalid teammate inference mode %q: must be model, harness or off", path, file.Teammate.Infer)
	}
	return file.Teammate, nil
}
