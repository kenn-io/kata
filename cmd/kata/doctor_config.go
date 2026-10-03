package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kata/internal/hooks"
)

type doctorState struct {
	checks         []diagnostics.Check
	workspace      string
	project        string
	workspaceValid bool
}

func (s *doctorState) add(id, category string, fn func() diagnostics.Check) {
	s.checks = append(s.checks, diagnostics.Run(id, category, fn))
}

func (s *doctorState) localChecks() {
	s.add("config.daemon", "config", doctorDaemonConfig)
	s.add("config.workspace", "config", s.workspaceConfig)
	s.add("config.hooks", "config", doctorHookConfig)
}

func doctorDaemonConfig() diagnostics.Check {
	if _, err := config.ReadDaemonConfig(); err != nil {
		return diagnostics.Check{Status: "fail", Summary: "Local daemon configuration or environment overrides are invalid", Fix: "Correct <KATA_HOME>/config.toml using kata's configuration reference; check spelling, value types, URLs and environment overrides."}
	}
	if _, err := config.ReadDisplayConfig(); err != nil {
		return diagnostics.Check{Status: "fail", Summary: "Local display configuration is invalid", Fix: "Correct [display] in <KATA_HOME>/config.toml; markdown_renderer must begin with an executable."}
	}
	path, err := config.DaemonConfigPath()
	if err != nil {
		return diagnostics.Check{Status: "fail", Summary: "Cannot resolve local configuration path", Fix: "Set KATA_HOME to the intended local data directory."}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return diagnostics.Check{Status: "info", Summary: "No optional local daemon config file; defaults and environment overrides validated"}
	}
	return diagnostics.Check{Status: "ok", Summary: "Local daemon and display configuration are valid"}
}

func doctorHookConfig() diagnostics.Check {
	path, err := config.HookConfigPath()
	if err != nil {
		return diagnostics.Check{Status: "fail", Summary: "Cannot resolve local hooks configuration", Fix: "Check KATA_HOME."}
	}
	if _, err := hooks.LoadStartup(path); err != nil {
		return diagnostics.Check{Status: "fail", Summary: "Local hooks.toml is invalid", Fix: "Correct hook event names, commands, timeouts, working directories and tunable keys in <KATA_HOME>/hooks.toml."}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return diagnostics.Check{Status: "info", Summary: "No optional local hooks.toml"}
	}
	return diagnostics.Check{Status: "ok", Summary: "Local hooks configuration is valid; active daemon hooks are checked separately"}
}

func (s *doctorState) workspaceConfig() diagnostics.Check {
	s.project = strings.TrimSpace(flags.Project)
	explicitProject := s.project != ""
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return invalidDoctorWorkspace()
	}
	disc, err := config.DiscoverPaths(start)
	if err != nil {
		return invalidDoctorWorkspace()
	}
	s.workspace = start
	s.workspaceValid = true
	var unknown int
	if disc.WorkspaceRoot != "" {
		cfg, err := config.ReadProjectConfig(disc.WorkspaceRoot)
		if err != nil {
			return diagnostics.Check{Status: "fail", Summary: "Workspace .kata.toml is invalid", Fix: "Correct version = 1 and [project].name in .kata.toml."}
		}
		if s.project == "" {
			s.project = cfg.Project.Name
		}
		unknown += doctorUnknownProjectKeys(filepath.Join(disc.WorkspaceRoot, config.ProjectConfigFilename))
	}
	localPath, err := client.LocalConfigPathInWorkspace(start)
	if err != nil {
		return diagnostics.Check{Status: "fail", Summary: "Workspace local override could not be verified", Fix: "Ensure git is available and the workspace is trusted; keep .kata.local.toml untracked."}
	}
	if localPath != "" {
		localCfg, err := config.ReadLocalConfig(filepath.Dir(localPath))
		if err != nil && !errors.Is(err, config.ErrLocalConfigMissing) {
			return diagnostics.Check{Status: "fail", Summary: "Workspace .kata.local.toml is invalid", Fix: "Correct version = 1 and the local override fields; keep this file untracked."}
		}
		if !explicitProject && localCfg != nil && localCfg.Project.Name != "" {
			s.project = localCfg.Project.Name
		}
		unknown += doctorUnknownProjectKeys(localPath)
	}
	if unknown > 0 {
		return diagnostics.Check{Status: "warn", Summary: "Workspace configuration contains ignored keys", Details: []string{fmt.Sprintf("%d unrecognized key(s) in .kata.toml or .kata.local.toml", unknown)}, Fix: "Check key spelling against the configuration reference; ignored fields do not change Kata's behavior."}
	}
	if s.project == "" {
		return diagnostics.Check{Status: "warn", Summary: "No project binding or explicit project; project lookup will be skipped", Fix: "Use --project <name> or initialize the intended workspace with kata init."}
	}
	return diagnostics.Check{Status: "ok", Summary: "Workspace configuration is valid and supplies a project name"}
}

func invalidDoctorWorkspace() diagnostics.Check {
	return diagnostics.Check{Status: "fail", Summary: "Workspace path cannot be resolved", Fix: "Pass --workspace with an existing accessible path."}
}

// Only key counts are reported: parser errors and arbitrary key names may contain secrets.
func doctorUnknownProjectKeys(path string) int {
	var cfg config.ProjectConfig
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return 0
	}
	return len(md.Undecoded())
}
