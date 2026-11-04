package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Preferences identify configuration, never a responsible actor or installation.
func TestDefinitionResponsibilityPreferences(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		adapter := hegel.Draw(ht, hegel.Text())
		executor := hegel.Draw(ht, hegel.Text())
		raw, err := json.Marshal(map[string]string{"adapter": adapter, "executor": executor})
		require.NoError(ht, err)
		got := DefinitionResponsibility(raw, false)
		require.Nil(ht, got.Actor)
		require.Nil(ht, got.Runtime)
		require.Equal(ht, "unknown", got.RuntimeStatus)
		for _, pair := range []struct {
			input  string
			output *string
		}{{adapter, got.Adapter}, {executor, got.Executor}} {
			if strings.TrimSpace(pair.input) == "" {
				require.Nil(ht, pair.output)
			} else {
				require.Equal(ht, pair.input, *pair.output)
			}
		}
	}, hegel.WithTestCases(1000))
}

func TestDefinitionResponsibilityUnusablePreferences(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `{`, `{"adapter":42,"executor":true}`, `{"adapter":{},"executor":[]}`, `{"adapter":" \t","executor":null}`} {
		got := DefinitionResponsibility(jsontext.Value(raw), false)
		require.Nil(t, got.Adapter, raw)
		require.Nil(t, got.Executor, raw)
		require.Nil(t, got.Runtime, raw)
		require.Equal(t, "unknown", got.RuntimeStatus, raw)
	}
}

func TestNativeResponsibilityDoesNotInventIdentity(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw, err := json.Marshal(map[string]string{"adapter": hegel.Draw(ht, hegel.Text())})
		require.NoError(ht, err)
		got := DefinitionResponsibility(raw, true)
		require.Nil(ht, got.Actor)
		require.Equal(ht, "owning daemon", *got.Runtime)
		require.Equal(ht, "daemon", got.RuntimeStatus)
	}, hegel.WithTestCases(1000))
}

func TestRunResponsibilityPreservesReportedEvidence(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.Text())
		label := hegel.Draw(ht, hegel.Text())
		got := RunResponsibility(actor, &label)
		require.Equal(ht, actor, *got.Actor)
		if strings.TrimSpace(label) == "" {
			require.Nil(ht, got.Runtime)
			require.Equal(ht, "unreported", got.RuntimeStatus)
		} else {
			require.Equal(ht, label, *got.Runtime)
			require.Equal(ht, "reported", got.RuntimeStatus)
		}
	}, hegel.WithTestCases(1000))
	got := RunResponsibility("worker", nil)
	require.Nil(t, got.Runtime)
	require.Equal(t, "unreported", got.RuntimeStatus)
}

func TestResponsibilityPreferencesAreIndependent(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := "runtime-" + hegel.Draw(ht, hegel.Text())
		validKey := hegel.Draw(ht, hegel.SampledFrom([]string{"adapter", "executor"}))
		invalidKey := "executor"
		if validKey == invalidKey {
			invalidKey = "adapter"
		}
		invalid := hegel.Draw(ht, hegel.SampledFrom([]jsontext.Value{[]byte(`null`), []byte(`true`), []byte(`42`), []byte(`[]`), []byte(`{"adapter":"nested-runtime"}`)}))
		encoded, err := json.Marshal(text)
		require.NoError(ht, err)
		raw, err := json.Marshal(map[string]jsontext.Value{validKey: encoded, invalidKey: invalid})
		require.NoError(ht, err)
		got := DefinitionResponsibility(raw, false)
		valid, rejected := got.Adapter, got.Executor
		if validKey == "executor" {
			valid, rejected = got.Executor, got.Adapter
		}
		require.Equal(ht, text, *valid)
		require.Nil(ht, rejected)
	}, hegel.WithTestCases(1000))
}
