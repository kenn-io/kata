package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/spf13/cobra"
)

const attentionSessionKey = "work.attention_session"

// Installed adapters carry session ownership. The legacy bare hook remains a
// separate stdin-ignoring compatibility entry point.
func newNativeAgentAttentionCmd() *cobra.Command {
	var session, ref string
	var hostPID int
	cmd := &cobra.Command{
		Use: "attention-native <harness> <start|end>", Short: "Track a native session's attention ownership", Hidden: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(2)(cmd, args); err != nil {
				return err
			}
			if _, err := canonicalNativeAttentionTarget(args[0]); err != nil {
				return err
			}
			if args[1] != "start" && args[1] != "end" {
				return agentHookUsage("attention-native requires start or end")
			}
			if cmd.Flags().Changed("session") && strings.TrimSpace(session) == "" {
				return agentHookUsage("--session must not be empty")
			}
			if cmd.Flags().Changed("host-pid") && hostPID <= 0 {
				return agentHookUsage("--host-pid must be positive")
			}
			if cmd.Flags().Changed("host-pid") && !cmd.Flags().Changed("session") {
				return agentHookUsage("--host-pid requires --session")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _ := canonicalNativeAttentionTarget(args[0])
			cwd := ""
			if !cmd.Flags().Changed("session") {
				var err error
				session, cwd, err = readNativeAttentionPayloadFor(target, args[1], cmd.InOrStdin())
				if err != nil {
					return nil
				}
			}
			if !cmd.Flags().Changed("ref") {
				ref = os.Getenv("KATA_REF")
			}
			if _, ok := attentionRef(ref); !ok {
				return nil
			}
			var generation string
			var err error
			if hostPID > 0 {
				generation, err = nativeAttentionLaunchGeneration(hostPID)
			} else {
				generation, err = nativeAttentionAncestorGeneration()
			}
			if err != nil {
				return nil
			}
			sum := sha256.Sum256([]byte(target + "\x00" + generation + "\x00" + session))
			owner := hex.EncodeToString(sum[:])
			// Native routing is authoritative; do not apply Claude's legacy fallback.
			if flags.Workspace == "" && cwd != "" {
				previous := flags.Workspace
				flags.Workspace = cwd
				defer func() { flags.Workspace = previous }()
			}
			timeout := 3 * time.Second
			if args[1] == "end" {
				timeout = 1250 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			previous := cmd.Context()
			cmd.SetContext(ctx)
			defer cmd.SetContext(previous)
			d := &liveAttnDaemon{cmd: cmd}
			if args[1] == "start" {
				return attnStartSession(d, ref, owner)
			}
			return attnEndSession(d, ref, owner)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "native session ID; bypasses stdin")
	cmd.Flags().IntVar(&hostPID, "host-pid", 0, "native extension host PID for launch identity")
	cmd.Flags().StringVar(&ref, "ref", "", "captured tracked issue reference (default KATA_REF)")
	return cmd
}

func canonicalNativeAttentionTarget(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "factory":
		name = "droid"
	case "agy", "antigravity-cli":
		name = "antigravity"
	}
	switch name {
	case "claude", "codex", "gemini", "copilot", "cursor", "qwen", "hermes", "droid", "antigravity", "amp", "opencode", "pi", "openclaw", "kimi-code", "kimi", "muse", "grok", "zcode":
		return name, nil
	}
	return "", agentHookUsage(fmt.Sprintf("unknown harness %q", name))
}

func decodeNativeAttentionPayload(input io.Reader) (map[string]any, error) {
	const limit = 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("native attention payload too large")
	}
	var payload map[string]any
	if err = json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, errors.New("native attention payload must be an object")
	}
	return payload, nil
}

func readNativeAttentionPayload(input io.Reader) (session, workspace string, err error) {
	payload, err := decodeNativeAttentionPayload(input)
	if err != nil {
		return "", "", err
	}
	return nativeAttentionPayloadIdentity(payload)
}

func readNativeAttentionPayloadFor(target, mode string, input io.Reader) (string, string, error) {
	payload, err := decodeNativeAttentionPayload(input)
	if err != nil {
		return "", "", err
	}
	// Per-turn and child-session completions never end the tracked host session.
	for _, key := range []string{"subagentType", "subagent_type", "parent_session_id", "parentSessionId"} {
		if value, exists := payload[key]; exists && value != nil && value != "" {
			return "", "", errors.New("child session attention is not host attention")
		}
	}
	expected := "session" + mode
	if target == "hermes" {
		expected = "onsessionstart"
		if mode == "end" {
			expected = "onsessionfinalize"
		}
	}
	seen := false
	for _, key := range []string{"hook_event_name", "hookEventName"} {
		if raw, exists := payload[key]; exists {
			value, ok := raw.(string)
			normalized := strings.ToLower(strings.ReplaceAll(value, "_", ""))
			matches := normalized == expected || (target == "hermes" && mode == "start" && normalized == "onsessionreset")
			if !ok || !matches {
				return "", "", errors.New("unexpected native lifecycle event")
			}
			seen = true
		}
	}
	if !seen {
		// Copilot camelCase lifecycle payloads omit the event name. Require the
		// documented lifecycle-only discriminator; Stop uses stopReason instead.
		valid := false
		if target == "copilot" {
			if mode == "start" {
				source, _ := payload["source"].(string)
				valid = source == "startup" || source == "resume" || source == "new"
			} else {
				reason, _ := payload["reason"].(string)
				valid = reason == "complete" || reason == "error" || reason == "abort" || reason == "timeout" || reason == "user_exit"
			}
		}
		if !valid {
			return "", "", errors.New("native lifecycle event missing")
		}
	}
	return nativeAttentionPayloadIdentity(payload)
}

func nativeAttentionPayloadIdentity(payload map[string]any) (session, workspace string, err error) {

	for _, key := range []string{"session_id", "sessionId", "sessionID", "conversation_id", "conversationId"} {
		if raw, exists := payload[key]; exists {
			value, ok := raw.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return "", "", errors.New("invalid native session identity")
			}
			if session != "" && session != value {
				return "", "", errors.New("ambiguous native session identity")
			}
			session = value
		}
	}
	if session == "" {
		return "", "", errors.New("native session identity missing")
	}
	var workspaces []string
	if raw, exists := payload["cwd"]; exists {
		path, err := nativeAttentionWorkspacePath(raw)
		if err != nil {
			return "", "", err
		}
		workspaces = append(workspaces, path)
	}
	for _, key := range []string{"workspace_roots", "workspacePaths"} {
		raw, exists := payload[key]
		if !exists {
			continue
		}
		paths, ok := raw.([]any)
		if !ok {
			return "", "", fmt.Errorf("invalid native workspace identity in %s", key)
		}
		if len(paths) != 1 {
			return "", "", fmt.Errorf("ambiguous native workspace identity in %s", key)
		}
		path, err := nativeAttentionWorkspacePath(paths[0])
		if err != nil {
			return "", "", err
		}
		workspaces = append(workspaces, path)
	}
	if len(workspaces) > 0 {
		workspace = workspaces[0]
		for _, candidate := range workspaces[1:] {
			if !sameNativeAttentionWorkspace(workspace, candidate) {
				return "", "", errors.New("conflicting native workspace identities")
			}
		}
	}
	return session, workspace, nil
}

func nativeAttentionWorkspacePath(raw any) (string, error) {
	path, ok := raw.(string)
	if !ok || path == "" || !filepath.IsAbs(path) {
		return "", errors.New("invalid native workspace identity")
	}
	return filepath.Clean(path), nil
}

func sameNativeAttentionWorkspace(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func nativeAttentionLaunchGeneration(pid int) (string, error) {
	if override := os.Getenv("KATA_SESSION_ID"); override != "" {
		return "launcher:" + override, nil
	}
	if pid <= 0 || pid > math.MaxInt32 {
		return "", errors.New("invalid native host PID")
	}
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return "", err
	}
	created, err := proc.CreateTime()
	if err != nil {
		return "", err
	}
	if created <= 0 {
		return "", errors.New("native host creation time unavailable")
	}
	return strconv.Itoa(pid) + ":" + strconv.FormatInt(created, 10), nil
}

func nativeAttentionAncestorGeneration() (string, error) {
	if override := os.Getenv("KATA_SESSION_ID"); override != "" {
		return "launcher:" + override, nil
	}
	pid := int32(os.Getppid()) //nolint:gosec // G115: gopsutil uses int32 PIDs; nonpositive ancestors are rejected before lookup.
	for range 12 {
		if pid <= 1 {
			return "", errors.New("native host ancestor unavailable")
		}
		proc, err := process.NewProcess(pid)
		if err != nil {
			return "", err
		}
		name, err := proc.Name()
		if err != nil {
			return "", err
		}
		name = strings.ToLower(strings.TrimSuffix(filepath.Base(name), ".exe"))
		switch name {
		case "sh", "bash", "zsh", "dash", "fish", "cmd", "powershell", "pwsh", "env", "kata":
			pid, err = proc.Ppid()
			if err != nil {
				return "", err
			}
		default:
			return nativeAttentionLaunchGeneration(int(pid))
		}
	}
	return "", errors.New("native host ancestor depth exceeded")
}

func attnStartSession(d attnDaemon, kataRef, owner string) error {
	ref, ok := attentionRef(kataRef)
	if !ok || owner == "" {
		return nil
	}
	for range attnWriteAttempts {
		lookup := d.lookup(ref)
		switch lookup.kind {
		case lookupTransient:
			return errors.New("native attention start: issue lookup unavailable")
		case lookupGone:
			return nil
		case lookupOpen:
		default:
			return errors.New("native attention start: issue lookup unavailable")
		}
		if lookup.session == owner {
			return nil
		}
		switch d.setMetaIfRevision(ref, map[string]string{attentionKey: attnValueOK, attentionSessionKey: owner}, lookup.revision) {
		case attnWriteApplied:
			return nil
		case attnWriteFailed:
			return errors.New("native attention start: metadata update failed")
		case attnWriteConflict:
			continue
		default:
			return errors.New("native attention start: metadata update failed")
		}
	}
	return errors.New("native attention start: metadata changed repeatedly")
}
func attnEndSession(d attnDaemon, kataRef, owner string) error {
	ref, ok := attentionRef(kataRef)
	if !ok || owner == "" {
		return nil
	}
	for range attnWriteAttempts {
		lookup := d.lookup(ref)
		switch lookup.kind {
		case lookupTransient:
			return errors.New("native attention end: issue lookup unavailable")
		case lookupGone:
			return nil
		case lookupOpen:
		default:
			return errors.New("native attention end: issue lookup unavailable")
		}
		if lookup.session != owner {
			return nil
		}
		patch := map[string]string{attentionSessionKey: "ended:" + owner}
		if lookup.attention == attnValueOK {
			patch[attentionKey] = attnValueNeedsHuman
			patch[attentionMsgKey] = attnHandoffMsg
		}
		switch d.setMetaIfRevision(ref, patch, lookup.revision) {
		case attnWriteApplied:
			return nil
		case attnWriteFailed:
			return errors.New("native attention end: metadata update failed")
		case attnWriteConflict:
			continue
		default:
			return errors.New("native attention end: metadata update failed")
		}
	}
	return errors.New("native attention end: metadata changed repeatedly")
}
