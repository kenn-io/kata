package testfix

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RunIsolated gives a test package its own application and user home. Child test
// processes retain their parent's explicit fixture setup.
func RunIsolated(m *testing.M) int {
	if os.Getenv("KATA_TEST_HOME") != "" {
		return m.Run()
	}
	output, err := exec.Command("go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve test toolchain: %v\n", err)
		return 1
	}
	var toolchain map[string]string
	if err := json.Unmarshal(output, &toolchain); err != nil {
		fmt.Fprintf(os.Stderr, "decode test toolchain: %v\n", err)
		return 1
	}
	home, err := os.MkdirTemp("", "kata-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test home: %v\n", err)
		return 1
	}
	environment := map[string]string{"HOME": home, "USERPROFILE": home, "KATA_HOME": home, "KATA_TEST_HOME": home}
	maps.Copy(environment, toolchain)
	// Keep the selected Go executable when HOME changes a tool manager's lookup.
	environment["PATH"] = filepath.Join(toolchain["GOROOT"], "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(name, "GIT_") {
			_ = os.Unsetenv(name)
		}
	}
	environment["GIT_CONFIG_GLOBAL"] = filepath.Join(home, ".gitconfig")
	environment["GIT_CONFIG_NOSYSTEM"] = "1"
	for name, value := range environment {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "set test environment: %v\n", err)
			_ = os.RemoveAll(home)
			return 1
		}
	}
	code := m.Run()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "remove test home: %v\n", err)
		code = 1
	}
	return code
}
