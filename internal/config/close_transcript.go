package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kata/internal/transcript"
)

// CloseTranscriptConfig is a client preference, read from the invoking
// client's Kata home even when it targets a remote daemon.
type CloseTranscriptConfig struct {
	Enabled       bool              `toml:"enabled"`
	AgentsViewURL string            `toml:"agentsview_url"`
	SessionIDEnv  map[string]string `toml:"session_id_env"`
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
		return CloseTranscriptConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var file struct {
		Close struct {
			Transcript CloseTranscriptConfig `toml:"transcript"`
		} `toml:"close"`
	}
	if _, err = toml.Decode(string(data), &file); err != nil {
		return CloseTranscriptConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return checkSessionIDEnv(path, file.Close.Transcript)
}

// checkSessionIDEnv rejects agent names the API would refuse. Only the client
// checks them, so a typo here never stops the daemon.
func checkSessionIDEnv(path string, cfg CloseTranscriptConfig) (CloseTranscriptConfig, error) {
	for agent := range cfg.SessionIDEnv {
		if !transcript.ValidAgent(agent) {
			return CloseTranscriptConfig{}, fmt.Errorf("%s: close.transcript.session_id_env key %q must be a lowercase alphanumeric or hyphen token starting with a letter or digit, at most 32 characters", path, agent)
		}
	}
	return cfg, nil
}
