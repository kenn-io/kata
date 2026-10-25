package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Private evidence decoding establishes no native authority. Newborn capture
// and descriptor correspondence belong to the actual admitted native retain Tx.
func TestScopedSourceCustodyEvidenceCodec(t *testing.T) {
	view, _, _ := verificationView(t)
	p := CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: "member", RouteDigest: view.RouteDigest}
	plain, err := json.Marshal(p)
	require.NoError(t, err)
	plain = append([]byte(" \n"), append(plain, []byte("\n ")...)...)
	t.Run("strict-legacy-no-upgrade", func(t *testing.T) {
		before := bytes.Clone(plain)
		actual, evidence, err := DecodeScopedSourceCustodyPrincipal(plain)
		require.NoError(t, err)
		require.Equal(t, p, actual)
		require.Nil(t, evidence, "a legacy tuple does not prove any admitted view")
		require.Equal(t, before, plain, "decoding cannot rewrite retained bytes")
	})
	frame := map[string]any{"version": 1, "principal": p, "view": view.Key, "descriptor_digest": strings.Repeat("a", 64)}
	t.Run("exact-view-and-retained-descriptor", func(t *testing.T) {
		raw, err := json.Marshal(frame)
		require.NoError(t, err)
		actual, evidence, err := DecodeScopedSourceCustodyPrincipal(raw)
		require.NoError(t, err)
		require.Equal(t, p, actual)
		require.NotNil(t, evidence)
		require.Equal(t, 1, evidence.Version)
		require.Equal(t, p, evidence.Principal)
		require.Equal(t, view.Key, evidence.View)
		require.Equal(t, strings.Repeat("a", 64), evidence.DescriptorDigest)
	})
	t.Run("closed-required-frame", func(t *testing.T) {
		for _, mutate := range []func(map[string]any){
			func(f map[string]any) { delete(f, "version") },
			func(f map[string]any) { f["version"] = nil },
			func(f map[string]any) { f["version"] = 2 },
			func(f map[string]any) { delete(f, "principal") },
			func(f map[string]any) { f["principal"] = nil },
			func(f map[string]any) { delete(f, "view") },
			func(f map[string]any) { f["view"] = nil },
			func(f map[string]any) { f["view"] = ScopedViewKey{ViewUID: view.Key.ViewUID} },
			func(f map[string]any) { delete(f, "descriptor_digest") },
			func(f map[string]any) { f["descriptor_digest"] = nil },
			func(f map[string]any) { f["descriptor_digest"] = "short" },
			func(f map[string]any) { f["holder"] = nil },
			func(f map[string]any) { f["local_caller"] = nil },
			func(f map[string]any) { f["owner"] = true },
		} {
			changed := make(map[string]any, len(frame))
			for name, value := range frame {
				changed[name] = value
			}
			mutate(changed)
			raw, err := json.Marshal(changed)
			require.NoError(t, err)
			_, _, err = DecodeScopedSourceCustodyPrincipal(raw)
			require.Error(t, err)
		}
	})
	t.Run("legacy-rejects-new-presence-even-null", func(t *testing.T) {
		for _, name := range []string{"view", "descriptor_digest", "version", "holder"} {
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(plain, &fields))
			fields[name] = jsontext.Value(`null`)
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			_, _, err = DecodeScopedSourceCustodyPrincipal(raw)
			require.Error(t, err)
		}
	})
}
