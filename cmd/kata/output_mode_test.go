package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveOutputMode(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		json    bool
		agent   bool
		want    outputMode
		wantErr string
	}{
		{name: "default human", want: outputHuman},
		{name: "format json", format: "json", want: outputJSON},
		{name: "json alias", json: true, want: outputJSON},
		{name: "agent alias", agent: true, want: outputAgent},
		{name: "matching agent flags", format: "agent", agent: true, want: outputAgent},
		{name: "conflicting modes", format: "human", json: true, wantErr: "conflicting output modes"},
		{name: "bad format", format: "xml", wantErr: "unsupported output format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOutputModeValues(tc.format, tc.json, tc.agent)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveOutputModeArgs_BadFormatOutsideImport(t *testing.T) {
	_, err := resolveOutputModeArgs([]string{"--format", "xml"}, "", false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported output format")
}

func TestAgentValue_Quoting(t *testing.T) {
	assert.Equal(t, "abc4", agentValue("abc4"))
	assert.Equal(t, strconv.Quote("Fix login race"), agentValue("Fix login race"))
	assert.Equal(t, strconv.Quote(`quoted "title"`), agentValue(`quoted "title"`))
	assert.Equal(t, strconv.Quote("bad\nline"), agentValue("bad\nline"))
}

func TestAgentFencedText_ExtendsFenceForBackticks(t *testing.T) {
	got := agentFencedText("``` inside")
	assert.Contains(t, got, "````text\n")
	assert.True(t, strings.HasSuffix(got, "\n````\n"))
}

func TestAgentFencedText_ChoosesFenceAfterSanitizing(t *testing.T) {
	got := agentFencedText("``\x00` inside")
	assert.Contains(t, got, "````text\n")
	assert.Contains(t, got, "``` inside")
	assert.True(t, strings.HasSuffix(got, "\n````\n"))
}
