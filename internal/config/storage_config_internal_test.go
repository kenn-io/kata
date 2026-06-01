package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractStorageSection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "only storage",
			in:   "[storage]\ndsn = \"x\"\n",
			want: "[storage]\ndsn = \"x\"\n",
		},
		{
			name: "storage then auth drops auth",
			in:   "[storage]\ndsn = \"x\"\n[auth]\ntoken = \"y\"\n",
			want: "[storage]\ndsn = \"x\"\n",
		},
		{
			name: "auth then storage drops auth",
			in:   "[auth]\ntoken = \"y\"\n[storage]\ndsn = \"x\"\n",
			want: "[storage]\ndsn = \"x\"\n",
		},
		{
			name: "storage subtable kept",
			in:   "[storage.sub]\nfoo = \"bar\"\n[auth]\ntoken = \"y\"\n",
			want: "[storage.sub]\nfoo = \"bar\"\n",
		},
		{
			name: "look-alike heading [storaged] is NOT storage",
			in:   "[storaged]\nfoo = \"bar\"\n[storage]\ndsn = \"x\"\n",
			want: "[storage]\ndsn = \"x\"\n",
		},
		{
			name: "leading whitespace preserved",
			in:   "  [storage]\n  dsn = \"x\"\n",
			want: "  [storage]\n  dsn = \"x\"\n",
		},
		{
			name: "no storage section",
			in:   "[auth]\ntoken = \"y\"\nlisten = \"x\"\n",
			want: "",
		},
		{
			name: "top-level keys before any section drop",
			in:   "listen = \"x\"\n[storage]\ndsn = \"y\"\n",
			want: "[storage]\ndsn = \"y\"\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(extractStorageSection([]byte(c.in)))
			assert.Equal(t, c.want, got)
		})
	}
}

func TestExtractStorageSection_MalformedSectionsOutsideStorageAreSkipped(t *testing.T) {
	// The point of the narrow reader: malformed bytes in unrelated sections
	// must not poison the extraction. extractStorageSection is byte-level
	// only, so this test mostly proves we don't choke on the input — the
	// real "would-fail" assertion lives in the readStorageConfig test.
	in := "[auth]\ntoken =\n[storage]\ndsn = \"postgres://h/db\"\n"
	got := string(extractStorageSection([]byte(in)))
	assert.True(t, strings.Contains(got, "postgres://h/db"))
	assert.False(t, strings.Contains(got, "token ="))
}
