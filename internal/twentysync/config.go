package twentysync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

var uuidPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{32}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// CanonicalID accepts only UUIDs that are safe as fixed API path segments.
func CanonicalID(value string) (string, error) {
	if !uuidPattern.MatchString(value) {
		return "", fmt.Errorf("twenty identity must be a UUID")
	}
	s := strings.ToLower(strings.ReplaceAll(value, "-", ""))
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// ParseSince accepts exclusive UTC dates and whole-second RFC3339 cutoffs.
func ParseSince(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		at, err = time.Parse(time.DateOnly, value)
	}
	if err != nil || strings.ContainsAny(value, ".,") || !validSourceTime(at) {
		return nil, fmt.Errorf("twenty since requires a UTC date or whole-second RFC3339 timestamp")
	}
	return new(at.UTC()), nil
}

// ValidStatusValue accepts a nonempty single-line API option value.
func ValidStatusValue(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 255 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}

func normalizeConfig(c Config) (Config, error) {
	return normalizeSourceConfig(c, true)
}

// NormalizeDiscoveryConfig validates enable options before reading credentials.
// Workspace identity may be absent until the API key's workspace is discovered.
func NormalizeDiscoveryConfig(c Config) (Config, error) {
	return normalizeSourceConfig(c, false)
}

// Discovery sessions may read workspace identity before a binding exists.
func normalizeSourceConfig(c Config, requireWorkspace bool) (Config, error) {
	var err error
	if c.APIOrigin, err = config.CanonicalTwentyOrigin(c.APIOrigin); err != nil {
		return Config{}, err
	}
	if c.WebOrigin, err = config.CanonicalTwentyOrigin(c.WebOrigin); err != nil {
		return Config{}, err
	}
	if requireWorkspace || c.WorkspaceID != "" {
		if c.WorkspaceID, err = CanonicalID(c.WorkspaceID); err != nil {
			return Config{}, err
		}
	}
	if c.StatusSync == "" {
		c.StatusSync = "one-way"
	}
	if c.StatusSync != "one-way" && c.StatusSync != "two-way" {
		return Config{}, fmt.Errorf("twenty status_sync must be one-way or two-way")
	}
	if c.ClosedStatus == "" {
		c.ClosedStatus = "DONE"
	}
	if c.OpenStatus == "" {
		c.OpenStatus = "TODO"
	}
	if c.OpenStatuses == nil {
		c.OpenStatuses = []string{"TODO", "IN_PROGRESS"}
	}
	c.OpenStatuses = slices.Clone(c.OpenStatuses)
	if len(c.OpenStatuses) == 0 || len(c.OpenStatuses) > maxItems || !ValidStatusValue(c.ClosedStatus) || !ValidStatusValue(c.OpenStatus) {
		return Config{}, fmt.Errorf("invalid Twenty status mapping")
	}
	slices.Sort(c.OpenStatuses)
	for i, value := range c.OpenStatuses {
		if !ValidStatusValue(value) || value == c.ClosedStatus || (i > 0 && value == c.OpenStatuses[i-1]) {
			return Config{}, fmt.Errorf("ambiguous or invalid Twenty status mapping")
		}
	}
	if !slices.Contains(c.OpenStatuses, c.OpenStatus) {
		return Config{}, fmt.Errorf("twenty open_status must be classified open")
	}
	if c.TitlePrefix == nil {
		c.TitlePrefix = new(true)
	}
	at, err := ParseSince(c.Since)
	if err != nil {
		return Config{}, err
	}
	c.Since = ""
	if at != nil {
		c.Since = at.Format(time.RFC3339)
	}
	return c, nil
}

// EncodeConfig returns canonical nonsecret binding configuration.
func EncodeConfig(c Config) (jsontext.Value, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("cannot encode Twenty binding config")
	}
	return raw, nil
}

// DecodeConfig rejects unknown fields while allowing private runner progress.
func DecodeConfig(raw jsontext.Value) (Config, error) {
	if _, err := db.DecodeIssueStatusScan(raw); err != nil {
		return Config{}, fmt.Errorf("invalid Twenty config JSON")
	}
	public, err := db.PublicIssueSyncConfig(raw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid Twenty config JSON")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(public, &fields); err != nil || fields == nil {
		return Config{}, fmt.Errorf("invalid Twenty config JSON")
	}
	for _, key := range []string{"status_sync", "title_prefix", "open_status", "closed_status", "open_statuses"} {
		if value, ok := fields[key]; ok && (string(value) == "null" || string(value) == `""`) {
			return Config{}, fmt.Errorf("invalid Twenty config JSON")
		}
	}
	var c Config
	if err := json.Unmarshal(public, &c, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, fmt.Errorf("invalid Twenty config JSON")
	}
	return normalizeConfig(c)
}

// ClassifyStatus uses saved inbound policy, never mutable display labels.
func ClassifyStatus(c Config, value *string) (string, string, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return "", "", err
	}
	return classifyStatus(c, value)
}

func classifyStatus(c Config, value *string) (string, string, error) {
	if value == nil {
		return "open", "", nil
	}
	if *value == c.ClosedStatus {
		return "closed", "done", nil
	}
	if slices.Contains(c.OpenStatuses, *value) {
		return "open", "", nil
	}
	return "", "", fmt.Errorf("unmapped Twenty task status; configure open_statuses or closed_status")
}

// ValidateSchema prevents unclassified live options and unsafe write targets.
func ValidateSchema(c Config, schema Schema) error {
	c, err := normalizeConfig(c)
	if err != nil {
		return err
	}
	if len(schema.StatusOptions) == 0 || len(schema.StatusOptions) > maxItems {
		return fmt.Errorf("twenty task status schema has no options or exceeds limits")
	}
	seen := map[string]bool{}
	for _, value := range schema.StatusOptions {
		if !ValidStatusValue(value) || seen[value] {
			return fmt.Errorf("invalid or duplicate Twenty status option")
		}
		seen[value] = true
		if _, _, err := classifyStatus(c, &value); err != nil {
			return err
		}
	}
	if c.StatusSync == "two-way" && (!seen[c.OpenStatus] || !seen[c.ClosedStatus]) {
		return fmt.Errorf("twenty two-way write targets are missing from the live status schema")
	}
	return nil
}
