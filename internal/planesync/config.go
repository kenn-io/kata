package planesync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

var uuidPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{32}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
var workspacePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// CanonicalID accepts only plain UUIDs, keeping path segments unambiguous.
func CanonicalID(value string) (string, error) {
	if !uuidPattern.MatchString(value) {
		return "", fmt.Errorf("plane identity must be a UUID")
	}
	s := strings.ToLower(strings.ReplaceAll(value, "-", ""))
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// ParseSince accepts UTC dates and whole-second RFC3339 cutoffs.
func ParseSince(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		at, err = time.Parse(time.DateOnly, value)
	}
	if err != nil || strings.ContainsAny(value, ".,") || at.UTC().Year() < 1 || at.UTC().Year() > 9999 {
		return nil, fmt.Errorf("plane since requires a UTC date or whole-second RFC3339 timestamp")
	}
	at = at.UTC()
	return &at, nil
}

func normalizeConfig(c Config) (Config, error) {
	if c.StatusSync == "" {
		c.StatusSync = "one-way"
	}
	if c.StatusSync != "one-way" && c.StatusSync != "two-way" {
		return Config{}, fmt.Errorf("plane status_sync must be one-way or two-way")
	}
	for _, id := range []*string{&c.ClosedStateID, &c.OpenStateID} {
		if *id != "" {
			canonical, err := CanonicalID(*id)
			if err != nil {
				return Config{}, err
			}
			*id = canonical
		}
	}

	var err error
	if c.APIOrigin == "" || c.WebOrigin == "" {
		return Config{}, fmt.Errorf("plane config requires API and web origins")
	}
	if c.APIOrigin, err = config.CanonicalPlaneOrigin(c.APIOrigin); err != nil {
		return Config{}, err
	}
	if c.WebOrigin, err = config.CanonicalPlaneOrigin(c.WebOrigin); err != nil {
		return Config{}, err
	}
	if err := ValidateWorkspace(c.Workspace); err != nil {
		return Config{}, err
	}
	if c.ProjectID, err = CanonicalID(c.ProjectID); err != nil {
		return Config{}, err
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

// EncodeConfig returns canonical stable configuration JSON.
func EncodeConfig(c Config) (jsontext.Value, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("cannot encode Plane config")
	}
	return raw, nil
}

// DecodeConfig accepts only public binding fields and redacts invalid input.
func DecodeConfig(raw jsontext.Value) (Config, error) {
	if _, err := db.DecodeIssueStatusScan(raw); err != nil {
		return Config{}, fmt.Errorf("invalid Plane config JSON")
	}
	public, err := db.PublicIssueSyncConfig(raw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid Plane config JSON")
	}
	raw = public

	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Config{}, fmt.Errorf("invalid Plane config JSON")
	}
	if value, ok := fields["status_sync"]; ok && (string(value) == "null" || string(value) == `""`) {
		return Config{}, fmt.Errorf("invalid Plane config JSON")
	}
	if value, ok := fields["title_prefix"]; ok && string(value) == "null" {
		return Config{}, fmt.Errorf("invalid Plane config JSON")
	}
	var c Config
	if err := json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, fmt.Errorf("invalid Plane config JSON")
	}
	return normalizeConfig(c)
}

// ValidateWorkspace accepts an unambiguous Plane workspace path segment.
func ValidateWorkspace(value string) error {
	if !workspacePattern.MatchString(value) || len(value) > 255 {
		return fmt.Errorf("plane workspace must be a nonempty URL-safe slug")
	}
	return nil
}
