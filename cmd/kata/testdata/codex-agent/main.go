package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() { os.Exit(run(os.Getenv("KATA_TEST_CODEX_AGENT"))) }

// run answers the app-server requests agenthook.Check sends. Trust keys follow
// Codex's group and handler indices so tests can observe index changes.
func run(mode string) int {
	time.Sleep(map[string]time.Duration{"slow": 2 * time.Second, "slower": 4 * time.Second}[mode])
	if mode == "hang" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			return 1
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialized":
			continue
		case "initialize":
		case "config/read":
			result = map[string]any{"config": map[string]any{"features": map[string]bool{"hooks": true}}}
		case "hooks/list":
			path := filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json")
			data, err := os.ReadFile(path) //nolint:gosec // Test fixture config path.
			var config struct {
				Hooks map[string][]struct {
					Hooks []json.RawMessage `json:"hooks"`
				} `json:"hooks"`
			}
			if err != nil || json.Unmarshal(data, &config) != nil {
				return 1
			}
			hooks := []map[string]any{}
			for event, groups := range config.Hooks {
				for i, group := range groups {
					for j := range group.Hooks {
						trust := os.Getenv("TEST_HOOK_TRUST")
						if trust == "one-modified" {
							trust = map[bool]string{true: "modified", false: "trusted"}[strings.EqualFold(event, "SessionEnd")]
						}
						hooks = append(hooks, map[string]any{"source": "user", "sourcePath": path, "key": fmt.Sprintf("%s:%s:%d:%d", path, strings.ToLower(event), i, j), "enabled": true, "trustStatus": trust})
					}
				}
			}
			result = map[string]any{"data": []any{map[string]any{"hooks": hooks, "errors": []any{}}}}
		default:
			return 1
		}
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"id": req.ID, "result": result}) != nil {
			return 1
		}
	}
	return 0
}
