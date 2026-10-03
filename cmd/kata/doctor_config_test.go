package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/diagnostics"
)

func doctorFinding(t *testing.T, r diagnostics.Report, id string) diagnostics.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("missing check %s: %+v", id, r)
	return diagnostics.Check{}
}

func TestDoctorLocalConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, file, content, id, status string }{
		{"missing daemon config", "", "", "config.daemon", "info"},
		{"invalid daemon config", "config.toml", "[auth]\ntoken = [secret-token\n", "config.daemon", "fail"},
		{"unknown daemon key", "config.toml", "litsen = '127.0.0.1:7777'", "config.daemon", "fail"},
		{"invalid display config", "config.toml", "[display]\nmarkdown_renderer=['']", "config.daemon", "fail"},
		{"unknown hook key", "hooks.toml", "[hooks]\npol_size=2", "config.hooks", "fail"},
		{"missing hooks", "", "", "config.hooks", "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, workspace := doctorTestEnv(t)
			if tc.file != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, tc.file), []byte(tc.content), 0600))
			}
			flags = globalFlags{Workspace: workspace}
			r := collectDoctor(context.Background())
			require.Equal(t, tc.status, doctorFinding(t, r, tc.id).Status)
			data, err := json.Marshal(r)
			require.NoError(t, err)
			require.NotContains(t, string(data), "secret-token")
		})
	}
}

func TestDoctorWorkspaceConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, binding, local, project, status string }{
		{"unbound", "", "", "", "warn"},
		{"explicit project", "", "", "example-project", "ok"},
		{"valid", "version=1\n[project]\nname='example-project'", "", "", "ok"},
		{"unknown ignored key", "version=1\n[project]\nname='example-project'\nnaem='other'", "", "", "warn"},
		{"local invalid", "version=1\n[project]\nname='example-project'", "version=2", "", "fail"},
		{"local unknown", "version=1\n[project]\nname='example-project'", "version=1\n[server]\nurs='https://daemon.example'", "", "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, workspace := doctorTestEnv(t)
			for file, data := range map[string]string{".kata.toml": tc.binding, ".kata.local.toml": tc.local} {
				if data != "" {
					require.NoError(t, os.WriteFile(filepath.Join(workspace, file), []byte(data), 0600))
				}
			}
			flags = globalFlags{Workspace: workspace, Project: tc.project}
			r := collectDoctor(context.Background())
			require.Equal(t, tc.status, doctorFinding(t, r, "config.workspace").Status)
		})
	}
}

func TestDoctorUsesLocalProjectOverrideUnlessExplicitlySelected(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"),
		[]byte("version=1\n[project]\nname='base-project'\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte("version=1\n[project]\nname='local-project'\n"), 0600))

	for _, tc := range []struct {
		name, explicitProject, wantProject string
	}{
		{name: "local override", wantProject: "local-project"},
		{name: "explicit selection", explicitProject: "explicit-project", wantProject: "explicit-project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags = globalFlags{Workspace: workspace, Project: tc.explicitProject}
			var state doctorState
			finding := state.workspaceConfig()
			require.Equal(t, "ok", finding.Status)
			require.Equal(t, tc.wantProject, state.project)
		})
	}
}

func TestDoctorInvalidWorkspaceDoesNotProbeAnotherTarget(t *testing.T) {
	_, workspace := doctorTestEnv(t)
	flags = globalFlags{Workspace: filepath.Join(workspace, "missing")}
	r := collectDoctor(context.Background())
	require.Equal(t, "fail", doctorFinding(t, r, "config.workspace").Status)
	require.Equal(t, "info", doctorFinding(t, r, "daemon.connection").Status)
}

func TestDoctorValidatesClosestLocalOverride(t *testing.T) {
	for _, tc := range []struct{ name, root, nested, status string }{
		{"invalid closest override", "version=1", "version=2", "fail"},
		{"unknown closest key", "version=1", "version=1\n[server]\nurs='https://daemon.example'", "warn"},
		{"shadowed invalid override", "version=2", "version=1", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, workspace := doctorTestEnv(t)
			nested := filepath.Join(workspace, "component")
			require.NoError(t, os.Mkdir(nested, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version=1\n[project]\nname='example-project'"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte(tc.root), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(nested, ".kata.local.toml"), []byte(tc.nested), 0600))
			flags = globalFlags{Workspace: nested}
			var state doctorState
			finding := state.workspaceConfig()
			require.Equal(t, tc.status, finding.Status)
			require.Equal(t, "example-project", state.project)
		})
	}
}
