package config

import (
	"runtime"
	"slices"
	"strings"
)

var harnessEnvKeys = [...]string{
	"AGENT",
	"CLAUDE_CODE_SESSION_ID",
	"CODEX_SESSION_ID",
	"CODEX_THREAD_ID",
	"CURSOR_AGENT",
	"GEMINI_CLI",
	"KATA_HARNESS_MODEL",
	"KATA_HARNESS_SESSION",
	"OMPCODE",
	"OPENCLAW_SHELL",
	"PI_CODING_AGENT",
	"PI_MODEL",
	"PI_PROVIDER",
	"PI_SESSION_ID",
	"QWEN_CODE",
}

// StripHarnessEnv returns a copy of environ without coding-agent identity keys.
// Key matching follows the host platform's environment-variable rules.
func StripHarnessEnv(environ []string) []string {
	return FilterHarnessEnv(environ, runtime.GOOS == "windows")
}

// FilterHarnessEnv returns a copy of environ without coding-agent identity
// keys. Other entries retain their order and contents. Set caseInsensitive for
// Windows environment-variable matching.
func FilterHarnessEnv(environ []string, caseInsensitive bool) []string {
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if caseInsensitive {
			key = strings.ToUpper(key)
		}
		if !slices.Contains(harnessEnvKeys[:], key) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
