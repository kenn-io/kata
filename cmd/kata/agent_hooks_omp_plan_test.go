package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func fakeOmpPlan(t *testing.T, home, config, plugins string) *[][]string {
	t.Helper()
	previous := runOmp
	var calls [][]string
	runOmp = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		require.Equal(t, home, dir)
		calls = append(calls, append([]string(nil), args...))
		switch strings.Join(args, " ") {
		case "config path":
			return []byte(home + "\n"), nil
		case "config get todo.enabled --json":
			return []byte(config), nil
		case "plugin list --json":
			return []byte(plugins), nil
		default:
			return nil, fmt.Errorf("unexpected omp command: %v", args)
		}
	}
	t.Cleanup(func() { runOmp = previous })
	return &calls
}

func ompPlanCLI(t *testing.T, verb string, extra ...string) string {
	t.Helper()
	resetFlags(t)
	args := append([]string{"agent-hook", verb, "omp", "--json"}, extra...)
	if verb == "install" {
		args = append(args, "--executable", os.Args[0])
	}
	out, _, err := executeAgentHook(t, strings.NewReader(""), args...)
	require.NoError(t, err)
	return out
}

type ompPropertyT struct{ *hegel.T }

func (p ompPropertyT) FailNow() { p.Fatal("assertion failed") }

func TestOmpPlanFlagValidation(t *testing.T) {
	for _, verb := range []string{"install", "uninstall"} {
		for _, tc := range []struct {
			args []string
			want string
		}{
			{nil, "--plan requires exactly one explicit omp target"},
			{[]string{"--all"}, "--plan requires exactly one explicit omp target"},
			{[]string{"omp", "pi"}, "--plan requires exactly one explicit omp target"},
			{[]string{"omm"}, `unknown harness "omm"; choose claude, codex, gemini, copilot, cursor, qwen, hermes, droid, antigravity, amp, opencode, omp, pi, openclaw, kimi-code, kimi, muse, grok, zcode`},
			{[]string{"pi"}, "--plan currently supports only omp"},
			{[]string{"pi", "--plan=false"}, "--plan currently supports only omp"},
			{[]string{"omp", "--scope", "project"}, "--plan requires --scope user"},
			{[]string{"omp", "--local"}, "--plan requires --scope user"},
		} {
			t.Run(verb+strings.Join(tc.args, " "), func(t *testing.T) {
				resetFlags(t)
				_, _, err := executeAgentHook(t, strings.NewReader(""), append([]string{"agent-hook", verb, "--plan"}, tc.args...)...)
				require.EqualError(t, err, tc.want)
			})
		}
	}
}

func TestOmpPlanRetainedOnReinstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[]}`)
	ompPlanCLI(t, "install", "--plan")
	path := filepath.Join(home, "extensions", "kata.js")
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
	require.NoError(t, err)
	ompPlanCLI(t, "install")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", "omp", "--json")
	require.NoError(t, err)
	var report agentHookStatusReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.True(t, report.Harnesses[0].Configured.Plan)
	ompPlanCLI(t, "install", "--plan=false")
	ompPlanCLI(t, "uninstall", "--plan=false")
	after, err = os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestOmpPlanFalseUninstallState(t *testing.T) {
	for _, installed := range []string{"absent", "base", "plan"} {
		for _, format := range []string{"json", "human"} {
			t.Run(installed+"/"+format, func(t *testing.T) {
				home := isolateAgentHookHomes(t)
				fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[]}`)
				path := filepath.Join(home, "extensions", "kata.js")
				var before []byte
				want := "absent"
				if installed != "absent" {
					selection := "--plan=false"
					if installed == "plan" {
						selection = "--plan"
					}
					ompPlanCLI(t, "install", selection)
					var err error
					before, err = os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
					require.NoError(t, err)
					meta, err := parseOmpAgentHookMetadata(before)
					require.NoError(t, err)
					require.Equal(t, installed == "plan", meta.Plan)
					want = "unchanged"
				}
				resetFlags(t)
				args := []string{"agent-hook", "uninstall", "omp", "--plan=false"}
				if format == "json" {
					args = append(args, "--json")
				}
				out, _, err := executeAgentHook(t, strings.NewReader(""), args...)
				require.NoError(t, err)
				if format == "json" {
					var report struct {
						Action  string              `json:"action"`
						Results []agentHookMutation `json:"results"`
					}
					require.NoError(t, json.Unmarshal([]byte(out), &report))
					require.Equal(t, "uninstall", report.Action)
					require.Len(t, report.Results, 1)
					require.Equal(t, "omp", report.Results[0].Harness)
					require.Equal(t, path, report.Results[0].ConfigPath)
					require.Equal(t, want, report.Results[0].State)
					require.False(t, report.Results[0].Changed)
				} else {
					require.Equal(t, "omp: "+want+" — "+path, strings.Split(out, "\n")[0])
				}
				after, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
				if installed == "absent" {
					require.ErrorIs(t, err, os.ErrNotExist)
				} else {
					require.NoError(t, err)
					require.Equal(t, before, after)
				}
			})
		}
	}
}

func TestOmpPlanAliasAndContractSelection(t *testing.T) {
	home := isolateAgentHookHomes(t)
	fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[]}`)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "oh-my-pi", "--plan", "--attention=false", "--executable", os.Args[0])
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(home, "extensions", "kata.js")) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
	require.NoError(t, err)
	meta, err := parseOmpAgentHookMetadata(data)
	require.NoError(t, err)
	require.True(t, meta.Plan)
	require.True(t, meta.Contract)
	require.False(t, meta.Attention)
}

func TestOmpUninstallPlanOnly(t *testing.T) {
	home := isolateAgentHookHomes(t)
	fakeOmpPlan(t, home, `{"value":false}`, `{"marketplace":[]}`)
	ompPlanCLI(t, "install", "--plan")
	ompPlanCLI(t, "uninstall", "--plan")
	path := filepath.Join(home, "extensions", "kata.js")
	data, err := os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
	require.NoError(t, err)
	meta, err := parseOmpAgentHookMetadata(data)
	require.NoError(t, err)
	require.False(t, meta.Plan)
	require.True(t, meta.Contract)
	require.True(t, meta.Attention)
	ompPlanCLI(t, "install", "--plan")
	ompPlanCLI(t, "uninstall", "--contract-only")
	data, err = os.ReadFile(path) //nolint:gosec // G304: isolated hook fixture under t.TempDir.
	require.NoError(t, err)
	meta, err = parseOmpAgentHookMetadata(data)
	require.NoError(t, err)
	require.True(t, meta.Plan)
	require.False(t, meta.Contract)
	require.True(t, meta.Attention)
	ompPlanCLI(t, "uninstall")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// Frozen PR 2 reader: deliberately does not know Plan.
type ompPR2Metadata struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	Scope      string `json:"scope"`
	Workspace  string `json:"workspace"`
	Executable string `json:"executable"`
	Source     string `json:"source"`
	SourceSet  bool   `json:"sourceSet"`
	Contract   bool   `json:"contract"`
	Attention  bool   `json:"attention"`
}

func TestOmpMetadataOldReaderCompatProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		meta := drawOmpMetadata(ht)
		data, err := generateOmpAgentHooks(meta)
		require.NoError(ompPropertyT{ht}, err)
		first, _, _ := bytes.Cut(data, []byte("\n"))
		var old ompPR2Metadata
		err = json.Unmarshal(bytes.TrimPrefix(first, []byte(ompAgentHookMetadataPrefix)), &old, json.RejectUnknownMembers(true))
		if meta.Plan {
			require.Error(ompPropertyT{ht}, err)
		} else {
			require.NoError(ompPropertyT{ht}, err)
			require.Equal(ompPropertyT{ht}, ompPR2Metadata{meta.Format, meta.Version, meta.Scope, meta.Workspace, meta.Executable, meta.Source, meta.SourceSet, meta.Contract, meta.Attention}, old)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestOmpPlanUnionIdempotenceProperty(t *testing.T) {
	opts := ompTestOptions(t)
	opts.Scope = "user"
	fakeOmp(t, opts.Home+"\n", nil)
	hegel.Test(t, func(ht *hegel.T) {
		a, b := drawOmpMetadata(ht), drawOmpMetadata(ht)
		a.Scope, a.Workspace = "user", ""
		data, err := generateOmpAgentHooks(a)
		require.NoError(ompPropertyT{ht}, err)
		path := filepath.Join(opts.Home, "extensions", "kata.js")
		require.NoError(ompPropertyT{ht}, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(ompPropertyT{ht}, os.WriteFile(path, data, 0600))
		opts.Contract, opts.Attention, opts.Plan = b.Contract, b.Attention, b.Plan
		opts.Executable = ""
		plan, err := planOmpAgentHooks(opts, false)
		require.NoError(ompPropertyT{ht}, err)
		require.Equal(ompPropertyT{ht}, a.Plan || opts.Plan, plan.Plan)
		require.Equal(ompPropertyT{ht}, a.Contract || b.Contract || opts.Plan, plan.Contract)
		require.Equal(ompPropertyT{ht}, a.Attention || b.Attention, plan.AttentionStart)
		require.Equal(ompPropertyT{ht}, a.Plan, plan.CurrentPlan)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(ompPropertyT{ht}, err)
		plan, err = planOmpAgentHooks(opts, false)
		require.NoError(ompPropertyT{ht}, err)
		changed, err := publishNativeAgentHookPlan(plan)
		require.NoError(ompPropertyT{ht}, err)
		require.False(ompPropertyT{ht}, changed)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}
