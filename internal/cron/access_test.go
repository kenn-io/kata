package cron

import (
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// The public contract exempts only an exact enabled-to-disabled replacement.
func TestRequiresDefineGrant(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prompt := hegel.Draw(ht, hegel.Text())
		old := JobDefinition{Version: 1, Kind: "job", Enabled: hegel.Draw(ht, hegel.Booleans()), Action: Action{Kind: "execute", Prompt: prompt}}
		old.SecretRefs = map[string]string{"FIRST": prompt, "SECOND": hegel.Draw(ht, hegel.Text()), "THIRD": hegel.Draw(ht, hegel.Text())}
		next := old
		require.True(ht, RequiresDefineGrant(nil, &next))
		require.True(ht, RequiresDefineGrant(&old, nil))
		require.True(ht, RequiresDefineGrant(&old, &next))
		next.Enabled = false
		require.Equal(ht, !old.Enabled, RequiresDefineGrant(&old, &next))
		next.Action.Prompt += "changed"
		require.True(ht, RequiresDefineGrant(&old, &next))
		next.Action = Action{Kind: "notify"}
		require.True(ht, RequiresDefineGrant(&old, &next))
		old.Action = next.Action
		require.False(ht, RequiresDefineGrant(&old, &next))
	}, hegel.WithDatabase(t.TempDir()))
}

// A pause changes only enabled even if a client reformats opaque JSON options.
func TestRequiresDefineGrantOptionsFormatting(t *testing.T) {
	old := JobDefinition{Version: 1, Kind: "job", Enabled: true, Action: Action{Kind: "execute", Prompt: "Inspect"}, Options: []byte(`{"adapter":{"count":9007199254740993,"label":"example"}}`)}
	next := old
	next.Enabled = false
	next.Options = []byte(`{ "adapter": { "label": "example", "count": 9007199254740993 } }`)
	require.False(t, RequiresDefineGrant(&old, &next))
	next.Options = []byte(`{"adapter":{"count":9007199254740992,"label":"example"}}`)
	require.True(t, RequiresDefineGrant(&old, &next), "large integers must not lose precision")
}

// Map iteration order must not turn an unchanged secret reference into an edit.
func TestRequiresDefineGrantSecretReferences(t *testing.T) {
	old := JobDefinition{Version: 1, Kind: "job", Enabled: true, Action: Action{Kind: "execute", Prompt: "Inspect"}, SecretRefs: map[string]string{"FIRST": "secretref://one", "SECOND": "secretref://two", "THIRD": "secretref://three", "FOURTH": "secretref://four"}}
	next := old
	next.Enabled = false
	for range 64 {
		require.False(t, RequiresDefineGrant(&old, &next))
	}
	next.SecretRefs = map[string]string{"FIRST": "secretref://changed", "SECOND": "secretref://two", "THIRD": "secretref://three", "FOURTH": "secretref://four"}
	require.True(t, RequiresDefineGrant(&old, &next))
}
