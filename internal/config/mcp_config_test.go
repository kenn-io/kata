package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPConfigValidation(t *testing.T) {
	for _, cfg := range []MCPConfig{{}, {Enabled: true, Actor: "example-agent", Projects: []string{"hub-project", "équipe 👋"}}} {
		require.NoError(t, cfg.Validate())
	}
	for _, cfg := range []MCPConfig{
		{Actor: " actor "}, {Actor: "actor\nname"}, {Actor: "\xff"}, {Projects: []string{"\xff"}},
		{Projects: []string{""}}, {Projects: []string{" project "}},
		{Projects: []string{"a\nb"}}, {Projects: []string{"hub-project", "hub-project"}},
	} {
		require.Error(t, cfg.Validate(), "%+v", cfg)
	}
}

func FuzzMCPConfigRejectsDuplicateProjects(f *testing.F) {
	f.Add("hub-project")
	f.Add("")
	f.Add("a\x00b")
	f.Fuzz(func(t *testing.T, name string) {
		// Duplicate names must never turn a configured allowlist into all-projects.
		require.Error(t, (MCPConfig{Projects: []string{name, name}}).Validate())
	})
}
