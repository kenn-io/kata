package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() { os.Exit(run(os.Getenv("KATA_TEST_CODEX_AGENT"))) }

func run(mode string) int {
	scanner := bufio.NewScanner(os.Stdin)
	if mode == "hang" {
		if scanner.Scan() {
			_, _ = io.Copy(io.Discard, os.Stdin)
		}
		return 0
	}
	for scanner.Scan() {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			return 1
		}
		var result any
		switch req.Method {
		case "initialized":
			continue
		case "initialize":
			result = map[string]any{}
		case "config/read":
			result = map[string]any{"config": map[string]any{"features": map[string]bool{"hooks": true}}}
		case "hooks/list":
			path := filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json")
			data, err := os.ReadFile(path) //nolint:gosec // Test fixture config path.
			if err != nil {
				return 1
			}
			var config struct {
				Hooks map[string][]struct {
					Hooks []json.RawMessage `json:"hooks"`
				} `json:"hooks"`
			}
			if json.Unmarshal(data, &config) != nil {
				return 1
			}
			var hooks []map[string]any
			for event, groups := range config.Hooks {
				for i, group := range groups {
					for j := range group.Hooks {
						trust := os.Getenv("TEST_HOOK_TRUST")
						if trust == "one-modified" {
							trust = "trusted"
							if strings.EqualFold(event, "SessionEnd") {
								trust = "modified"
							}
						}
						hooks = append(hooks, map[string]any{"source": "user", "sourcePath": path, "key": fmt.Sprintf("%s:%s:%d:%d", path, strings.ToLower(event), i, j), "enabled": true, "trustStatus": trust})
					}
				}
			}
			result = map[string]any{"data": []any{map[string]any{"hooks": hooks, "errors": []any{}}}}
		default:
			return 1
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"id": req.ID, "result": result}); err != nil {
			return 1
		}
	}
	return 0
}
