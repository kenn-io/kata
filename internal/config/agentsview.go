package config

import (
	"errors"

	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/uid"
)

// AgentsViewConfig is operator-owned policy keyed by immutable project
// UID. Discovery advertises configured policy only and never resolves secrets.
type AgentsViewConfig struct {
	Project map[string]AgentsViewTarget `toml:"project"`
}

// AgentsViewTarget holds a project's advertised receiver and locator.
type AgentsViewTarget struct {
	Enabled     bool   `toml:"enabled"`
	ReceiverURL string `toml:"receiver_url"`
	LocatorURL  string `toml:"locator_url"`
}

// TranscriptServiceConfig is the legacy policy name.
// Deprecated: use AgentsViewConfig.
type TranscriptServiceConfig = AgentsViewConfig

// TranscriptServiceTarget is the legacy target name.
// Deprecated: use AgentsViewTarget.
type TranscriptServiceTarget = AgentsViewTarget

// ResolveAgentsViewConfig accepts the legacy section only when the canonical
// section is empty. Two populated sections are ambiguous and fail closed.
func ResolveAgentsViewConfig(canonical, legacy AgentsViewConfig) (AgentsViewConfig, error) {
	if len(canonical.Project) > 0 && len(legacy.Project) > 0 {
		return AgentsViewConfig{}, errors.New("configure either [agentsview] or legacy [transcript_service], not both")
	}
	if len(canonical.Project) == 0 {
		canonical = legacy
	}
	return NormalizeAgentsViewConfig(canonical)
}

// NormalizeAgentsViewConfig copies and validates project policy.
func NormalizeAgentsViewConfig(input AgentsViewConfig) (AgentsViewConfig, error) {
	output := AgentsViewConfig{Project: make(map[string]AgentsViewTarget, len(input.Project))}
	for project, target := range input.Project {
		if !uid.Valid(project) {
			return AgentsViewConfig{}, errors.New("agentsview project key must be an immutable project UID")
		}
		if target.Enabled {
			var err error
			target.ReceiverURL, err = httpurl.CanonicalHTTPSOrLoopbackBase(target.ReceiverURL)
			if err != nil {
				return AgentsViewConfig{}, err
			}
			target.LocatorURL, err = httpurl.CanonicalHTTPSOrLoopbackBase(target.LocatorURL)
			if err != nil {
				return AgentsViewConfig{}, err
			}
		}
		output.Project[project] = target
	}
	return output, nil
}
