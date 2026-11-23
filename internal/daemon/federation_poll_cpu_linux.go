package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// federationPollCPUs includes cgroup limits, which GOMAXPROCS rounds up to
// at least two even when a deployment has only one CPU of quota.
func federationPollCPUs() int {
	cpus := runtime.GOMAXPROCS(0)
	groups, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return cpus
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return cpus
	}
	return federationPollCPUsFromCgroup(cpus, groups, mounts)
}

func federationPollCPUsFromCgroup(cpus int, groups, mounts []byte) int {
	for group := range strings.SplitSeq(string(groups), "\n") {
		parts := strings.SplitN(group, ":", 3)
		if len(parts) != 3 {
			continue
		}
		version := "cgroup2"
		if parts[1] != "" {
			version = "cgroup"
			if !strings.Contains(","+parts[1]+",", ",cpu,") {
				continue
			}
		}
		for mount := range strings.SplitSeq(string(mounts), "\n") {
			before, after, ok := strings.Cut(mount, " - ")
			if !ok {
				continue
			}
			left, right := strings.Fields(before), strings.Fields(after)
			if len(left) < 5 || len(right) < 3 || right[0] != version ||
				(version == "cgroup" && !strings.Contains(","+right[2]+",", ",cpu,")) {
				continue
			}
			unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
			root, point := unescape.Replace(left[3]), filepath.Clean(unescape.Replace(left[4]))
			rel, err := filepath.Rel(root, parts[2])
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				continue
			}
			for path := filepath.Join(point, rel); ; path = filepath.Dir(path) {
				var quota, period int64
				if version == "cgroup2" {
					data, _ := os.ReadFile(filepath.Join(path, "cpu.max")) //nolint:gosec // The kernel supplies the cgroup path.
					fields := strings.Fields(string(data))
					if len(fields) == 2 {
						quota, _ = strconv.ParseInt(fields[0], 10, 64)
						period, _ = strconv.ParseInt(fields[1], 10, 64)
					}
				} else {
					q, _ := os.ReadFile(filepath.Join(path, "cpu.cfs_quota_us"))  //nolint:gosec // The kernel supplies the cgroup path.
					p, _ := os.ReadFile(filepath.Join(path, "cpu.cfs_period_us")) //nolint:gosec // The kernel supplies the cgroup path.
					quota, _ = strconv.ParseInt(strings.TrimSpace(string(q)), 10, 64)
					period, _ = strconv.ParseInt(strings.TrimSpace(string(p)), 10, 64)
				}
				if quota > 0 && period > 0 {
					if limit := (quota-1)/period + 1; limit < int64(cpus) {
						cpus = int(limit)
					}
				}
				if path == point {
					break
				}
			}
		}
	}
	return cpus
}
