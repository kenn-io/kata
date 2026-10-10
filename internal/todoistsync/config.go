package todoistsync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// ValidateID requires a Todoist ID. IDs are opaque strings; requests escape them.
func ValidateID(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("todoist ID is required")
	}
	return nil
}

// ParseHistorySince accepts a date or whole-second instant and returns a UTC floor.
func ParseHistorySince(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		at, err = time.Parse(time.DateOnly, value)
	}
	if err != nil || strings.ContainsAny(value, ".,") {
		return time.Time{}, fmt.Errorf("todoist history_since requires a UTC date or whole-second RFC3339 instant")
	}
	return at.UTC(), nil
}
func normalizeConfig(c Config) (Config, error) {
	origin, err := config.NormalizeTodoistSyncConfig(config.TodoistSyncConfig{APIOrigin: c.APIOrigin})
	if err != nil {
		return Config{}, err
	}
	c.APIOrigin = origin.APIOrigin
	for _, id := range []string{c.AccountID, c.ProjectID} {
		if err := ValidateID(id); err != nil {
			return Config{}, err
		}
	}
	at, err := ParseHistorySince(c.HistorySince)
	if err != nil {
		return Config{}, err
	}
	c.HistorySince = at.Format(time.RFC3339)
	if c.StatusSync == "" {
		c.StatusSync = "one-way"
	}
	if c.StatusSync != "one-way" && c.StatusSync != "two-way" {
		return Config{}, fmt.Errorf("todoist status_sync must be one-way or two-way")
	}
	if c.TitlePrefix == nil {
		c.TitlePrefix = new(true)
	}
	return c, nil
}

// EncodeConfig writes the normalized, non-secret binding configuration.
func EncodeConfig(c Config) (jsontext.Value, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// DecodeConfig validates source config and accepts the shared private status checkpoint.
func DecodeConfig(raw jsontext.Value) (Config, error) {
	bad := func() (Config, error) { return Config{}, fmt.Errorf("invalid Todoist binding config") }
	if _, err := db.DecodeIssueStatusScan(raw); err != nil {
		return bad()
	}
	public, err := db.PublicIssueSyncConfig(raw)
	if err != nil {
		return bad()
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(public, &fields) != nil || fields == nil {
		return bad()
	}
	for _, key := range []string{"title_prefix", "status_sync"} {
		if v, ok := fields[key]; ok && (string(v) == "null" || string(v) == `""`) {
			return bad()
		}
	}
	var c Config
	if json.Unmarshal(public, &c, json.RejectUnknownMembers(true)) != nil {
		return bad()
	}
	c, err = normalizeConfig(c)
	if err != nil {
		return bad()
	}
	return c, nil
}

// historyFloor is the normalized history_since instant.
func (c Config) historyFloor() time.Time {
	at, _ := ParseHistorySince(c.HistorySince)
	return at
}
