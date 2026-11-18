package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/pkg/connector"
)

func TestConnectorServeForgejoSingleRPC(t *testing.T) {
	resetFlags(t)
	t.Setenv("FORGEJO_TOKEN", "example-forgejo-token")
	t.Setenv("KATA_HOME", t.TempDir())
	var daemonCalls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		daemonCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer daemon.Close()
	t.Setenv("KATA_SERVER", daemon.URL)
	stubIsTTY(t, true)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/user", r.URL.Path)
		assert.Equal(t, "token example-forgejo-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":7,"login":"connector-bot","full_name":"Connector Bot"}`)
	}))
	defer provider.Close()
	input, err := json.Marshal(map[string]any{
		"protocol": connector.ProtocolVersion, "id": "request-example", "method": "describe",
		"instance": "forgejo-example", "settings": map[string]any{"origin": provider.URL}, "params": map[string]any{},
	})
	require.NoError(t, err)
	var out, diagnostics bytes.Buffer
	cmd := newRootCmd()
	cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: daemon.URL})
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	cmd.SetIn(bytes.NewReader(input))
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"connector", "serve", "forgejo"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	decoder := json.NewDecoder(&out)
	var response connector.Response
	require.NoError(t, decoder.Decode(&response))
	assert.Equal(t, "request-example", response.ID)
	assert.Equal(t, connector.ProtocolVersion, response.Protocol)
	assert.Nil(t, response.Error)
	var description connector.Description
	require.NoError(t, json.Unmarshal(response.Result, &description))
	assert.Equal(t, "forgejo", description.ConnectorID)
	assert.Equal(t, "7", description.SelfActorID)
	assert.NotContains(t, description.Capabilities, connector.CapabilityConditionalFields)
	assert.ErrorIs(t, decoder.Decode(new(any)), io.EOF)
	assert.Empty(t, diagnostics.String())
	assert.Zero(t, daemonCalls.Load())
}

func TestConnectorServeForgejoMissingCredentialsAndMalformedFraming(t *testing.T) {
	for _, test := range []struct {
		name, input, errorContains string
		invalid                    bool
	}{
		{name: "missing credentials", input: `{"protocol":"kata.connector.v1","id":"request-example","method":"describe","instance":"forgejo-example","settings":{"origin":"https://forge.example"},"params":{}}`},
		{name: "malformed", input: `not-json`, errorContains: "decode connector request:", invalid: true},
		{name: "trailing", input: `{"protocol":"kata.connector.v1","id":"request-example","method":"describe","instance":"forgejo-example","settings":{"origin":"https://forge.example"},"params":{}} {}`, errorContains: "trailing JSON", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetFlags(t)
			t.Setenv("FORGEJO_TOKEN", "")
			t.Setenv("KATA_HOME", t.TempDir())
			var out bytes.Buffer
			cmd := newRootCmd()
			cmd.SetIn(strings.NewReader(test.input))
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"connector", "serve", "forgejo"})
			err := cmd.ExecuteContext(t.Context())
			if test.invalid {
				require.Error(t, err)
				require.ErrorContains(t, err, test.errorContains)
				assert.Empty(t, out.String())
				return
			}
			require.NoError(t, err)
			var response connector.Response
			require.NoError(t, json.Unmarshal(out.Bytes(), &response))
			require.NotNil(t, response.Error)
			assert.Empty(t, response.Result)
		})
	}
}
