package testenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSkipAutomaticPostgresContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goos        string
		explicitDSN string
		autostart   string
		want        bool
	}{
		{name: "windows without service", goos: "windows", want: true},
		{name: "windows explicit service", goos: "windows", explicitDSN: "postgres://service/kata", want: false},
		{name: "linux without service", goos: "linux", want: false},
		{name: "linux autostart disabled", goos: "linux", autostart: "0", want: true},
		{name: "explicit service overrides disabled autostart", goos: "linux", explicitDSN: "postgres://service/kata", autostart: "0", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, skipAutomaticPostgresContainer(tc.goos, tc.explicitDSN, tc.autostart))
		})
	}
}
