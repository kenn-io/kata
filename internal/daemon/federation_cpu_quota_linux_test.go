package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationPollCPUsFromCgroup(t *testing.T) {
	for _, tc := range []struct {
		name, group, mount, files string
		want                      int
	}{
		{"v2 quota", "0::/slice/scope", "cgroup2", "scope/cpu.max=100000 100000", 1},
		{"v2 parent quota", "0::/slice/scope", "cgroup2", "scope/cpu.max=max 100000,cpu.max=100000 100000", 1},
		{"v2 fractional quota", "0::/slice/scope", "cgroup2", "scope/cpu.max=150000 100000", 2},
		{"v2 unlimited", "0::/slice/scope", "cgroup2", "scope/cpu.max=max 100000", 8},
		{"v1 quota", "5:cpu,cpuacct:/slice/scope", "cgroup", "scope/cpu.cfs_quota_us=100000,scope/cpu.cfs_period_us=100000", 1},
		{"v1 parent quota", "5:cpu,cpuacct:/slice/scope", "cgroup", "scope/cpu.cfs_quota_us=-1,scope/cpu.cfs_period_us=100000,cpu.cfs_quota_us=100000,cpu.cfs_period_us=100000", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "cgroup mount")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "scope"), 0o700))
			for file := range strings.SplitSeq(tc.files, ",") {
				name, contents, _ := strings.Cut(file, "=")
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600))
			}
			mountpoint := strings.ReplaceAll(root, " ", `\040`)
			mount := "29 23 0:26 /slice " + mountpoint + " rw - " + tc.mount + " cgroup rw,cpu"
			assert.Equal(t, tc.want, federationPollCPUsFromCgroup(8, []byte(tc.group), []byte(mount)))
		})
	}
}
