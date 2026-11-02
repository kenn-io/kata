package main

import "strings"

func selectedVerificationHint(hint string) string {
	if flags.Daemon == "" {
		return hint
	}
	replacement := "kata --daemon '" + strings.ReplaceAll(flags.Daemon, "'", "'\\''") + "' "
	lines := strings.Split(hint, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "kata ") && !strings.HasPrefix(line, "kata --daemon ") {
			lines[i] = replacement + strings.TrimPrefix(line, "kata ")
		}
	}
	return strings.Join(lines, "\n")
}
