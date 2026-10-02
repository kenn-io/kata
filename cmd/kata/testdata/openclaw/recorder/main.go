package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() { os.Exit(openClawRecordNative(os.Args[1:])) }

func openClawRecordNative(args []string) int {
	root := os.Getenv("OPENCLAW_TEST_ROOT")
	cwd, err := os.Getwd()
	if err != nil || root == "" {
		return 1
	}
	entry := struct {
		Args       []string `json:"args"`
		Executable string   `json:"executable"`
		Cwd        string   `json:"cwd"`
		Ref        string   `json:"ref"`
		Recipient  string   `json:"recipient"`
	}{args, os.Args[0], cwd, os.Getenv("KATA_REF"), os.Getenv("KATA_INBOX_USER")}
	data, err := json.Marshal(entry)
	if err != nil {
		return 1
	}
	file, err := os.OpenFile(filepath.Join(root, "calls.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return 1
	}
	_, err = file.Write(append(data, '\n'))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return 1
	}
	if len(args) == 0 {
		return 2
	}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }
	switch args[0] {
	case "agent-hook":
		if isOpenClawAttentionCall(args, "start", "retry-parent-session") && exists("fail-parent-start") {
			return 1
		}
		if isOpenClawAttentionCall(args, "start", "parent-session") && exists("slow-parent-start") {
			time.Sleep(250 * time.Millisecond)
		}
		if len(args) > 3 && args[3] == "start" && exists("slow-attention") {
			time.Sleep(5 * time.Second)
		}
	case "agent-contract-hook":
		if exists("fail-contract") {
			return 1
		}
		if exists("no-contract-context") {
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart"}}); err != nil {
				return 1
			}
			return 0
		}
		text := ""
		content, err := os.ReadFile(filepath.Join(root, "contract"))
		if err != nil {
			return 1
		}
		text = "contract:" + string(content)
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": text}}); err != nil {
			return 1
		}
	case "inbox":
		if exists("fail-inbox") {
			return 1
		}
		content, err := os.ReadFile(filepath.Join(root, "inbox"))
		if err != nil {
			return 1
		}
		if _, err = fmt.Fprint(os.Stdout, string(content)); err != nil {
			return 1
		}
	default:
		return 2
	}
	if len(args) > 1 && args[0] == "agent-hook" && args[1] == "attention-native" {
		completion, err := json.Marshal(struct {
			Args []string `json:"args"`
		}{args})
		if err != nil {
			return 1
		}
		file, err := os.OpenFile(filepath.Join(root, "attention-completions.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return 1
		}
		_, err = file.Write(append(completion, '\n'))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return 1
		}
	}
	return 0
}

func isOpenClawAttentionCall(args []string, mode, session string) bool {
	return len(args) >= 6 && args[0] == "agent-hook" && args[1] == "attention-native" && args[3] == mode && args[4] == "--session" && args[5] == session
}
