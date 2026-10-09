package config

import (
	"errors"
	"os"

	"github.com/BurntSushi/toml"
)

// CloseTranscriptConfig is a client preference, read from the invoking
// client's Kata home even when it targets a remote daemon.
type CloseTranscriptConfig struct {
	Enabled       bool   `toml:"enabled"`
	AgentsViewURL string `toml:"agentsview_url"`
}

// ReadCloseTranscriptConfig avoids daemon policy and credential resolution.
func ReadCloseTranscriptConfig() (CloseTranscriptConfig, error) {
	path, err := DaemonConfigPath()
	if err != nil {
		return CloseTranscriptConfig{}, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- derived from KATA_HOME
	if errors.Is(err, os.ErrNotExist) {
		return CloseTranscriptConfig{}, nil
	}
	if err != nil {
		return CloseTranscriptConfig{}, errors.New("cannot read close transcript configuration")
	}
	var file struct {
		Close struct {
			Transcript CloseTranscriptConfig `toml:"transcript"`
		} `toml:"close"`
	}
	if _, err = toml.Decode(string(data), &file); err != nil {
		return CloseTranscriptConfig{}, errors.New("cannot parse close transcript configuration")
	}
	return file.Close.Transcript, nil
}
