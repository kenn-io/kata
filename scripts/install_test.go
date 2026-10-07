package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the actual downloader against local HTTP, with only GitHub
// routing and the install directory changed. They never install on the host.
type installFixture struct {
	root, tools, target, scratch, downloader string
	platformOS, platformArch                 string
	accelerate                               bool
	server                                   *httptest.Server
	binary                                   []byte
	archiveStarted                           chan struct{}
	releaseArchive                           chan struct{}
	stop                                     chan struct{}
}

func newInstallFixture(t *testing.T, tool, scenario string) *installFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Bash installer is tested on Unix")
	}
	downloader, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("%s unavailable: %v", tool, err)
	}
	f := &installFixture{root: t.TempDir(), downloader: downloader, binary: []byte("#!/bin/sh\n# synthetic release\nexit 0\n"), archiveStarted: make(chan struct{}), releaseArchive: make(chan struct{}), stop: make(chan struct{})}
	if scenario == "web-assets-invalid" {
		f.binary = []byte("#!/bin/sh\nexit 1\n")
	}
	f.platformOS, f.platformArch = "linux", "amd64"
	f.tools = filepath.Join(f.root, "tools")
	f.target = filepath.Join(f.root, "bin")
	f.scratch = filepath.Join(f.root, "scratch")
	for _, dir := range []string{f.tools, f.target, f.scratch} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	for _, name := range []string{"bash", "awk", "tail", "tr", "tar", "gzip", "mv", "chmod", "uname", "mkdir", "mktemp", "rm", "cut", "grep"} {
		path, err := exec.LookPath(name)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(f.tools, name)))
	}
	for _, name := range []string{"sha256sum", "shasum"} {
		if path, err := exec.LookPath(name); err == nil {
			require.NoError(t, os.Symlink(path, filepath.Join(f.tools, name)))
		}
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "kata", Mode: 0o755, Size: int64(len(f.binary))}))
	_, err = tw.Write(f.binary)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	sum := fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			if scenario == "lookup-failure" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if scenario == "lookup-invalid" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if scenario == "lookup-stall" {
				<-f.stop
				return
			}
			http.Redirect(w, r, "/releases/tag/v0.18.0", http.StatusFound)
		case "/releases/tag/v0.18.0":
			w.WriteHeader(http.StatusOK)
		case "/releases/download/v0.18.0/SHA256SUMS":
			if scenario == "checksum-failure" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if scenario == "checksum-mismatch" {
				_, _ = fmt.Fprintln(w, strings.Repeat("0", 64)+"  kata_0.18.0_linux_amd64.tar.gz")
				return
			}
			_, _ = fmt.Fprintf(w, "%s  kata_0.18.0_linux_amd64.tar.gz\n%s  kata_0.18.0_darwin_arm64.tar.gz\n", sum, sum)
		case "/releases/download/v0.18.0/kata_0.18.0_linux_amd64.tar.gz", "/releases/download/v0.18.0/kata_0.18.0_darwin_arm64.tar.gz":
			if scenario == "archive-failure" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if scenario == "gated" {
				select {
				case f.archiveStarted <- struct{}{}:
				case <-f.stop:
					return
				}
				select {
				case <-f.releaseArchive:
				case <-f.stop:
					return
				}
			}
			if scenario == "archive-partial" {
				w.Header().Set("Content-Length", fmt.Sprint(archive.Len()+100))
				_, _ = w.Write(archive.Bytes()[:archive.Len()/2])
				return
			}
			if scenario == "archive-stall" {
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-f.stop:
				}
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(archive.Len()))
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { close(f.stop); f.server.Close() })
	// The adapter forwards every option to the real client, changing only the
	// fixed release origin. Later timeout tests may accelerate native deadlines.
	adapter := `#!/bin/bash
args=()
previous=""
for arg in "$@"; do
 case "$arg" in
  https://github.com/kenn-io/kata/*) arg="$INSTALL_TEST_SERVER/${arg#https://github.com/kenn-io/kata/}" ;;
 esac
 if [[ "${INSTALL_TEST_ACCELERATE:-}" == 1 ]]; then
  case "$previous:$arg" in
   --speed-time:60|--max-time:60|-T:30) arg=1 ;;
  esac
 fi
 previous="$arg"
 args+=("$arg")
done
exec "$INSTALL_TEST_DOWNLOADER" "${args[@]}"
`
	require.NoError(t, os.WriteFile(filepath.Join(f.tools, tool), []byte(adapter), 0o700)) //nolint:gosec // G306: the owner-only temporary downloader adapter must be executable.
	return f
}

func (f *installFixture) command(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	installer, err := filepath.Abs("install.sh")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, filepath.Join(f.tools, "bash"), "--noprofile", "--norc", "-s", "--", installer, f.target) //nolint:gosec // G204: test-owned Bash path and fixed installer arguments.
	cmd.Stdin = strings.NewReader("source \"$1\"\nINSTALL_TEST_DIR=\"$2\"\nfind_install_dir() { printf '%s\\n' \"$INSTALL_TEST_DIR\"; }\n" + "detect_os() { printf '%s\\n' \"$INSTALL_TEST_OS\"; }\ndetect_arch() { printf '%s\\n' \"$INSTALL_TEST_ARCH\"; }\n" + script + "\n")
	cmd.Env = []string{"PATH=" + f.tools, "HOME=" + f.root, "TMPDIR=" + f.scratch, "INSTALL_TEST_SERVER=" + f.server.URL, "INSTALL_TEST_DOWNLOADER=" + f.downloader, "LC_ALL=C", "INSTALL_TEST_OS=" + f.platformOS, "INSTALL_TEST_ARCH=" + f.platformArch}
	if f.accelerate {
		cmd.Env = append(cmd.Env, "INSTALL_TEST_ACCELERATE=1")
	}
	cmd.WaitDelay = time.Second
	return cmd
}

type installOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	marker string
	ready  chan struct{}
}

func (b *installOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buffer.Write(p)
	if b.ready != nil && strings.Contains(b.buffer.String(), b.marker) {
		close(b.ready)
		b.ready = nil
	}
	return n, err
}
func (b *installOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buffer.String() }

func TestShellInstallerProgress(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		t.Run(tool, func(t *testing.T) {
			f := newInstallFixture(t, tool, "gated")
			for attempt := range 2 {
				stagePrinted := make(chan struct{})
				out := installOutput{marker: "Downloading kata_0.18.0_linux_amd64.tar.gz", ready: stagePrinted}
				var diagnostics installOutput
				cmd := f.command(t, "main")
				cmd.Stdout = &out
				cmd.Stderr = &diagnostics
				require.NoError(t, cmd.Start())
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case <-f.archiveStarted:
				case err := <-done:
					t.Fatalf("installer exited before archive request: %v\n%s\n%s", err, out.String(), diagnostics.String())
				case <-time.After(8 * time.Second):
					t.Fatal("archive request never began")
				}
				select {
				case <-stagePrinted:
				case <-time.After(time.Second):
					t.Fatal("installer must print its stage before the blocked transfer completes")
				}
				assert.Contains(t, out.String(), "Installing kata...")
				if attempt == 0 {
					close(f.releaseArchive)
				}
				require.NoError(t, <-done, out.String()+diagnostics.String())
				assert.Contains(t, out.String(), "Downloading SHA256SUMS")
				assert.Contains(t, out.String(), "Installing binary")
				assert.Contains(t, out.String(), "Installation complete!")
				assert.Regexp(t, `100(\.0)?%`, diagnostics.String(), "native transfer progress must remain visible with piped stdin and non-TTY stderr")
				installed, err := os.ReadFile(filepath.Join(f.target, "kata"))
				require.NoError(t, err)
				assert.Equal(t, f.binary, installed)
				entries, err := os.ReadDir(f.scratch)
				require.NoError(t, err)
				assert.Empty(t, entries)
			}
		})
	}
}

func (f *installFixture) failedInstall(t *testing.T) (string, string) {
	t.Helper()
	original := []byte("existing installation\n")
	require.NoError(t, os.WriteFile(filepath.Join(f.target, "kata"), original, 0o700)) //nolint:gosec // G306: the owner-only existing-install fixture must be executable.
	var out, diagnostics bytes.Buffer
	cmd := f.command(t, "main")
	cmd.Stdout = &out
	cmd.Stderr = &diagnostics
	require.Error(t, cmd.Run(), out.String()+diagnostics.String())
	require.NotEqual(t, -1, cmd.ProcessState.ExitCode(), "downloader must terminate before the test watchdog: "+diagnostics.String())
	assert.NotContains(t, out.String(), "Installation complete!")
	installed, err := os.ReadFile(filepath.Join(f.target, "kata"))
	require.NoError(t, err)
	assert.Equal(t, original, installed)
	entries, err := os.ReadDir(f.scratch)
	require.NoError(t, err)
	assert.Empty(t, entries, "failed install must remove temporary downloads")
	return out.String(), diagnostics.String()
}

func TestShellInstallerLookupFailure(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		for _, scenario := range []string{"lookup-failure", "lookup-invalid", "no-downloader"} {
			t.Run(tool+"/"+scenario, func(t *testing.T) {
				f := newInstallFixture(t, tool, scenario)
				if scenario == "no-downloader" {
					require.NoError(t, os.Remove(filepath.Join(f.tools, tool)))
				}
				_, diagnostics := f.failedInstall(t)
				assert.Contains(t, diagnostics, "latest release")
				if scenario == "no-downloader" {
					assert.Contains(t, diagnostics, "curl or wget")
				} else {
					assert.Contains(t, diagnostics, "retry")
				}
				if scenario == "lookup-failure" {
					assert.Contains(t, diagnostics, "503", "retain the downloader's underlying HTTP failure")
				}
			})
		}
	}
}

func TestShellInstallerLookupStdout(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		t.Run(tool, func(t *testing.T) {
			f := newInstallFixture(t, tool, "")
			var diagnostics bytes.Buffer
			cmd := f.command(t, "get_latest_version")
			cmd.Stderr = &diagnostics
			out, err := cmd.Output()
			require.NoError(t, err, diagnostics.String())
			assert.Equal(t, "v0.18.0\n", string(out), "progress and headers must not pollute the captured release tag")
		})
	}
}

func TestShellInstallerArtifactFailure(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		for _, tc := range []struct{ scenario, artifact string }{
			{"archive-failure", "kata_0.18.0_linux_amd64.tar.gz"},
			{"archive-partial", "kata_0.18.0_linux_amd64.tar.gz"},
			{"checksum-failure", "SHA256SUMS"},
		} {
			t.Run(tool+"/"+tc.scenario, func(t *testing.T) {
				f := newInstallFixture(t, tool, tc.scenario)
				_, diagnostics := f.failedInstall(t)
				assert.Contains(t, diagnostics, "Could not download "+tc.artifact)
				assert.Contains(t, diagnostics, "retry")
				if tc.scenario != "archive-partial" {
					assert.Contains(t, diagnostics, "503")
				}
			})
		}
	}
}

func TestShellInstallerStalledNetwork(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		for _, scenario := range []string{"lookup-stall", "archive-stall"} {
			t.Run(tool+"/"+scenario, func(t *testing.T) {
				f := newInstallFixture(t, tool, scenario)
				f.accelerate = true
				_, diagnostics := f.failedInstall(t)
				assert.Contains(t, diagnostics, "retry")
				if scenario == "lookup-stall" {
					assert.Contains(t, diagnostics, "latest release")
				} else {
					assert.Contains(t, diagnostics, "kata_0.18.0_linux_amd64.tar.gz")
				}
			})
		}
	}
}

func TestShellInstallerValidationBeforeReplacement(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		for _, tc := range []struct{ scenario, diagnostic string }{
			{"checksum-mismatch", "Checksum verification failed"},
			{"web-assets-invalid", "validated Kata web UI"},
		} {
			t.Run(tool+"/"+tc.scenario, func(t *testing.T) {
				f := newInstallFixture(t, tool, tc.scenario)
				_, diagnostics := f.failedInstall(t)
				assert.Contains(t, diagnostics, tc.diagnostic)
			})
		}
	}
}

func TestShellInstallerDarwinARM64Archive(t *testing.T) {
	for _, tool := range []string{"curl", "wget"} {
		t.Run(tool, func(t *testing.T) {
			f := newInstallFixture(t, tool, "")
			f.platformOS, f.platformArch = "darwin", "arm64"
			out, err := f.command(t, "main").CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Contains(t, string(out), "kata_0.18.0_darwin_arm64.tar.gz")
			installed, err := os.ReadFile(filepath.Join(f.target, "kata"))
			require.NoError(t, err)
			assert.Equal(t, f.binary, installed)
		})
	}
}

func TestShellInstallerBusyBoxWget(t *testing.T) {
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Skip("BusyBox unavailable")
	}
	for _, scenario := range []string{"", "lookup-failure", "archive-failure", "lookup-stall", "archive-stall"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInstallFixture(t, "wget", scenario)
			native := filepath.Join(f.root, "native")
			require.NoError(t, os.Mkdir(native, 0o700))
			f.downloader = filepath.Join(native, "wget")
			require.NoError(t, os.Symlink(busybox, f.downloader))
			f.accelerate = true
			if scenario != "" {
				_, diagnostics := f.failedInstall(t)
				assert.NotContains(t, diagnostics, "unrecognized option")
				assert.Contains(t, diagnostics, "retry")
				if strings.HasSuffix(scenario, "failure") {
					assert.Contains(t, diagnostics, "503")
				}
				return
			}
			out, err := f.command(t, "main").CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Contains(t, string(out), "Downloading SHA256SUMS")
			assert.Contains(t, string(out), "Installation complete!")
			installed, err := os.ReadFile(filepath.Join(f.target, "kata"))
			require.NoError(t, err)
			assert.Equal(t, f.binary, installed)
		})
	}
}
