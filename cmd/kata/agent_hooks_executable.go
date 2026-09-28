package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type agentHookExecutableEnv struct {
	Executable   func() (string, error)
	EvalSymlinks func(string) (string, error)
	Path         string
	GOOS         string
}

func resolveAgentHookExecutable(override string, env agentHookExecutableEnv) (string, string, error) {
	if override != "" {
		path, err := exec.LookPath(override)
		if err != nil {
			return "", "", fmt.Errorf("resolve hook executable: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", "", fmt.Errorf("inspect hook executable: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("hook executable %s is not a regular file", path)
		}
		path, err = filepath.Abs(path)
		return path, "", err
	}
	running, err := env.Executable()
	if err != nil {
		return "", "", err
	}
	running, err = filepath.Abs(running)
	if err != nil {
		return "", "", err
	}
	resolved, resolveErr := env.EvalSymlinks(running)
	name := "kata"
	if env.GOOS == "windows" {
		name += ".exe"
	}
	if resolveErr == nil {
		for _, dir := range filepath.SplitList(env.Path) {
			if dir == "" {
				dir = "."
			}
			candidate, err := filepath.Abs(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			info, err := os.Stat(candidate)
			if err != nil || !info.Mode().IsRegular() || (env.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
				continue
			}
			actual, err := env.EvalSymlinks(candidate)
			if err == nil && (actual == resolved || (env.GOOS == "windows" && strings.EqualFold(actual, resolved))) {
				return candidate, "", nil
			}
		}
	}
	return running, "using the running executable path; it may not survive an upgrade (use --executable for a stable path)", nil
}

// agentHookCommandExecutable reads only the first argument. It never evaluates
// shell expansions or executes a hook. Kit defines the command quoting forms.
func agentHookCommandExecutable(command, goos string, powershell bool) (string, error) {
	command = strings.TrimSpace(command)
	if powershell {
		command = strings.TrimSpace(strings.TrimPrefix(command, "&"))
		return firstAgentHookShellWord(command, true)
	}
	if goos == "windows" {
		return firstAgentHookWindowsWord(command)
	}
	return firstAgentHookShellWord(command, false)
}

func firstAgentHookShellWord(command string, powershell bool) (string, error) {
	var word strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote == '\'' {
			if c == '\'' {
				if powershell && i+1 < len(command) && command[i+1] == '\'' {
					word.WriteByte(c)
					i++
					continue
				}
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if quote == '"' {
			if c == '"' {
				quote = 0
				continue
			}
			if !powershell && c == '\\' && i+1 < len(command) && strings.ContainsRune("\"\\$`\n", rune(command[i+1])) {
				i++
				c = command[i]
			}
			word.WriteByte(c)
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		started = true
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if !powershell && c == '\\' {
			i++
			if i >= len(command) {
				return "", fmt.Errorf("incomplete executable escape")
			}
			c = command[i]
		}
		word.WriteByte(c)
	}
	if quote != 0 || !started || word.Len() == 0 {
		return "", fmt.Errorf("invalid quoted hook executable")
	}
	return word.String(), nil
}

func firstAgentHookWindowsWord(command string) (string, error) {
	var word strings.Builder
	quoted := false
	for i := 0; i < len(command); {
		c := command[i]
		if !quoted && (c == ' ' || c == '\t') {
			break
		}
		if c == '\\' {
			start := i
			for i < len(command) && command[i] == '\\' {
				i++
			}
			count := i - start
			if i < len(command) && command[i] == '"' {
				word.WriteString(strings.Repeat("\\", count/2))
				if count%2 == 1 {
					word.WriteByte('"')
				} else {
					quoted = !quoted
				}
				i++
			} else {
				word.WriteString(strings.Repeat("\\", count))
			}
			continue
		}
		if c == '"' {
			quoted = !quoted
		} else {
			word.WriteByte(c)
		}
		i++
	}
	if quoted || word.Len() == 0 {
		return "", fmt.Errorf("invalid quoted hook executable")
	}
	return word.String(), nil
}
