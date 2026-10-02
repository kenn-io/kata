package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// These are the existing adapter floors, exercised by the published SDK tests:
// @opencode-ai/plugin 1.0.154 (v1), and @opencode/plugin 2.0.0 (v2).
// Future majors are unsupported; prereleases are not stable runtime evidence.
var openCodeStableVersion = regexp.MustCompile(`^(?:opencode )?v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func parseOpenCodeRuntimeAPI(output string) (api string, known bool, err error) {
	match := openCodeStableVersion.FindStringSubmatch(strings.TrimSpace(output))
	if match == nil {
		return "", false, fmt.Errorf("OpenCode --version did not report a recognized stable release")
	}
	major, e1 := strconv.Atoi(match[1])
	minor, e2 := strconv.Atoi(match[2])
	patch, e3 := strconv.Atoi(match[3])
	if e1 != nil || e2 != nil || e3 != nil {
		return "", false, fmt.Errorf("OpenCode version numbers are out of range")
	}
	switch {
	case major == 1 && (minor > 0 || patch >= 154):
		return "v1", true, nil
	case major == 2:
		return "v2", true, nil
	default:
		return "", true, fmt.Errorf("unsupported OpenCode release %s; supported stable versions are v1 >=1.0.154 or v2 >=2.0.0", strings.TrimSpace(output))
	}
}

type openCodeVersionOutput struct{ buffer bytes.Buffer }

func (out *openCodeVersionOutput) Write(data []byte) (int, error) {
	if out.buffer.Len()+len(data) > 4096 {
		return 0, fmt.Errorf("OpenCode version output exceeds 4096 bytes")
	}
	return out.buffer.Write(data)
}

func resolveOpenCodeRuntimeAPI(ctx context.Context, override string) (string, string, error) {
	path, err := exec.LookPath("opencode")
	var output openCodeVersionOutput
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		command := exec.CommandContext(probeCtx, path, "--version") //nolint:gosec // G204: selected agent executable, fixed literal argv; no shell.
		command.Stdout = &output
		command.Stderr = io.Discard
		command.WaitDelay = 100 * time.Millisecond
		err = command.Run()
		if probeCtx.Err() != nil {
			err = probeCtx.Err()
		}
	}
	api, known := "", false
	if err == nil {
		api, known, err = parseOpenCodeRuntimeAPI(output.buffer.String())
	}
	if err != nil {
		if override != "" && !known && ctx.Err() == nil {
			return override, "OpenCode runtime API could not be verified; using explicit --api " + override + ". Check the installed runtime before loading the plugin.", nil
		}
		return "", "", agentHookUsage(fmt.Sprintf("detect OpenCode API: %v; check opencode on PATH or use --api v1|v2 when runtime detection is unavailable", err))
	}
	if override != "" && api != override {
		return "", "", agentHookUsage(fmt.Sprintf("OpenCode runtime requires %s, conflicting with --api %s; select the matching runtime/API before installing", api, override))
	}
	return api, "", nil
}
