package harness

import (
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

type processAncestry struct {
	startPID int32
	lookup   func(pid int32) (name string, args []string, ppid int32, created int64, err error)
}

// ProcessAncestry probes at most 16 ancestors, starting with os.Getppid().
// Matching processes have generation "<pid>:<create-time-ms>".
func ProcessAncestry() Ancestry {
	return processAncestry{
		startPID: int32(os.Getppid()), //nolint:gosec // G115: gopsutil uses int32 PIDs; nonpositive ancestors are rejected before lookup.
		lookup:   lookupProcess,
	}
}

func (a processAncestry) Nearest(candidates []string) (string, string, bool) {
	if len(candidates) == 0 {
		return "", "", false
	}
	pid := a.startPID
	for range 16 {
		if pid <= 1 {
			return "", "", false
		}
		name, args, parent, created, err := a.lookup(pid)
		if err != nil {
			return "", "", false
		}
		name = executableName(name)
		for _, candidate := range candidates {
			m := namedMarker(candidate)
			if m != nil && m.executableMatches(name, args) {
				if created <= 0 {
					return "", "", false
				}
				generation := strconv.FormatInt(int64(pid), 10) + ":" + strconv.FormatInt(created, 10)
				return m.name, generation, true
			}
		}
		if !ancestryWrapper(name, args) {
			return "", "", false
		}
		pid = parent
	}
	return "", "", false
}

func (m marker) executableMatches(name string, args []string) bool {
	if slices.Contains(m.executables, name) {
		return true
	}
	for _, prefix := range m.executablePrefixes {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	if !slices.Contains(m.runtimes, name) || len(args) <= 1 {
		return false
	}
	entrypoint := args[1]
	if slices.Contains(m.executables, executableName(entrypoint)) {
		return true
	}
	return m.name == "pi" && piPackageEntrypoint(entrypoint)
}

func piPackageEntrypoint(script string) bool {
	script = path.Clean(strings.ToLower(strings.ReplaceAll(script, "\\", "/")))
	for _, entrypoint := range []string{
		"node_modules/@earendil-works/pi-coding-agent/dist/cli.js",
		"node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
	} {
		if script == entrypoint || strings.HasSuffix(script, "/"+entrypoint) {
			return true
		}
	}
	return false
}

func executableName(name string) string {
	// Accept Windows paths in script argv as well as platform-native paths.
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}

func ancestryWrapper(name string, args []string) bool {
	switch name {
	case "sh", "bash", "zsh", "dash", "fish", "cmd", "powershell", "pwsh", "env":
		return true
	case "kata":
		// Find the command after the current root persistent flags. Any daemon
		// command is a barrier, regardless of flags between daemon and start.
		// Unrecognized flags or incomplete argv leave ancestry unresolved;
		// uncertainty must not attribute daemon descendants to its launcher.
		for i := 1; i < len(args); i++ {
			flag, flagValue, assigned := strings.Cut(args[i], "=")
			switch flag {
			case "--format", "--teammate", "--as", "--workspace", "--project", "--daemon":
				if !assigned {
					i++
					if i >= len(args) {
						return false
					}
				}
			case "--json", "--agent", "--quiet", "-q":
				if assigned {
					if _, err := strconv.ParseBool(flagValue); err != nil {
						return false
					}
				}
			default:
				if strings.HasPrefix(args[i], "-") {
					return false
				}
				return args[i] != "daemon"
			}
		}
		return false
	default:
		return false
	}
}

func lookupProcess(pid int32) (name string, args []string, ppid int32, created int64, err error) {
	proc, err := process.NewProcess(pid)
	if err != nil {
		return "", nil, 0, 0, err
	}
	name, err = proc.Name()
	if err != nil {
		return "", nil, 0, 0, err
	}
	args, err = proc.CmdlineSlice()
	if err != nil {
		return "", nil, 0, 0, err
	}
	ppid, err = proc.Ppid()
	if err != nil {
		return "", nil, 0, 0, err
	}
	created, err = proc.CreateTime()
	if err != nil {
		return "", nil, 0, 0, err
	}
	return name, args, ppid, created, nil
}
