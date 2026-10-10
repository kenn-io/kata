package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"time"
)

func main() {
	if os.Getenv("GO_WANT_SHOW_MARKDOWN_HELPER") != "1" {
		return
	}
	marker := slices.Index(os.Args, "--")
	if marker < 0 || marker+1 >= len(os.Args) {
		os.Exit(20)
	}
	mode := os.Args[marker+1]
	switch mode {
	case "echo", "echo-newline":
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(21)
		}
		fmt.Printf("arg=%s env=%s input=%s", os.Args[marker+2], os.Getenv("SHOW_RENDER_ENV"), payload)
		if mode == "echo-newline" {
			fmt.Println()
		}
	case "fail":
		payload, _ := io.ReadAll(os.Stdin)
		_, _ = fmt.Fprintf(os.Stderr, "renderer rejected %s", payload)
		os.Exit(9)
	case "wait":
		// Keep a timer registered so the runtime does not mistake this helper
		// process for a deadlock and exit before the renderer cancels it.
		time.Sleep(24 * time.Hour)
	case "spawn-descendant":
		readyPath := os.Args[marker+2]
		//nolint:gosec // G204: this test starts its own fixed test binary helper with fixed arguments.
		child := exec.Command(
			os.Args[0], "--", "wait",
		)
		child.Env = os.Environ()
		configureShowMarkdownHelperChild(child)
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(23)
		}
		// Publish readiness by rename so the parent cannot observe a partial PID.
		readyTempPath := readyPath + ".tmp"
		//nolint:gosec // G703: readyTempPath is a test-owned path created under t.TempDir.
		if err := os.WriteFile(readyTempPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(24)
		}
		//nolint:gosec // G703: both paths are test-owned paths created under t.TempDir.
		if err := os.Rename(readyTempPath, readyPath); err != nil {
			os.Exit(25)
		}
		// Keep a timer registered for the same reason as the wait helper. The
		// renderer cancellation path is responsible for ending this process.
		time.Sleep(24 * time.Hour)
	default:
		os.Exit(22)
	}
	os.Exit(0)
}
