package twentysync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const workspaceID = "11111111-1111-4111-8111-111111111111"
const taskID = "22222222-2222-4222-8222-222222222222"

func testConfig() Config {
	return Config{APIOrigin: "https://twenty.example", WebOrigin: "https://ui.example", WorkspaceID: workspaceID}
}

func TestBindingConfig(t *testing.T) {
	raw, err := EncodeConfig(testConfig())
	require.NoError(t, err)
	c, err := DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, "twenty:https://twenty.example/"+workspaceID, c.SourceKey())
	require.Equal(t, workspaceID, c.RemoteID())
	require.Equal(t, "one-way", c.StatusSync)
	require.Equal(t, "DONE", c.ClosedStatus)
	require.Equal(t, "TODO", c.OpenStatus)
	require.Equal(t, []string{"IN_PROGRESS", "TODO"}, c.OpenStatuses)
	require.True(t, c.UseTitlePrefix())
	for _, input := range []string{`null`, `[]`, `{}`, `{"api_origin":"secret"}`, `{"token":"secret"}`} {
		_, err := DecodeConfig(jsontext.Value(input))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.WorkspaceID = "../../secret" },
		func(c *Config) { c.StatusSync = "invalid" },
		func(c *Config) { c.OpenStatuses = []string{"DONE", "TODO"} },
		func(c *Config) { c.OpenStatus = "IN_PROGRESS"; c.OpenStatuses = []string{"TODO"} },
		func(c *Config) { c.OpenStatuses = []string{} },
	} {
		c := testConfig()
		edit(&c)
		_, err := EncodeConfig(c)
		require.Error(t, err)
	}
	c = testConfig()
	c.Since = "2026-10-04T01:00:00+01:00"
	raw, err = EncodeConfig(c)
	require.NoError(t, err)
	c, err = DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, "2026-10-04T00:00:00Z", c.Since)
	for _, value := range []string{"yesterday", "2026-10-04T00:00:00.1Z"} {
		_, err := ParseSince(value)
		require.Error(t, err)
	}
	at, err := ParseSince("2026-10-04")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), *at)
}

func TestStatusClassification(t *testing.T) {
	c := testConfig()
	schema := Schema{StatusOptions: []string{"TODO", "IN_PROGRESS", "DONE"}}
	require.NoError(t, ValidateSchema(c, schema))
	for _, value := range []*string{nil, new("TODO"), new("IN_PROGRESS"), new("DONE")} {
		status, reason, err := ClassifyStatus(c, value)
		require.NoError(t, err)
		if value != nil && *value == "DONE" {
			require.Equal(t, "closed", status)
			require.Equal(t, "done", reason)
		} else {
			require.Equal(t, "open", status)
			require.Empty(t, reason)
		}
	}
	require.Error(t, ValidateSchema(c, Schema{StatusOptions: []string{"TODO", "DONE", "CUSTOM"}}))
	c.ClosedStatus = "COMPLETE"
	c.OpenStatus = "READY"
	c.OpenStatuses = []string{"READY", "WORKING"}
	require.NoError(t, ValidateSchema(c, Schema{StatusOptions: []string{"READY", "WORKING", "COMPLETE"}}))
	require.NoError(t, ValidateSchema(c, Schema{StatusOptions: []string{"WORKING", "COMPLETE"}}))
	c.StatusSync = "two-way"
	require.Error(t, ValidateSchema(c, Schema{StatusOptions: []string{"WORKING", "COMPLETE"}}))
}

func TestBindingConfigRejectsSecretsAndAcceptsRunnerProgress(t *testing.T) {
	raw, err := EncodeConfig(testConfig())
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &fields))
	fields["token"] = jsontext.Value(`"secret"`)
	withSecret, err := json.Marshal(fields)
	require.NoError(t, err)
	_, err = DecodeConfig(withSecret)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
	delete(fields, "token")
	fields["_status_sync"] = jsontext.Value(`{"pending":{"after":1,"through":5},"sweep":{"after":0,"through":2}}`)
	withProgress, err := json.Marshal(fields)
	require.NoError(t, err)
	c, err := DecodeConfig(withProgress)
	require.NoError(t, err)
	require.Equal(t, workspaceID, c.WorkspaceID)
	for _, key := range []string{"title_prefix", "status_sync", "open_status", "closed_status", "open_statuses"} {
		t.Run(key, func(t *testing.T) {
			var invalid map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(raw, &invalid))
			invalid[key] = jsontext.Value(`null`)
			bs, err := json.Marshal(invalid)
			require.NoError(t, err)
			_, err = DecodeConfig(bs)
			require.Error(t, err)
		})
	}
}
