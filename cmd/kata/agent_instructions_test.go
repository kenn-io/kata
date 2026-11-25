package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"gopkg.in/yaml.v3"
)

// Clean uninstall must preserve foreign files and their modes even if a later
// artifact write fails. Reproduce a partial uninstall with an injected I/O error.
func TestAgentInstructionsRollbackPreservesDeletedPreimage(t *testing.T) {
	home := t.TempDir()
	artifacts, err := planInstructionArtifacts(home, "", "example-agent", "install")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(artifacts))
	path := artifacts[0].Path
	require.NoError(t, os.Chmod(path, 0o640)) //nolint:gosec // G302: exercise preservation of an existing fixture mode.
	before, err := os.ReadFile(path)          //nolint:gosec // G304: artifact path under TempDir.
	require.NoError(t, err)
	artifacts, err = planInstructionArtifacts(home, "", "", "uninstall")
	require.NoError(t, err)
	failed := false
	err = applyInstructionArtifactsWithWriter(artifacts, func(path, content string, exists bool, mode os.FileMode) error {
		if !failed && strings.HasSuffix(path, "SKILL.md") {
			failed = true
			return errors.New("injected write failure")
		}
		return writeInstructionArtifact(path, content, exists, mode)
	})
	require.ErrorContains(t, err, "injected write failure")
	after, err := os.ReadFile(path) //nolint:gosec // G304: artifact path under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	}
}

func TestAgentInstructionsSnapshotAndPathProtection(t *testing.T) {
	home := t.TempDir()
	artifacts, err := planInstructionArtifacts(home, "", "example-agent", "install")
	require.NoError(t, err)
	path := artifacts[0].Path
	require.NoError(t, os.WriteFile(path, []byte("Concurrent edit"), 0o600))
	require.ErrorContains(t, applyInstructionArtifacts(artifacts), "changed during planning")
	data, err := os.ReadFile(path) //nolint:gosec // G304: artifact path under TempDir.
	require.NoError(t, err)
	require.Equal(t, "Concurrent edit", string(data))
	_, err = os.Stat(artifacts[1].Path)
	require.ErrorIs(t, err, os.ErrNotExist)
	// A symlinked home must not redirect a bundle into another directory.
	alias := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(home, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, err = planInstructionArtifacts(alias, "", "example-agent", "install")
	require.ErrorContains(t, err, "real directories")
}

// macOS's /var and /tmp aliases are outside the harness-owned tree. Reproduce
// that layout without requiring a macOS runner; managed aliases remain refused.
func TestAgentInstructionsAllowsParentAlias(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	home := filepath.Join(physical, "muse-home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	alias := filepath.Join(root, "parent-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	artifacts, err := planInstructionArtifacts(filepath.Join(alias, "muse-home"), "", "example-agent", "install")
	require.NoError(t, err)
	resolvedHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedHome, "AGENTS.md"), artifacts[0].Path)
	require.NoError(t, applyInstructionArtifacts(artifacts))
	remove, err := planInstructionArtifacts(filepath.Join(alias, "muse-home"), "", "", "uninstall")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(remove))
}

func TestAgentInstructionsPublishedFailureRollsBack(t *testing.T) {
	home := t.TempDir()
	artifacts, err := planInstructionArtifacts(home, "", "example-agent", "install")
	require.NoError(t, err)
	failed := false
	err = applyInstructionArtifactsWithWriter(artifacts, func(path, content string, exists bool, mode os.FileMode) error {
		err := writeInstructionArtifact(path, content, exists, mode)
		if !failed && strings.HasSuffix(path, "SKILL.md") && err == nil {
			failed = true
			return fmt.Errorf("injected post-publication failure: %w", atomicfile.ErrPublished)
		}
		return err
	})
	require.Error(t, err)
	for _, a := range artifacts {
		_, err := os.Stat(a.Path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

// A failed install must restore even empty files that existed beforehand.
func TestAgentInstructionsRollbackRestoresExistingEmptyFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	artifacts, err := planInstructionArtifacts(home, "", "example-agent", "install")
	require.NoError(t, err)
	failed := false
	err = applyInstructionArtifactsWithWriter(artifacts, func(path, content string, keepEmpty bool, mode os.FileMode) error {
		if !failed && strings.HasSuffix(path, "SKILL.md") {
			failed = true
			return errors.New("injected write failure")
		}
		return writeInstructionArtifact(path, content, keepEmpty, mode)
	})
	require.ErrorContains(t, err, "injected write failure")
	data, err := os.ReadFile(path) //nolint:gosec // G304: artifact path under TempDir.
	require.NoError(t, err)
	require.Empty(t, data)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestAgentInstructionsForeignSuffixesAndActorUpdate(t *testing.T) {
	home := t.TempDir()
	artifacts, err := planInstructionArtifacts(home, "", "example-agent", "install")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(artifacts))
	for _, a := range artifacts {
		file, err := os.OpenFile(a.Path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = file.WriteString("Foreign suffix\r\n")
		require.NoError(t, err)
		require.NoError(t, file.Close())
	}
	updated, err := planInstructionArtifacts(home, "", "example-agent/worker", "install")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(updated))
	for _, a := range updated {
		require.Contains(t, a.Content, "kata inbox --for 'example-agent/worker' --json")
		require.Equal(t, 1, strings.Count(a.Content, "BEGIN KATA HOOKLESS MUSE"))
		require.True(t, strings.HasSuffix(a.Content, "Foreign suffix\r\n"))
	}
	uninstall, err := planInstructionArtifacts(home, "", "", "uninstall")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(uninstall))
	for _, a := range uninstall {
		content, err := os.ReadFile(a.Path)
		require.NoError(t, err)
		require.Equal(t, "Foreign suffix\r\n", string(content))
	}
}

// ne8p's explicit contract: one shared briefing in all three artifacts, a
// write-free dry-run, stable reinstallation and lossless removal.
func TestAgentInstructionsMuseLifecycle(t *testing.T) {
	resetFlags(t)
	t.Chdir(t.TempDir())
	home := filepath.Join(t.TempDir(), "home", "muse-home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	home, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	instructions := filepath.Join(home, "AGENTS.md")
	foreign := []byte("# Existing instructions\r\nKeep this exact text without a final newline.")
	require.NoError(t, os.WriteFile(instructions, foreign, 0o640)) //nolint:gosec // G306: exercise preservation of an existing fixture mode.
	t.Setenv("KATA_AUTH_TOKEN", "secret-must-not-appear")
	args := []string{"agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent", "--json"}
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, append(args, "--dry")...)
	require.NoError(t, err, stderr)
	var report struct {
		NativeAttention bool `json:"native_attention"`
		DryRun          bool `json:"dry_run"`
		Artifacts       []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Changed bool   `json:"changed"`
			State   string `json:"state"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.False(t, report.NativeAttention)
	require.True(t, report.DryRun)
	require.Len(t, report.Artifacts, 3)
	require.NotContains(t, out, "secret-must-not-appear")
	for _, a := range report.Artifacts {
		require.Contains(t, a.Content, agentContractText)
		require.True(t, a.Changed)
	}
	before, err := os.ReadFile(instructions) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, foreign, before)
	_, err = os.Stat(filepath.Join(home, "workspace"))
	require.ErrorIs(t, err, os.ErrNotExist)
	var skill struct{ Name, Description string }
	skillText := report.Artifacts[1].Content
	require.True(t, strings.HasPrefix(skillText, "---\n"))
	require.NoError(t, yaml.Unmarshal([]byte(strings.Split(skillText, "---")[1]), &skill))
	require.Equal(t, "kata", skill.Name)
	require.Contains(t, skill.Description, "conversation start")
	require.Contains(t, skill.Description, "completion")
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, args...)
	require.NoError(t, err, stderr)
	installed := map[string][]byte{}
	for _, a := range report.Artifacts {
		installed[a.Path], err = os.ReadFile(a.Path)
		require.NoError(t, err)
		require.Equal(t, a.Content, string(installed[a.Path]))
	}
	info, err := os.Stat(instructions)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	}
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, args...)
	require.NoError(t, err, stderr)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, a := range report.Artifacts {
		require.False(t, a.Changed)
		data, err := os.ReadFile(a.Path)
		require.NoError(t, err)
		require.Equal(t, installed[a.Path], data)
	}
	// Foreign suffixes added after install must also survive uninstall.
	require.NoError(t, os.WriteFile(instructions, append(installed[instructions], []byte("Foreign suffix\n")...), 0o640)) //nolint:gosec // G306: retain the existing fixture mode.
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "status", "muse", "--home", home, "--json")
	require.NoError(t, err, stderr)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, a := range report.Artifacts {
		require.Equal(t, "installed", a.State)
	}
	for range 2 {
		_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home)
		require.NoError(t, err, stderr)
	}
	data, err := os.ReadFile(instructions) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, string(foreign)+"Foreign suffix\n", string(data))
	for _, a := range report.Artifacts[1:] {
		_, err := os.Stat(a.Path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestMusePollSpecDocumentsClientTokenFileOptions(t *testing.T) {
	poll := musePollSpec("example-agent")
	require.Contains(t, poll, "[auth].token_file")
	require.Contains(t, poll, "KATA_AUTH_TOKEN_FILE")
	require.NotContains(t, poll, "There is no client auth.token_file setting")
}

func TestAgentInstructionsUninstallDryPreviewDoesNotWrite(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	installOut, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent", "--json")
	require.NoError(t, err, stderr)

	var installed struct {
		Artifacts []struct {
			Path string `json:"path"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(installOut), &installed))
	require.Len(t, installed.Artifacts, 3)
	paths := make([]string, 0, len(installed.Artifacts))
	before := make(map[string][]byte, len(installed.Artifacts))
	for _, artifact := range installed.Artifacts {
		path := artifact.Path
		paths = append(paths, path)
		before[path], err = os.ReadFile(path) //nolint:gosec // G304: artifact path under TempDir.
		require.NoError(t, err)
	}

	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home, "--dry", "--json")
	require.NoError(t, err, stderr)
	var report struct {
		DryRun    bool `json:"dry_run"`
		Artifacts []struct {
			Path    string  `json:"path"`
			State   string  `json:"state"`
			Changed bool    `json:"changed"`
			Content *string `json:"content"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.True(t, report.DryRun)
	require.Len(t, report.Artifacts, len(paths))
	for _, artifact := range report.Artifacts {
		require.Equal(t, "removed", artifact.State)
		require.True(t, artifact.Changed)
		require.NotNil(t, artifact.Content)
		require.Empty(t, *artifact.Content)
		after, err := os.ReadFile(artifact.Path) //nolint:gosec // G304: artifact path under TempDir.
		require.NoError(t, err)
		require.Equal(t, before[artifact.Path], after)
	}
}

func TestAgentInstructionsJSONOmitsContentsOutsideDryRuns(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	privateText := "private-instruction-fixture-7d2c"
	require.NoError(t, os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(privateText), 0o600)) //nolint:gosec // G306: private-data fixture under TempDir.

	assertNoArtifactContents := func(out string) {
		t.Helper()
		require.NotContains(t, out, privateText)
		var response struct {
			Artifacts []map[string]json.RawMessage `json:"artifacts"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &response))
		require.Len(t, response.Artifacts, 3)
		for _, artifact := range response.Artifacts {
			_, hasContent := artifact["content"]
			require.False(t, hasContent)
		}
	}

	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent", "--json")
	require.NoError(t, err, stderr)
	assertNoArtifactContents(out)

	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "status", "muse", "--home", home, "--json")
	require.NoError(t, err, stderr)
	assertNoArtifactContents(out)

	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home, "--json")
	require.NoError(t, err, stderr)
	assertNoArtifactContents(out)

	data, err := os.ReadFile(filepath.Join(home, "AGENTS.md")) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, privateText, string(data))
}

func TestAgentInstructionsRejectsDryRunAlias(t *testing.T) {
	for _, verb := range []string{"install", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			resetFlags(t)
			home := t.TempDir()
			if verb == "uninstall" {
				_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent")
				require.NoError(t, err, stderr)
			}

			args := []string{"agent-hook", "instructions", verb, "muse", "--home", home}
			if verb == "install" {
				args = append(args, "--actor", "example-agent")
			}
			_, stderr, err := executeAgentHook(t, unreadableHookInput{}, append(args, "--dry-run")...)
			require.Error(t, err)
			require.Contains(t, stderr, "unknown flag: --dry-run")
			require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
		})
	}
}

func TestAgentInstructionsRefusesForeignAndMalformedBeforeWriting(t *testing.T) {
	resetFlags(t)
	for _, content := range []string{"Unowned skill\n", "---\n# BEGIN KATA HOOKLESS MUSE\nunfinished"} {
		t.Run(content, func(t *testing.T) {
			home := t.TempDir()
			skill := filepath.Join(home, "workspace", "custom-skills", "kata", "SKILL.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(skill), 0o700))
			require.NoError(t, os.WriteFile(skill, []byte(content), 0o600))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent")
			require.Error(t, err)
			require.Empty(t, out)
			_, err = os.Stat(filepath.Join(home, "AGENTS.md"))
			require.ErrorIs(t, err, os.ErrNotExist)
			data, err := os.ReadFile(skill) //nolint:gosec // G304: fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, content, string(data))
		})
	}
}

func TestAgentInstructionsUsageAndQuickstartHelp(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	for _, tail := range [][]string{
		{"install", "muse", "--home", home},
		{"install", "muse", "--home", home, "--actor", "bad\nactor"},
		{"install", "muse-code", "--home", home, "--actor", "example-agent"},
		{"install", "muse", "--actor", "example-agent", "--home="},
		{"install", "muse", "--actor", "example-agent"},
		{"uninstall", "muse"},
		{"status", "muse"},
	} {
		out, _, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hook", "instructions"}, tail...)...)
		require.Error(t, err)
		require.Empty(t, out)
		require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
	}
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "quickstart", "--help")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "agent-hook instructions install muse")
	require.Contains(t, out, "instruction-following")
	// Setup must never change the native briefing bytes.
	out, stderr, err = executeAgentHook(t, unreadableHookInput{}, "quickstart", "--format", "contract")
	require.NoError(t, err, stderr)
	require.Equal(t, agentContractText, out)
}

// Editors and checkouts may strip the final newline or convert line endings.
// Uninstall must still find and remove the managed blocks.
func TestAgentInstructionsUninstallAfterLineEndingEdits(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(string) string
		want string
	}{
		{"final newline stripped", func(s string) string { return strings.TrimSuffix(s, "\n") }, "# Existing\n"},
		{"CRLF", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }, "# Existing\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			home := t.TempDir()
			instructions := filepath.Join(home, "AGENTS.md")
			require.NoError(t, os.WriteFile(instructions, []byte("# Existing\n"), 0o600))
			_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent")
			require.NoError(t, err, stderr)
			skill := filepath.Join(home, "workspace", "custom-skills", "kata", "SKILL.md")
			poll := filepath.Join(home, "workspace", "custom-skills", "kata", "POLL.md")
			for _, path := range []string{instructions, skill, poll} {
				data, err := os.ReadFile(path) //nolint:gosec // G304: artifact path under TempDir.
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(tc.edit(string(data))), 0o600))
			}

			_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home)
			require.NoError(t, err, stderr)
			data, err := os.ReadFile(instructions) //nolint:gosec // G304: fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, tc.want, string(data))
			for _, path := range []string{skill, poll} {
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

// Removing the blank separator must not make uninstall join foreign lines.
func TestAgentInstructionsUninstallAfterBlankLineRemoved(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	path := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("# Existing\n"), 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent")
	require.NoError(t, err, stderr)
	data, err := os.ReadFile(path) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	edited := strings.Replace(string(data), "# Existing\n\n", "# Existing\n", 1) + "# Additional instructions\n"
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o600)) //nolint:gosec // G703: path is the test-owned AGENTS.md under TempDir.
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home)
	require.NoError(t, err, stderr)
	data, err = os.ReadFile(path) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, "# Existing\n# Additional instructions\n", string(data))
}

// Valid actors may contain marker phrases; only complete marker lines count.
func TestAgentInstructionsActorContainingMarkerPhrase(t *testing.T) {
	for _, actor := range []string{"x END KATA HOOKLESS MUSE", "<!-- END KATA HOOKLESS MUSE -->"} {
		t.Run(actor, func(t *testing.T) {
			resetFlags(t)
			home := t.TempDir()
			_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", actor)
			require.NoError(t, err, stderr)
			out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "status", "muse", "--home", home, "--json")
			require.NoError(t, err, stderr)
			var report struct {
				Artifacts []struct {
					State string `json:"state"`
				} `json:"artifacts"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			require.Len(t, report.Artifacts, 3)
			for _, a := range report.Artifacts {
				require.Equal(t, "installed", a.State)
			}
			_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home)
			require.NoError(t, err, stderr)
			_, err = os.Stat(filepath.Join(home, "workspace", "custom-skills", "kata", "SKILL.md"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// Uninstall keeps AGENTS.md even when only the managed block remained, so a
// file that existed empty before installation is not deleted.
func TestAgentInstructionsUninstallKeepsEmptyAgentsFile(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	instructions := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(instructions, nil, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "install", "muse", "--home", home, "--actor", "example-agent")
	require.NoError(t, err, stderr)
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "uninstall", "muse", "--home", home)
	require.NoError(t, err, stderr)
	data, err := os.ReadFile(instructions) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Empty(t, data)
}

type instructionsReport struct {
	Artifacts []struct {
		Path    string `json:"path"`
		Changed bool   `json:"changed"`
		State   string `json:"state"`
	} `json:"artifacts"`
}

func runInstructions(t *testing.T, args ...string) instructionsReport {
	t.Helper()
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, append(append([]string{"agent-hook", "instructions"}, args...), "--json")...)
	require.NoError(t, err, stderr)
	var report instructionsReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	return report
}

func artifactStates(report instructionsReport, home string) map[string]string {
	states := map[string]string{}
	for _, a := range report.Artifacts {
		rel, _ := filepath.Rel(home, a.Path)
		states[filepath.ToSlash(rel)] = a.State
	}
	return states
}

func realTempHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return home
}

// kenn-io/kata#1315: consumer Muse discovers custom skills in
// workspace/custom-skills/<name>/, where SKILL.md alone is enough.
func TestAgentInstructionsDefaultsToCustomSkills(t *testing.T) {
	resetFlags(t)
	home := realTempHome(t)
	report := runInstructions(t, "install", "muse", "--home", home, "--actor", "example-agent")
	require.Equal(t, map[string]string{
		"AGENTS.md":                             "installed",
		"workspace/custom-skills/kata/SKILL.md": "installed",
		"workspace/custom-skills/kata/POLL.md":  "installed",
	}, artifactStates(report, home))
	_, err := os.Stat(filepath.Join(home, "workspace", "skills"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// --skills-dir selects another skills directory inside home for every verb.
func TestAgentInstructionsSkillsDirOverride(t *testing.T) {
	resetFlags(t)
	home := realTempHome(t)
	for _, dir := range []string{"workspace/skills", filepath.Join(home, "skills")} {
		rel, err := filepath.Rel(home, filepath.Join(home, strings.TrimPrefix(dir, home)))
		require.NoError(t, err)
		want := map[string]string{
			"AGENTS.md": "installed",
			filepath.ToSlash(filepath.Join(rel, "kata", "SKILL.md")): "installed",
			filepath.ToSlash(filepath.Join(rel, "kata", "POLL.md")):  "installed",
		}
		require.Equal(t, want, artifactStates(runInstructions(t, "install", "muse", "--home", home, "--actor", "example-agent", "--skills-dir", dir), home))
		require.Equal(t, want, artifactStates(runInstructions(t, "status", "muse", "--home", home, "--skills-dir", dir), home))
		for path := range want {
			want[path] = "removed"
		}
		require.Equal(t, want, artifactStates(runInstructions(t, "uninstall", "muse", "--home", home, "--skills-dir", dir), home))
	}
}

// Every managed file stays inside the harness home.
func TestAgentInstructionsSkillsDirMustStayInsideHome(t *testing.T) {
	home := realTempHome(t)
	for _, dir := range []string{"../outside", filepath.Join(filepath.Dir(home), "outside")} {
		t.Run(dir, func(t *testing.T) {
			resetFlags(t)
			for _, verb := range []string{"install", "status", "uninstall"} {
				args := []string{"agent-hook", "instructions", verb, "muse", "--home", home, "--skills-dir", dir}
				if verb == "install" {
					args = append(args, "--actor", "example-agent")
				}
				out, _, err := executeAgentHook(t, unreadableHookInput{}, args...)
				require.Error(t, err)
				require.Empty(t, out)
				require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
			}
			_, err := os.Stat(filepath.Join(home, "AGENTS.md"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// installLegacyBundle reproduces an install made before the custom-skills
// default, with foreign text added to its POLL.md.
func installLegacyBundle(t *testing.T, home string) (skill, poll string) {
	t.Helper()
	runInstructions(t, "install", "muse", "--home", home, "--actor", "example-agent", "--skills-dir", "workspace/skills")
	skill = filepath.Join(home, "workspace", "skills", "kata", "SKILL.md")
	poll = filepath.Join(home, "workspace", "skills", "kata", "POLL.md")
	data, err := os.ReadFile(poll) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(poll, append(data, "Foreign poll notes\n"...), 0o600)) //nolint:gosec // G703: poll is the test-owned legacy POLL.md under TempDir.
	return skill, poll
}

// Without --skills-dir, an older workspace/skills/kata copy is reported,
// moved on install, and removed on uninstall, so two copies cannot drift.
func TestAgentInstructionsMigratesLegacySkills(t *testing.T) {
	resetFlags(t)
	home := realTempHome(t)
	skill, poll := installLegacyBundle(t, home)
	require.Equal(t, map[string]string{
		"AGENTS.md":                             "installed",
		"workspace/custom-skills/kata/SKILL.md": "absent",
		"workspace/custom-skills/kata/POLL.md":  "absent",
		"workspace/skills/kata/SKILL.md":        "installed",
		"workspace/skills/kata/POLL.md":         "installed",
	}, artifactStates(runInstructions(t, "status", "muse", "--home", home), home))

	require.Equal(t, map[string]string{
		"AGENTS.md":                             "installed",
		"workspace/custom-skills/kata/SKILL.md": "installed",
		"workspace/custom-skills/kata/POLL.md":  "installed",
		"workspace/skills/kata/SKILL.md":        "removed",
		"workspace/skills/kata/POLL.md":         "removed",
	}, artifactStates(runInstructions(t, "install", "muse", "--home", home, "--actor", "example-agent"), home))
	_, err := os.Stat(skill)
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(poll) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, "Foreign poll notes\n", string(data))
	require.Len(t, runInstructions(t, "status", "muse", "--home", home).Artifacts, 3)
}

func TestAgentInstructionsUninstallRemovesLegacySkills(t *testing.T) {
	resetFlags(t)
	home := realTempHome(t)
	skill, poll := installLegacyBundle(t, home)
	require.Equal(t, map[string]string{
		"AGENTS.md":                             "removed",
		"workspace/custom-skills/kata/SKILL.md": "absent",
		"workspace/custom-skills/kata/POLL.md":  "absent",
		"workspace/skills/kata/SKILL.md":        "removed",
		"workspace/skills/kata/POLL.md":         "removed",
	}, artifactStates(runInstructions(t, "uninstall", "muse", "--home", home), home))
	_, err := os.Stat(skill)
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(poll) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, "Foreign poll notes\n", string(data))
}

// A failure while removing the legacy copy restores both locations.
func TestAgentInstructionsLegacyMigrationRollsBack(t *testing.T) {
	resetFlags(t)
	home := realTempHome(t)
	skill, poll := installLegacyBundle(t, home)
	before := map[string][]byte{}
	for _, path := range []string{filepath.Join(home, "AGENTS.md"), skill, poll} {
		data, err := os.ReadFile(path) //nolint:gosec // G304: fixture under TempDir.
		require.NoError(t, err)
		before[path] = data
	}
	artifacts, err := planInstructionArtifacts(home, "", "example-agent", "install")
	require.NoError(t, err)
	err = applyInstructionArtifactsWithWriter(artifacts, func(path, content string, keepEmpty bool, mode os.FileMode) error {
		if path == poll && !strings.Contains(content, "BEGIN KATA HOOKLESS MUSE") {
			return errors.New("injected legacy removal failure")
		}
		return writeInstructionArtifact(path, content, keepEmpty, mode)
	})
	require.ErrorContains(t, err, "injected legacy removal failure")
	for path, want := range before {
		data, err := os.ReadFile(path) //nolint:gosec // G304: fixture under TempDir.
		require.NoError(t, err)
		require.Equal(t, string(want), string(data))
	}
	for _, name := range []string{"SKILL.md", "POLL.md"} {
		_, err := os.Stat(filepath.Join(home, "workspace", "custom-skills", "kata", name))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

// Kata never wrote through symlinks, so a symlinked legacy directory holds no
// managed copy and must not block the default location.
func TestAgentInstructionsIgnoresSymlinkedLegacySkills(t *testing.T) {
	resetFlags(t)
	home := realTempHome(t)
	shared := filepath.Join(t.TempDir(), "shared-skills")
	require.NoError(t, os.MkdirAll(filepath.Join(shared, "kata"), 0o700))
	foreign := filepath.Join(shared, "kata", "SKILL.md")
	require.NoError(t, os.WriteFile(foreign, []byte("Shared skill\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "workspace"), 0o700))
	if err := os.Symlink(shared, filepath.Join(home, "workspace", "skills")); err != nil {
		t.Skip("symlinks unavailable")
	}
	for _, verb := range []string{"status", "install", "uninstall"} {
		args := []string{verb, "muse", "--home", home}
		if verb == "install" {
			args = append(args, "--actor", "example-agent")
		}
		require.Len(t, runInstructions(t, args...).Artifacts, 3, verb)
	}
	data, err := os.ReadFile(foreign) //nolint:gosec // G304: fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, "Shared skill\n", string(data))
}

// Only an unsafe legacy path is skipped. A legacy copy that cannot be
// inspected is reported, so it is not silently left behind.
func TestAgentInstructionsReportsUninspectableLegacySkills(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that bind the current user")
	}
	resetFlags(t)
	home := realTempHome(t)
	installLegacyBundle(t, home)
	legacy := filepath.Join(home, "workspace", "skills")
	require.NoError(t, os.Chmod(legacy, 0))
	t.Cleanup(func() { _ = os.Chmod(legacy, 0o700) }) //nolint:gosec // G302: restore the fixture directory for cleanup.
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "instructions", "status", "muse", "--home", home)
	require.ErrorIs(t, err, os.ErrPermission)
}
