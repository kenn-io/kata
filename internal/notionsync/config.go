package notionsync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

var uuidPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{32}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
var locatorSuffixPattern = regexp.MustCompile(`(?:^|-)([0-9a-fA-F]{32}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

func canonicalID(value string) (string, error) {
	if !uuidPattern.MatchString(value) {
		return "", fmt.Errorf("notion identity must be a UUID")
	}
	compact := strings.ToLower(strings.ReplaceAll(value, "-", ""))
	return compact[:8] + "-" + compact[8:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:], nil
}

// ParseDatabaseLocator parses convenience URLs locally, without fetching them.
func ParseDatabaseLocator(value string) (string, error) {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("invalid Notion database locator")
	}
	value = strings.TrimSpace(value)
	if id, err := canonicalID(value); err == nil {
		return id, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return "", fmt.Errorf("invalid Notion database locator")
	}
	switch strings.ToLower(u.Hostname()) {
	case "notion.so", "www.notion.so", "notion.com", "www.notion.com", "app.notion.com":
	default:
		return "", fmt.Errorf("notion database URL must use a supported Notion host")
	}
	if u.Port() != "" && u.Port() != "443" {
		return "", fmt.Errorf("notion database URL requires the default HTTPS port")
	}
	// Reject encoded paths and multiple UUID-bearing segments: a view or nested
	// object path must not accidentally become the selected database identity.
	if u.RawPath != "" || strings.Contains(u.EscapedPath(), "%") {
		return "", fmt.Errorf("invalid Notion database URL path")
	}
	segments := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	for _, segment := range segments[:len(segments)-1] {
		if segment == "" || segment == "." || segment == ".." || locatorSuffixPattern.MatchString(segment) {
			return "", fmt.Errorf("ambiguous Notion database URL path")
		}
	}
	match := locatorSuffixPattern.FindStringSubmatch(segments[len(segments)-1])
	if match == nil {
		return "", fmt.Errorf("notion database URL requires a final UUID")
	}
	return canonicalID(match[1])
}

// ParseSince accepts the same whole-second RFC3339 or UTC-date cutoff as GitHub.
func ParseSince(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil || strings.ContainsAny(value, ".,") {
		return nil, fmt.Errorf("notion sync since must be YYYY-MM-DD or RFC3339 with whole seconds")
	}
	parsed = parsed.UTC()
	if parsed.Year() < 0 || parsed.Year() > 9999 {
		return nil, fmt.Errorf("notion sync since must remain a four-digit RFC3339 year in UTC")
	}
	return &parsed, nil
}

func normalizeConfig(c Config) (Config, error) {
	var err error
	if c.DataSourceID, err = canonicalID(c.DataSourceID); err != nil {
		return Config{}, err
	}
	if c.DatabaseID, err = canonicalID(c.DatabaseID); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(c.TitlePropertyID) == "" || strings.TrimSpace(c.StatusPropertyID) == "" || strings.TrimSpace(c.AssigneePropertyID) == "" {
		return Config{}, fmt.Errorf("notion config requires title, status, and assignee property IDs")
	}
	if c.TitlePropertyID == c.StatusPropertyID || c.TitlePropertyID == c.AssigneePropertyID || c.StatusPropertyID == c.AssigneePropertyID {
		return Config{}, fmt.Errorf("notion selected property IDs must be distinct")
	}
	if len(c.DoneStatusIDs) == 0 {
		return Config{}, fmt.Errorf("notion config requires at least one completed status ID")
	}
	for _, id := range c.DoneStatusIDs {
		if strings.TrimSpace(id) == "" {
			return Config{}, fmt.Errorf("notion completed status IDs must be nonempty")
		}
	}
	c.DoneStatusIDs = slices.Clone(c.DoneStatusIDs)
	slices.Sort(c.DoneStatusIDs)
	c.DoneStatusIDs = slices.Compact(c.DoneStatusIDs)
	since, err := ParseSince(c.Since)
	if err != nil {
		return Config{}, err
	}
	c.Since = ""
	if since != nil {
		c.Since = since.Format(time.RFC3339)
	}
	if c.TitlePrefix == nil {
		c.TitlePrefix = new(true)
	}
	return c, nil
}

// EncodeConfig emits a validated canonical configuration in stable field order.
func EncodeConfig(c Config) (jsontext.Value, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("cannot encode Notion config")
	}
	return raw, nil
}

// DecodeConfig rejects unknown keys and never includes supplied values in errors.
func DecodeConfig(raw jsontext.Value) (Config, error) {
	// A missing choice defaults to true, but an explicit null is invalid.
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Config{}, fmt.Errorf("invalid Notion config JSON")
	}
	if value, present := fields["title_prefix"]; present && string(value) == "null" {
		return Config{}, fmt.Errorf("invalid Notion config JSON")
	}
	var c Config
	if err := json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, fmt.Errorf("invalid Notion config JSON")
	}
	return normalizeConfig(c)
}

// ResolveConfig discovers unique typed properties or resolves explicit selectors.
func ResolveConfig(ds DataSource, selectors Selectors, since string) (Config, error) {
	title, err := selectProperty(ds.Properties, "title", "")
	if err != nil {
		return Config{}, err
	}
	status, err := selectProperty(ds.Properties, "status", selectors.StatusProperty)
	if err != nil {
		return Config{}, err
	}
	assignee, err := selectProperty(ds.Properties, "people", selectors.AssigneeProperty)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		DataSourceID:       ds.ID,
		DatabaseID:         ds.DatabaseID,
		TitlePropertyID:    title.ID,
		StatusPropertyID:   status.ID,
		AssigneePropertyID: assignee.ID,
		Since:              since,
	}
	for _, selector := range selectors.DoneStatuses {
		option, err := selectOption(status.Options, selector)
		if err != nil {
			return Config{}, err
		}
		c.DoneStatusIDs = append(c.DoneStatusIDs, option.ID)
	}
	c, err = normalizeConfig(c)
	if err != nil {
		return Config{}, err
	}
	if err := ValidateSchema(c, ds); err != nil {
		return Config{}, err
	}
	return c, nil
}

func selectProperty(properties []Property, kind, selector string) (Property, error) {
	if selector == "" {
		var matches []Property
		for _, property := range properties {
			if property.Type == kind {
				matches = append(matches, property)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
	} else {
		choices := make([]Option, 0, len(properties))
		for _, property := range properties {
			choices = append(choices, Option{ID: property.ID, Name: property.Name})
		}
		selected, err := selectOption(choices, selector)
		if err != nil {
			return Property{}, err
		}
		for _, property := range properties {
			if property.ID == selected.ID {
				if property.Type != kind {
					return Property{}, fmt.Errorf("selected Notion property must have type %s", kind)
				}
				return property, nil
			}
		}
	}
	var choices []string
	for _, property := range properties {
		if property.Type == kind {
			choices = append(choices, fmt.Sprintf("%s (%s)", property.Name, property.ID))
		}
	}
	return Property{}, fmt.Errorf("select exactly one Notion %s property; choices: %s", kind, strings.Join(choices, ", "))
}

func selectOption(options []Option, selector string) (Option, error) {
	var ids, names []Option
	for _, option := range options {
		if option.ID == selector {
			ids = append(ids, option)
		}
		if option.Name == selector {
			names = append(names, option)
		}
	}
	if len(ids) == 1 {
		for _, named := range names {
			if named.ID != ids[0].ID {
				return Option{}, fmt.Errorf("ambiguous Notion ID/name selection")
			}
		}
		return ids[0], nil
	}
	if len(ids) == 0 && len(names) == 1 {
		return names[0], nil
	}
	return Option{}, fmt.Errorf("notion selection must match one existing ID or exact name")
}

// ValidateSchema verifies selected identities and types, allowing parent movement
// and display renames while rejecting removed completion options.
func ValidateSchema(c Config, ds DataSource) error {
	c, err := normalizeConfig(c)
	if err != nil {
		return err
	}
	id, err := canonicalID(ds.ID)
	if err != nil || id != c.DataSourceID {
		return fmt.Errorf("notion schema belongs to a different data source")
	}
	if _, err := canonicalID(ds.DatabaseID); err != nil {
		return err
	}
	titleCount := 0
	for _, property := range ds.Properties {
		if property.Type == "title" {
			titleCount++
		}
	}
	if titleCount != 1 {
		return fmt.Errorf("notion schema requires exactly one title property")
	}
	for _, selection := range []struct{ id, kind string }{{c.TitlePropertyID, "title"}, {c.StatusPropertyID, "status"}, {c.AssigneePropertyID, "people"}} {
		var matches []Property
		for _, property := range ds.Properties {
			if property.ID == selection.id {
				matches = append(matches, property)
			}
		}
		if len(matches) != 1 || matches[0].Type != selection.kind {
			return fmt.Errorf("selected Notion %s property is missing or has changed type", selection.kind)
		}
		if selection.kind == "status" {
			for _, completed := range c.DoneStatusIDs {
				count := 0
				for _, option := range matches[0].Options {
					if option.ID == completed {
						count++
					}
				}
				if count != 1 {
					return fmt.Errorf("selected Notion completion option is missing or ambiguous")
				}
			}
		}
	}
	return nil
}

// ValidateReenable permits cutoff, container, and presentation changes while keeping mapping fixed.
func ValidateReenable(previous, next Config) error {
	previous, err := normalizeConfig(previous)
	if err != nil {
		return err
	}
	next, err = normalizeConfig(next)
	if err != nil {
		return err
	}
	if previous.DataSourceID != next.DataSourceID || previous.TitlePropertyID != next.TitlePropertyID || previous.StatusPropertyID != next.StatusPropertyID || previous.AssigneePropertyID != next.AssigneePropertyID || !slices.Equal(previous.DoneStatusIDs, next.DoneStatusIDs) {
		return fmt.Errorf("notion source and selected mapping IDs cannot change on re-enable")
	}
	return nil
}
