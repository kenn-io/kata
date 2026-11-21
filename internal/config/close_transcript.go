package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kata/internal/transcript"
)

// CloseTranscriptMode says when a close attaches the closing session.
type CloseTranscriptMode int

const (
	// CloseTranscriptOff attaches nothing.
	CloseTranscriptOff CloseTranscriptMode = iota
	// CloseTranscriptOn attaches the closing session and warns when none is
	// available.
	CloseTranscriptOn
	// CloseTranscriptAuto attaches the closing session when one is named and
	// stays quiet when none is.
	CloseTranscriptAuto
)

// CloseTranscriptConfig is a client preference, read from the invoking
// client's Kata home even when it targets a remote daemon.
type CloseTranscriptConfig struct {
	Mode          CloseTranscriptMode
	AgentsViewURL string
	SessionIDEnv  map[string]string
	Project       map[string]CloseTranscriptTarget
	override      *CloseTranscriptMode
}

// CloseTranscriptTarget configures attachment and the locator for a project.
// An omitted Mode inherits the global mode.
type CloseTranscriptTarget struct {
	Mode          *CloseTranscriptMode
	AgentsViewURL string
}

// closeTranscriptFile is [close.transcript] as written. The daemon decodes it
// only to accept its keys; enabled stays raw so only kata close checks it.
type closeTranscriptFile struct {
	Enabled       any                                  `toml:"enabled"`
	AgentsViewURL string                               `toml:"agentsview_url"`
	SessionIDEnv  map[string]string                    `toml:"session_id_env"`
	Project       map[string]closeTranscriptFileTarget `toml:"project"`
}

type closeTranscriptFileTarget struct {
	Enabled       any    `toml:"enabled"`
	AgentsViewURL string `toml:"agentsview_url"`
}

// Target selects the project's settings, applying the process override.
func (cfg CloseTranscriptConfig) Target(project string) CloseTranscriptTarget {
	target, ok := cfg.Project[project]
	if !ok {
		target.AgentsViewURL = cfg.AgentsViewURL
	}
	if cfg.override != nil {
		target.Mode = cfg.override
	} else if target.Mode == nil {
		target.Mode = &cfg.Mode
	}
	return target
}

// AnyEnabled reports whether any configured target can record a session.
func (cfg CloseTranscriptConfig) AnyEnabled() bool {
	return cfg.anyMode(func(m CloseTranscriptMode) bool { return m != CloseTranscriptOff })
}

// AnyOn reports whether any configured target is true, so a close that names
// no session should warn. Auto targets never warn about a missing session.
func (cfg CloseTranscriptConfig) AnyOn() bool {
	return cfg.anyMode(func(m CloseTranscriptMode) bool { return m == CloseTranscriptOn })
}

func (cfg CloseTranscriptConfig) anyMode(match func(CloseTranscriptMode) bool) bool {
	if cfg.override != nil {
		return match(*cfg.override)
	}
	if match(cfg.Mode) {
		return true
	}
	for name := range cfg.Project {
		if match(*cfg.Target(name).Mode) {
			return true
		}
	}
	return false
}

// ForceEnabled reports whether the process explicitly enabled recording, with
// true or auto.
func (cfg CloseTranscriptConfig) ForceEnabled() bool {
	return cfg.override != nil && *cfg.override != CloseTranscriptOff
}

// ReadCloseTranscriptConfig avoids daemon policy and credential resolution.
// KATA_CLOSE_TRANSCRIPT_ENABLED (true, false or auto) overrides the file's
// enabled setting for one process; false skips the file, so its transcript
// settings cannot warn.
func ReadCloseTranscriptConfig() (CloseTranscriptConfig, error) {
	value := strings.TrimSpace(os.Getenv("KATA_CLOSE_TRANSCRIPT_ENABLED"))
	if value == "" {
		return readCloseTranscriptFile()
	}
	mode := CloseTranscriptAuto
	if !strings.EqualFold(value, "auto") {
		on, err := strconv.ParseBool(value)
		if err != nil {
			return CloseTranscriptConfig{}, errors.New("KATA_CLOSE_TRANSCRIPT_ENABLED must be true, false or auto")
		}
		if !on {
			return CloseTranscriptConfig{}, nil
		}
		mode = CloseTranscriptOn
	}
	cfg, err := readCloseTranscriptFile()
	if err != nil {
		return CloseTranscriptConfig{}, err
	}
	cfg.Mode = mode
	cfg.override = &mode
	return cfg, nil
}

func readCloseTranscriptFile() (CloseTranscriptConfig, error) {
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
			Transcript closeTranscriptFile `toml:"transcript"`
		} `toml:"close"`
	}
	if _, err = toml.Decode(string(data), &file); err != nil {
		return CloseTranscriptConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	raw := file.Close.Transcript
	cfg := CloseTranscriptConfig{AgentsViewURL: raw.AgentsViewURL, SessionIDEnv: raw.SessionIDEnv}
	if mode, err := fileCloseTranscriptMode(path, "close.transcript.enabled", raw.Enabled); err != nil {
		return CloseTranscriptConfig{}, err
	} else if mode != nil {
		cfg.Mode = *mode
	}
	for name, target := range raw.Project {
		mode, err := fileCloseTranscriptMode(path, "close.transcript.project."+name+".enabled", target.Enabled)
		if err != nil {
			return CloseTranscriptConfig{}, err
		}
		if cfg.Project == nil {
			cfg.Project = map[string]CloseTranscriptTarget{}
		}
		cfg.Project[name] = CloseTranscriptTarget{Mode: mode, AgentsViewURL: target.AgentsViewURL}
	}
	return checkSessionIDEnv(path, cfg)
}

// fileCloseTranscriptMode reads an enabled value as written in the file. It
// returns nil when the key is absent.
func fileCloseTranscriptMode(path, key string, value any) (*CloseTranscriptMode, error) {
	var mode CloseTranscriptMode
	switch value {
	case nil:
		return nil, nil
	case false:
		mode = CloseTranscriptOff
	case true:
		mode = CloseTranscriptOn
	default:
		if s, ok := value.(string); !ok || !strings.EqualFold(s, "auto") {
			return nil, fmt.Errorf("%s: %s must be true, false or \"auto\"", path, key)
		}
		mode = CloseTranscriptAuto
	}
	return &mode, nil
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
