package tickticksync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidateID restricts TickTick identities to one plain URL path segment.
func ValidateID(id string) error {
	if !identityPattern.MatchString(id) {
		return fmt.Errorf("TickTick identity must be a plain project or task ID")
	}
	return nil
}
func normalizeConfig(c Config) (Config, error) {
	if err := ValidateID(c.ProjectID); err != nil {
		return c, err
	}
	if c.StatusSync == "" {
		c.StatusSync = "one-way"
	}
	if c.StatusSync != "one-way" && c.StatusSync != "two-way" {
		return c, fmt.Errorf("TickTick status_sync must be one-way or two-way")
	}
	if c.TitlePrefix == nil {
		c.TitlePrefix = new(true)
	}
	return c, nil
}

// EncodeConfig validates and serializes public operator settings.
func EncodeConfig(c Config) (jsontext.Value, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// DecodeConfig validates public settings while excluding private worker state.
func DecodeConfig(raw jsontext.Value) (Config, error) {
	public, err := db.PublicIssueSyncConfig(raw)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err = json.Unmarshal(public, &c, json.RejectUnknownMembers(true)); err != nil {
		return c, fmt.Errorf("invalid TickTick binding config")
	}
	return normalizeConfig(c)
}

// DecodeCheckpoint validates durable task versions and missing-task position.
func DecodeCheckpoint(raw jsontext.Value) (Checkpoint, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Checkpoint{}, fmt.Errorf("invalid TickTick checkpoint container")
	}
	cp := Checkpoint{Versions: map[string]TaskVersion{}}
	if value, ok := fields[checkpointKey]; ok {
		if len(value) > maxCheckpointBytes || !strings.HasPrefix(strings.TrimSpace(string(value)), "{") || json.Unmarshal(value, &cp, json.RejectUnknownMembers(true)) != nil {
			return cp, fmt.Errorf("invalid TickTick checkpoint")
		}
	}
	if cp.Versions == nil {
		cp.Versions = map[string]TaskVersion{}
	}
	if len(cp.Versions) > maxTasks {
		return cp, fmt.Errorf("TickTick checkpoint exceeds 10000 tasks")
	}
	if cp.MissingAfter != "" {
		if err := ValidateID(cp.MissingAfter); err != nil {
			return cp, err
		}
	}
	for id, v := range cp.Versions {
		if ValidateID(id) != nil || len(v.Hash) != 64 || !validTime(v.FirstSeen) || !validTime(v.Version) || v.Version.Before(v.FirstSeen) {
			return cp, fmt.Errorf("invalid TickTick task version")
		}
	}
	return cp, nil
}

// WithCheckpoint stages a size-bounded private checkpoint in binding config.
func WithCheckpoint(raw jsontext.Value, cp Checkpoint) (jsontext.Value, error) {
	value, err := json.Marshal(cp, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if len(value) > maxCheckpointBytes {
		return nil, fmt.Errorf("TickTick checkpoint exceeds 2 MiB")
	}
	var fields map[string]jsontext.Value
	if err = json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("invalid TickTick checkpoint container")
	}
	fields[checkpointKey] = value
	return json.Marshal(fields, json.Deterministic(true))
}
func validTime(at time.Time) bool { return !at.IsZero() && at.Year() > 0 && at.Year() < 10000 }
