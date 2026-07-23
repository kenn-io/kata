package config

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalHTTPOrigin(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"HTTPS://Daemon.Example:443/path?q=1", "https://daemon.example"},
		{"http://127.0.0.1:80", "http://127.0.0.1"},
		{"https://Daemon.Example:8443", "https://daemon.example:8443"},
		{"https://[2001:db8::1]:443", "https://[2001:db8::1]"},
		{"http://daemon.example:00080/path", "http://daemon.example"},
		{"https://daemon.example:00443/path", "https://daemon.example"},
		{"https://daemon.example:08443/path", "https://daemon.example:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := CanonicalHTTPOrigin(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	invalid := []string{
		"https:///missing-host",
		"https://user:password@daemon.example",
		"ssh://daemon.example",
		"https://daemon.example:not-a-port",
		"https://daemon.example:0",
		"https://daemon.example:65536",
	}
	for _, input := range invalid {
		t.Run("reject "+input, func(t *testing.T) {
			_, err := CanonicalHTTPOrigin(input)
			require.Error(t, err)
		})
	}
}

func TestCanonicalHTTPOriginPreservesIPv6ZoneCaseAndEscaping(t *testing.T) {
	got, err := CanonicalHTTPOrigin(" HTTP://[FE80::ABCD%25EnABC]:00080/path ")
	require.NoError(t, err)
	assert.Equal(t, "http://[fe80::abcd%25EnABC]", got)

	reparsed, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "fe80::abcd%EnABC", reparsed.Hostname())
	assert.Empty(t, reparsed.Port())
}
