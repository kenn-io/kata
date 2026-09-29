package notionsync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const sourceID = "11111111-1111-4111-8111-111111111111"
const databaseID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func schema() DataSource {
	return DataSource{ID: sourceID, DatabaseID: databaseID, Name: "Example tasks", Properties: []Property{
		{ID: "title", Name: "Task description", Type: "title"},
		{ID: "s%3A1", Name: "Workflow", Type: "status", Options: []Option{{ID: "complete-b", Name: "Delivered"}, {ID: "complete-a", Name: "Accepted"}, {ID: "active", Name: "Doing"}}},
		{ID: "p%2F1", Name: "Responsible", Type: "people"},
	}}
}
func configFixture(t *testing.T) Config {
	t.Helper()
	c, err := ResolveConfig(schema(), Selectors{DoneStatuses: []string{"Delivered"}}, "")
	require.NoError(t, err)
	return c
}

func TestResolveConfigStableIDs(t *testing.T) {
	ds := schema()
	ds.ID = strings.ReplaceAll(strings.ToUpper(sourceID), "-", "")
	ds.DatabaseID = strings.ReplaceAll(strings.ToUpper(databaseID), "-", "")
	ds.Properties = append(ds.Properties, Property{ID: "other-status", Name: "Review", Type: "status"}, Property{ID: "other-people", Name: "Reviewers", Type: "people"})
	_, err := ResolveConfig(ds, Selectors{DoneStatuses: []string{"Delivered"}}, "")
	require.Error(t, err)
	c, err := ResolveConfig(ds, Selectors{StatusProperty: "Workflow", AssigneeProperty: "Responsible", DoneStatuses: []string{"Delivered", "complete-a", "Delivered"}}, "2026-09-28")
	require.NoError(t, err)
	require.Equal(t, Config{DataSourceID: sourceID, DatabaseID: databaseID, TitlePropertyID: "title", StatusPropertyID: "s%3A1", AssigneePropertyID: "p%2F1", DoneStatusIDs: []string{"complete-a", "complete-b"}, Since: "2026-09-28T00:00:00Z", TitlePrefix: new(true)}, c)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "Workflow")
	require.NotContains(t, string(raw), "Delivered")
	ds.Properties[0].Name = "Renamed title"
	ds.Properties[1].Name = "Renamed status"
	ds.Properties[1].Options[0].Name = "Renamed completion"
	ds.Properties[2].Name = "Renamed assignee"
	require.NoError(t, ValidateSchema(c, ds))
	renamed, err := ResolveConfig(ds, Selectors{StatusProperty: "s%3A1", AssigneeProperty: "p%2F1", DoneStatuses: []string{"complete-b", "complete-a"}}, c.Since)
	require.NoError(t, err)
	require.Equal(t, c, renamed)
}

func TestResolveConfigRejectsAmbiguousSelectors(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*DataSource)
		selectors Selectors
	}{
		{"property id/name collision", func(ds *DataSource) {
			ds.Properties = append(ds.Properties, Property{ID: "other", Name: "s%3A1", Type: "status"})
		}, Selectors{StatusProperty: "s%3A1", DoneStatuses: []string{"Delivered"}}},
		{"option id/name collision", func(ds *DataSource) {
			ds.Properties[1].Options = append(ds.Properties[1].Options, Option{ID: "other", Name: "complete-b"})
		}, Selectors{DoneStatuses: []string{"complete-b"}}},
		{"duplicate names", func(ds *DataSource) {
			ds.Properties = append(ds.Properties, Property{ID: "other", Name: "Workflow", Type: "status"})
		}, Selectors{StatusProperty: "Workflow", DoneStatuses: []string{"Delivered"}}},
		{"wrong selected type", func(_ *DataSource) {}, Selectors{StatusProperty: "title", DoneStatuses: []string{"Delivered"}}},
		{"multiple titles", func(ds *DataSource) {
			ds.Properties = append(ds.Properties, Property{ID: "second-title", Name: "Other", Type: "title"})
		}, Selectors{DoneStatuses: []string{"Delivered"}}},
		{"missing people", func(ds *DataSource) { ds.Properties = ds.Properties[:2] }, Selectors{DoneStatuses: []string{"Delivered"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds := schema()
			tt.mutate(&ds)
			_, err := ResolveConfig(ds, tt.selectors, "")
			require.Error(t, err)
		})
	}
}

func TestResolveConfigRequiresDoneStatuses(t *testing.T) {
	for _, values := range [][]string{nil, {}, {"unknown"}, {"delivered"}, {""}} {
		_, err := ResolveConfig(schema(), Selectors{DoneStatuses: values}, "")
		require.Error(t, err)
	}
}

func TestValidateSchemaMapping(t *testing.T) {
	c := configFixture(t)
	for _, change := range []func(*DataSource){
		func(ds *DataSource) { ds.ID = "22222222-2222-4222-8222-222222222222" },
		func(ds *DataSource) { ds.Properties[0].Type = "rich_text" },
		func(ds *DataSource) { ds.Properties[1].Type = "select" },
		func(ds *DataSource) { ds.Properties[2].ID = "replacement" },
		func(ds *DataSource) { ds.Properties[1].Options = ds.Properties[1].Options[1:] },
	} {
		ds := schema()
		change(&ds)
		require.Error(t, ValidateSchema(c, ds))
	}
	ds := schema()
	ds.DatabaseID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	ds.Properties[1].Options = append(ds.Properties[1].Options, Option{ID: "new", Name: "Done"})
	require.NoError(t, ValidateSchema(c, ds))
}

func TestValidateReenableImmutableMapping(t *testing.T) {
	c := configFixture(t)
	c.DoneStatusIDs = []string{"complete-a", "complete-b"}
	for _, change := range []func(*Config){
		func(c *Config) { c.DataSourceID = "22222222-2222-4222-8222-222222222222" },
		func(c *Config) { c.TitlePropertyID = "replacement" },
		func(c *Config) { c.StatusPropertyID = "replacement" },
		func(c *Config) { c.AssigneePropertyID = "replacement" },
		func(c *Config) { c.DoneStatusIDs = []string{"active"} },
	} {
		next := c
		change(&next)
		require.Error(t, ValidateReenable(c, next))
	}
	next := c
	next.Since = "2026-09-28"
	next.DatabaseID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	next.DoneStatusIDs = []string{"complete-b", "complete-a", "complete-b"}
	require.NoError(t, ValidateReenable(c, next))
}

func TestConfigCanonicalStrictJSON(t *testing.T) {
	c := configFixture(t)
	c.DoneStatusIDs = []string{"complete-b", "complete-a", "complete-b"}
	c.Since = "2026-09-28T03:00:00+03:00"
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	require.Equal(t, `{"data_source_id":"11111111-1111-4111-8111-111111111111","database_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","title_property_id":"title","status_property_id":"s%3A1","assignee_property_id":"p%2F1","done_status_ids":["complete-a","complete-b"],"since":"2026-09-28T00:00:00Z","title_prefix":true}`, string(raw))
	decoded, err := DecodeConfig(raw)
	require.NoError(t, err)
	again, err := EncodeConfig(decoded)
	require.NoError(t, err)
	require.Equal(t, raw, again)
	for _, key := range []string{"token", "token_env", "api_url", "status_property", "unknown"} {
		bad := jsontext.Value(strings.TrimSuffix(string(raw), "}") + `,"` + key + `":"secret-marker"}`)
		_, err := DecodeConfig(bad)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret-marker")
	}
	for _, value := range []string{`null`, `{}`, `[]`, `{"data_source_id":"bad"}`, strings.Replace(string(raw), `"done_status_ids":["complete-a","complete-b"]`, `"done_status_ids":null`, 1), strings.Replace(string(raw), `"title_property_id":"title"`, `"title_property_id":""`, 1)} {
		_, err := DecodeConfig(jsontext.Value(value))
		require.Error(t, err)
	}
}

func TestParseSince(t *testing.T) {
	for _, tt := range []struct{ input, want string }{{"", ""}, {"  ", ""}, {"0001-01-01", "0001-01-01T00:00:00Z"}, {"2026-09-28", "2026-09-28T00:00:00Z"}, {"2026-09-28T01:30:00+01:30", "2026-09-28T00:00:00Z"}} {
		got, err := ParseSince(tt.input)
		require.NoError(t, err)
		if tt.want == "" {
			require.Nil(t, got)
		} else {
			require.Equal(t, tt.want, got.Format(time.RFC3339))
		}
	}
	for _, input := range []string{"bad", "2026-02-30", "2026-09-28T00:00:00.1Z", "2026-09-28T00:00:00,1Z", "0000-01-01", "0000-01-01T00:00:00Z", "0001-01-01T00:00:00+01:00", "0000-01-01T00:00:00+01:00", "9999-12-31T23:59:59-01:00"} {
		_, err := ParseSince(input)
		require.Error(t, err, input)
	}
}

func TestParseDatabaseLocator(t *testing.T) {
	for _, input := range []string{databaseID, strings.ReplaceAll(strings.ToUpper(databaseID), "-", ""), "https://notion.so/" + databaseID, "https://www.notion.so/p/" + databaseID + "?v=bbbbbbbbbbbb4bbb8bbbbbbbbbbbbbbb#view", "https://notion.com/Example-" + strings.ReplaceAll(databaseID, "-", ""), "https://www.notion.com/" + databaseID, "https://app.notion.com:443/" + databaseID, "https://WWW.NOTION.SO/Tasks-" + databaseID, "https://www.notion.so/Tasks-" + databaseID + "/?v=ignored"} {
		got, err := ParseDatabaseLocator(input)
		require.NoError(t, err, input)
		require.Equal(t, databaseID, got)
	}
	for _, input := range []string{"bad", "TASK-42", "http://notion.so/" + databaseID, "https://custom.example/" + databaseID, "https://notion.so.evil.example/" + databaseID, "https://user@notion.so/" + databaseID, "https://notion.so:444/" + databaseID, "https://notion.so/TASK-42", "https://notion.so/" + databaseID + "/view", "https://notion.so/" + databaseID + "/" + sourceID, "https://notion.so/p/" + databaseID + "//", "https://notion.so/p/%61" + databaseID[1:]} {
		_, err := ParseDatabaseLocator(input)
		require.Error(t, err, input)
	}
}

func TestConfigTitlePrefix(t *testing.T) {
	raw, err := EncodeConfig(configFixture(t))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, choice := range []bool{false, true} {
		fields["title_prefix"] = choice
		wire, err := json.Marshal(fields)
		require.NoError(t, err)
		decoded, err := DecodeConfig(wire)
		require.NoError(t, err)
		encoded, err := EncodeConfig(decoded)
		require.NoError(t, err)
		var roundtrip map[string]any
		require.NoError(t, json.Unmarshal(encoded, &roundtrip))
		require.Equal(t, choice, roundtrip["title_prefix"])
	}
	delete(fields, "title_prefix")
	legacy, err := json.Marshal(fields)
	require.NoError(t, err)
	decoded, err := DecodeConfig(legacy)
	require.NoError(t, err)
	encoded, err := EncodeConfig(decoded)
	require.NoError(t, err)
	var defaults map[string]any
	require.NoError(t, json.Unmarshal(encoded, &defaults))
	require.Equal(t, true, defaults["title_prefix"])
	for _, value := range []any{nil, "false", 0, []any{}, map[string]any{}} {
		fields["title_prefix"] = value
		bad, err := json.Marshal(fields)
		require.NoError(t, err)
		_, err = DecodeConfig(bad)
		require.Error(t, err)
	}
}
