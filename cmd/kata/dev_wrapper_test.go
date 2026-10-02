package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the launcher with a fake compiler and command, so the test checks
// the actual child environment and cleanup instead of script source strings.
func TestDevKataWrapperIsolation(t *testing.T) {
	testDevKataWrapperIsolation(t, "bash")
}

// macOS's system Bash is 3.2. Other platforms can exercise that interpreter
// explicitly without adding a legacy shell to their normal test prerequisites.
func TestDevKataWrapperLegacyBash(t *testing.T) {
	interpreter := os.Getenv("KATA_TEST_BASH32")
	if interpreter == "" && runtime.GOOS == "darwin" {
		interpreter = "/bin/bash"
	}
	if interpreter == "" {
		t.Skip("Bash 3.2 interpreter unavailable")
	}
	version, err := exec.Command(interpreter, "--version").Output() //nolint:gosec // G204: the test explicitly selects its Bash interpreter.
	require.NoError(t, err)
	if !strings.Contains(string(version), "version 3.2") {
		t.Skip("selected interpreter is not Bash 3.2")
	}
	testDevKataWrapperIsolation(t, interpreter)
}

func testDevKataWrapperIsolation(t *testing.T, interpreter string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX development wrapper")
	}
	bin := t.TempDir()
	compiler := `#!/bin/bash
while [ "$#" -gt 0 ]; do
 if [ "$1" = -o ]; then output="$2"; shift 2; else shift; fi
done
cat > "$output" <<'COMMAND'
#!/bin/bash
if [ "$1" = daemon ] || [ "$1" = create ] || [ "$1" = --as ]; then exit 0; fi
if [ "$1" = init ]; then
 shift
 while [ "$#" -gt 0 ]; do
  if [ "$1" = --project ]; then shift 2; else exit 2; fi
 done
 exit 0
fi
printf '%s\n' "$KATA_HOME" "$KATA_DB" "$PWD" "${KATA_SERVER-unset}" "${KATA_DSN-unset}" "${KATA_AUTH_TOKEN-unset}" "${PORT-unset}" "${GOPROXY-unset}" "<${NO_COLOR-unset}>"
COMMAND
chmod +x "$output"
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(compiler), 0700)) //nolint:gosec // G306: the fixture must be executable by the wrapper.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DB", filepath.Join(t.TempDir(), "live.db"))
	t.Setenv("KATA_SERVER", "https://daemon.example")
	t.Setenv("KATA_DSN", "postgres://daemon.example/example")
	t.Setenv("KATA_AUTH_TOKEN", "example-token")
	t.Setenv("PORT", "7777")
	t.Setenv("GOPROXY", "https://proxy.example")
	t.Setenv("NO_COLOR", "")
	for _, mode := range []string{"version", "--demo"} {
		command := exec.Command(interpreter, "../../scripts/dev-kata.sh", mode) //nolint:gosec // G204: exercise the wrapper through the selected test shell.
		out, err := command.CombinedOutput()
		require.NoError(t, err, string(out))
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		require.Len(t, lines, 9)
		require.NotEqual(t, os.Getenv("KATA_HOME"), lines[0])
		require.NotEqual(t, os.Getenv("KATA_DB"), lines[1])
		require.Equal(t, filepath.Join(lines[0], "kata.db"), lines[1])
		require.Equal(t, filepath.Join(filepath.Dir(lines[0]), "workspace"), lines[2])
		require.Equal(t, []string{"unset", "unset", "unset", "unset", "unset"}, lines[3:8])
		require.Equal(t, "<>", lines[8], "set-but-empty allowlisted variables must survive")
		_, err = os.Stat(filepath.Dir(lines[0])) //nolint:gosec // G703: lines[0] is the wrapper's temporary home, asserted above.
		require.True(t, os.IsNotExist(err), "launcher must clean its temporary state")
	}
}

// The replaced TUI targets disabled VCS stamping. Preserve that tool contract
// when Git is present but cannot report status, as in ownership-restricted CI.
func TestDevKataWrapperBuildWithoutUsableGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX development wrapper")
	}
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	fixture := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(fixture, "scripts"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(fixture, "cmd", "kata"), 0700))
	script, err := os.ReadFile("../../scripts/dev-kata.sh")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "scripts", "dev-kata.sh"), script, 0700)) //nolint:gosec // G306: the fixture is an executable shell script.
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "go.mod"), []byte("module example.com/example-workspace\n\ngo 1.27\n"), 0600))
	main := `package main
import ("fmt"; "os")
func main() { if len(os.Args)>1 && os.Args[1]=="daemon" {return}; fmt.Println("isolated build succeeded") }
`
	require.NoError(t, os.WriteFile(filepath.Join(fixture, "cmd", "kata", "main.go"), []byte(main), 0600))
	init := exec.Command(realGit, "init", "--quiet", fixture) //nolint:gosec // G204: initialize only the test-owned temporary fixture.
	require.NoError(t, init.Run())
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 1\n"), 0700)) //nolint:gosec // G306: the fake Git command must be executable.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := exec.Command("bash", filepath.Join(fixture, "scripts", "dev-kata.sh"), "version") //nolint:gosec // G204: execute the test-owned wrapper fixture.
	out, err := command.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, "isolated build succeeded", strings.TrimSpace(string(out)))
}
