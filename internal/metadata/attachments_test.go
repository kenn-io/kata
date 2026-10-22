package metadata

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachmentValid(t *testing.T) {
	const good = `{"name":"shot.png","media_type":"image/png","size":12,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"}`
	entry, ok := Valid(jsontext.Value(good))
	require.True(t, ok)
	require.Equal(t, "shot.png", entry.Name)
	require.EqualValues(t, 12, entry.Size)
	for _, raw := range []string{
		`null`, `[]`, `"junk"`, `{}`, strings.Replace(good, `"shot.png"`, `"../shot.png"`, 1),
		strings.Replace(good, `"shot.png"`, `"a\\b"`, 1), strings.Replace(good, `"shot.png"`, `"a\n"`, 1),
		strings.Replace(good, `"shot.png"`, `"."`, 1), strings.Replace(good, `"shot.png"`, `".."`, 1),
		strings.Replace(good, `"shot.png"`, `"`+strings.Repeat("a", 256)+`"`, 1),
		strings.Replace(good, `"image/png"`, `""`, 1), strings.Replace(good, `:12`, `:-1`, 1),
		strings.Replace(good, `:12`, `:"12"`, 1), strings.Replace(good, `6f1c2e8a-2222-4222-8222-222222222222`, `invalid`, 1),
	} {
		_, valid := Valid(jsontext.Value(raw))
		require.False(t, valid, raw)
	}
	for _, tc := range []struct{ name, raw string }{
		{"missing size", strings.Replace(good, `"size":12,`, "", 1)},
		{"null size", strings.Replace(good, `"size":12`, `"size":null`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, valid := Valid(jsontext.Value(tc.raw))
			require.False(t, valid, tc.raw)
		})
	}
	_, ok = Valid(jsontext.Value(strings.TrimSuffix(good, "}") + `,"schema_version":2}`))
	require.False(t, ok, "unknown reference shapes are unreadable")
}
