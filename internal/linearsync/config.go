package linearsync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

var uuidPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{32}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// CanonicalID normalizes compact or hyphenated UUIDs to lowercase with hyphens.
func CanonicalID(value string) (string, error) {
	if !uuidPattern.MatchString(value) {
		return "", fmt.Errorf("linear identity must be a UUID")
	}
	s := strings.ToLower(strings.ReplaceAll(value, "-", ""))
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// ParseSince reads a date or whole-second RFC3339 cutoff and normalizes it to UTC.
func ParseSince(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		at, err = time.Parse(time.DateOnly, value)
	}
	if err != nil || strings.ContainsAny(value, ".,") || !validTime(at) {
		return nil, fmt.Errorf("linear since requires a UTC date or whole-second RFC3339 timestamp")
	}
	at = at.UTC()
	return &at, nil
}
func normalizeConfig(c Config) (Config, error) {
	for _, id := range []*string{&c.WorkspaceID, &c.TeamID, &c.ProjectID, &c.ClosedStateID, &c.OpenStateID} {
		if *id == "" && (id == &c.ProjectID || id == &c.ClosedStateID || id == &c.OpenStateID) {
			continue
		}
		var err error
		*id, err = CanonicalID(*id)
		if err != nil {
			return Config{}, err
		}
	}
	if c.StatusSync == "" {
		c.StatusSync = "one-way"
	}
	if c.StatusSync != "one-way" && c.StatusSync != "two-way" {
		return Config{}, fmt.Errorf("linear status_sync must be one-way or two-way")
	}
	at, err := ParseSince(c.Since)
	if err != nil {
		return Config{}, err
	}
	c.Since = ""
	if at != nil {
		c.Since = at.Format(time.RFC3339)
	}
	if c.TitlePrefix == nil {
		c.TitlePrefix = new(true)
	}
	return c, nil
}

// EncodeConfig validates and serializes non-secret binding options.
func EncodeConfig(c Config) (jsontext.Value, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// DecodeConfig reads validated options without exposing private status-scan state.
func DecodeConfig(raw jsontext.Value) (Config, error) {
	if _, err := db.DecodeIssueStatusScan(raw); err != nil {
		return Config{}, fmt.Errorf("invalid Linear config JSON")
	}
	public, err := db.PublicIssueSyncConfig(raw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid Linear config JSON")
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(public, &fields) != nil || fields == nil {
		return Config{}, fmt.Errorf("invalid Linear config JSON")
	}
	for _, key := range []string{"status_sync", "title_prefix"} {
		if v, ok := fields[key]; ok && (string(v) == "null" || key == "status_sync" && string(v) == `""`) {
			return Config{}, fmt.Errorf("invalid Linear config JSON")
		}
	}
	var c Config
	if json.Unmarshal(public, &c, json.RejectUnknownMembers(true)) != nil {
		return Config{}, fmt.Errorf("invalid Linear config JSON")
	}
	return normalizeConfig(c)
}
func validTime(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}
