package testenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSkipAutomaticPostgresContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		goos      string
		autostart string
		want      bool
	}{
		{name: "windows without service", goos: "windows", want: true},
		{name: "linux without service", goos: "linux", want: false},
		{name: "linux autostart disabled", goos: "linux", autostart: "0", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, skipAutomaticPostgresContainer(tc.goos, tc.autostart))
		})
	}
}
