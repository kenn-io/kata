package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

func TestNativeAgentHookPublish(t *testing.T) {
	t.Run("create and repeat", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "extensions", "kata.js")
		plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Content: []byte("native extension\n")}}}
		changed, err := publishNativeAgentHookPlan(plan)
		if err != nil || !changed {
			t.Fatalf("create: %t %v", changed, err)
		}
		data, exists, err := readNativeAgentHookFile(path)
		if err != nil || !exists || string(data) != "native extension\n" {
			t.Fatalf("snapshot: %q %t %v", data, exists, err)
		}
		plan.Changes[0].Original = data
		plan.Changes[0].OriginalExists = true
		changed, err = publishNativeAgentHookPlan(plan)
		if err != nil || changed {
			t.Fatalf("repeat: %t %v", changed, err)
		}
	})
	t.Run("external change rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hooks.json")
		if err := os.WriteFile(path, []byte("authored"), 0600); err != nil {
			t.Fatal(err)
		}
		plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Original: []byte("older"), OriginalExists: true, Content: []byte("managed")}}}
		_, err := publishNativeAgentHookPlan(plan)
		if err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("want conflict, got %v", err)
		}
		got, _ := os.ReadFile(path) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
		if string(got) != "authored" {
			t.Fatalf("overwrote authored: %q", got)
		}
	})
	t.Run("symlink ancestor preserved", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "extensions")); err != nil {
			t.Skip(err)
		}
		path := filepath.Join(root, "extensions", "kata.js")
		if _, exists, err := readNativeAgentHookFile(path); err != nil || exists {
			t.Fatalf("new linked config: exists=%t err=%v", exists, err)
		}
		if _, err := publishNativeAgentHookPlan(nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Content: []byte("managed")}}}); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(outside, "kata.js")); err != nil || string(data) != "managed" { //nolint:gosec // G304: isolated symlink target under TempDir.
			t.Fatalf("linked config: %q %v", data, err)
		}
	})
	t.Run("all snapshots validated before publication", func(t *testing.T) {
		root := t.TempDir()
		one := filepath.Join(root, "one.js")
		two := filepath.Join(root, "two.js")
		if err := os.WriteFile(two, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := publishNativeAgentHookPlan(nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: one, Content: []byte("one")}, {Path: two, Content: []byte("two")}}})
		if err == nil {
			t.Fatal("accepted stale second snapshot")
		}
		if _, err := os.Stat(one); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial publish: %v", err)
		}
	})
	t.Run("remove", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "owned.js")
		if err := os.WriteFile(path, []byte("owned"), 0600); err != nil {
			t.Fatal(err)
		}
		changed, err := publishNativeAgentHookPlan(nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Original: []byte("owned"), OriginalExists: true, Remove: true}}})
		if err != nil || !changed {
			t.Fatalf("remove: %t %v", changed, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retained artifact: %v", err)
		}
	})
}

func TestProjectNativeAgentHookRejectsSymlinkEscape(t *testing.T) {
	opts := piTestOptions(t)
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(opts.Dir, ".pi")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	plan, err := planNativeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := publishNativeAgentHookPlan(plan)
	if err == nil || changed || !strings.Contains(err.Error(), "outside project") {
		t.Fatalf("project hook escaped workspace: changed=%t err=%v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(external, "extensions", "kata.js")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external hook artifact was created: %v", err)
	}
}

func TestProjectNativeAgentHookAllowsExplicitExternalConfig(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	externalConfig := filepath.Join(external, "hooks.json")
	if err := os.WriteFile(externalConfig, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(workspace, ".agents")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "hooks.json")
	if err := os.Symlink(externalConfig, configPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	opts := nativeAgentHookOptions{
		Agent: "antigravity", Scope: "project", Home: t.TempDir(), Dir: workspace,
		ConfigPath: configPath, Executable: "kata", ConfigPathExplicit: true, Contract: true,
	}
	plan, err := planNativeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := publishNativeAgentHookPlan(plan)
	if err != nil || !changed {
		t.Fatalf("explicit external config: changed=%t err=%v", changed, err)
	}
	if info, err := os.Stat(externalConfig); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("explicit config target was not preserved: info=%v err=%v", info, err)
	}
}

func TestNativeAgentHookPublishRollback(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "own changes rolled back", true: "external edits retained"}[external], func(t *testing.T) {
			root := t.TempDir()
			one := filepath.Join(root, "one.js")
			two := filepath.Join(root, "two.js")
			if err := os.WriteFile(one, []byte("before"), 0600); err != nil {
				t.Fatal(err)
			}
			plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: one, Original: []byte("before"), OriginalExists: true, Content: []byte("after")}, {Path: two, Content: []byte("second")}}}
			fail := errors.New("publication failed")
			calls := 0
			rename := func(old, destination string) error {
				calls++
				if calls == 2 {
					if external {
						if err := os.WriteFile(one, []byte("external"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return fail
				}
				return os.Rename(old, destination)
			}
			changed, err := publishNativeAgentHookPlanWithRename(plan, rename)
			if !errors.Is(err, fail) {
				t.Fatalf("want publish failure, got %v", err)
			}
			got, _ := os.ReadFile(one) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			want := "before"
			if external {
				want = "external"
			}
			if string(got) != want {
				t.Fatalf("rollback: got %q want %q", got, want)
			}
			if changed != external {
				t.Fatalf("retained changes=%t want %t", changed, external)
			}
			if external && !strings.Contains(err.Error(), "retained") {
				t.Fatalf("missing retained-artifact report: %v", err)
			}
		})
	}
}

func TestNativeAgentHookRemoveRollbackRestoresPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix executable permission bits")
	}
	for _, mode := range []os.FileMode{0644, 0755} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			root := t.TempDir()
			removed := filepath.Join(root, "owned.js")
			second := filepath.Join(root, "config.json")
			before := []byte("owned extension\n")
			if err := os.WriteFile(removed, before, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(removed, mode); err != nil {
				t.Fatal(err)
			}
			plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{
				{Path: removed, Original: before, OriginalExists: true, Remove: true},
				{Path: second, Content: []byte("config\n")},
			}}
			failure := errors.New("second publication failed")
			rename := func(old, destination string) error {
				if destination == second {
					if _, err := os.Stat(removed); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("first artifact was not removed: %v", err)
					}
					return failure
				}
				return os.Rename(old, destination)
			}
			changed, err := publishNativeAgentHookPlanWithRename(plan, rename)
			if changed || !errors.Is(err, failure) {
				t.Fatalf("rollback result: changed=%t err=%v", changed, err)
			}
			got, err := os.ReadFile(removed) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if err != nil || string(got) != string(before) {
				t.Fatalf("restored bytes: %q err=%v", got, err)
			}
			info, err := os.Stat(removed)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != mode {
				t.Fatalf("restored permissions: got %04o want %04o", info.Mode().Perm(), mode)
			}
			if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("second artifact was published: %v", err)
			}
		})
	}
}

func TestNativeAgentHookPublishLocksUnchangedArtifacts(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged config", true: "absent removal"}[absent], func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "config.json")
			extension := filepath.Join(root, "extension.js")
			configChange := nativeAgentHookChange{Path: config, Remove: true}
			if !absent {
				if err := os.WriteFile(config, []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
				configChange = nativeAgentHookChange{Path: config, OriginalExists: true, Original: []byte("unchanged"), Content: []byte("unchanged")}
			}
			lockPath, err := nativeAgentHookLockPath(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
				t.Fatal(err)
			}
			lock := flock.New(lockPath)
			if err := lock.Lock(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lock.Unlock(); err != nil {
					t.Error(err)
				}
			})
			plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{configChange, {Path: extension, Content: []byte("new")}}}
			changed, err := publishNativeAgentHookPlan(plan)
			if changed || err == nil || !strings.Contains(err.Error(), "being updated") {
				t.Fatalf("locked bundle: changed=%t err=%v", changed, err)
			}
			if _, err := os.Stat(extension); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published with locked preimage: %v", err)
			}
		})
	}
}

func TestNativeAgentHookPublishRevalidatesUnchangedArtifactsAfterStaging(t *testing.T) {
	for _, absent := range []bool{false, true} {
		for _, configFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("absent=%t/config-first=%t", absent, configFirst), func(t *testing.T) {
				root := t.TempDir()
				config := filepath.Join(root, "config.json")
				extension := filepath.Join(root, "extension.js")
				configChange := nativeAgentHookChange{Path: config, Remove: true}
				if !absent {
					if err := os.WriteFile(config, []byte("unchanged"), 0600); err != nil {
						t.Fatal(err)
					}
					configChange = nativeAgentHookChange{Path: config, OriginalExists: true, Original: []byte("unchanged"), Content: []byte("unchanged")}
				}
				changes := []nativeAgentHookChange{configChange, {Path: extension, Content: []byte("new")}}
				if !configFirst {
					changes[0], changes[1] = changes[1], changes[0]
				}
				stage := func(path string, data []byte, mode os.FileMode) (string, error) {
					if path == config {
						t.Fatal("staged an unchanged artifact")
					}
					staged, err := stageNativeAgentHookFile(path, data, mode)
					if err != nil {
						return staged, err
					}
					if err := os.WriteFile(config, []byte("external edit"), 0600); err != nil {
						t.Fatal(err)
					}
					return staged, nil
				}
				changed, err := publishNativeAgentHookPlanWithFileOps(nativeAgentHookPlan{Changes: changes}, stage, os.Rename)
				if changed || err == nil || !strings.Contains(err.Error(), "changed after planning") {
					t.Fatalf("staging conflict: changed=%t err=%v", changed, err)
				}
				got, err := os.ReadFile(config) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
				if err != nil || string(got) != "external edit" {
					t.Fatalf("external edit not preserved: %q err=%v", got, err)
				}
				if _, err := os.Stat(extension); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published with stale bundle preimage: %v", err)
				}
				staged, err := filepath.Glob(filepath.Join(root, ".kata-hook-*"))
				if err != nil || len(staged) != 0 {
					t.Fatalf("staging files remain: %v err=%v", staged, err)
				}
			})
		}
	}
}

// Arbitrary file bytes must survive no-op publication, while any unequal
// preimage must be rejected without replacing the current file.
func FuzzNativeAgentHookSnapshot(f *testing.F) {
	f.Add([]byte("owned\n"), []byte("external\n"))
	f.Add([]byte{}, []byte{0, 255})
	f.Add([]byte("same"), []byte("same"))
	f.Fuzz(func(t *testing.T, before, external []byte) {
		if len(before) > 65536 || len(external) > 65536 {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), "owned.js")
		if err := os.WriteFile(path, before, 0600); err != nil {
			t.Fatal(err)
		}
		plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{{Path: path, Original: before, OriginalExists: true, Content: before}}}
		changed, err := publishNativeAgentHookPlan(plan)
		if err != nil || changed {
			t.Fatalf("no-op: %t %v", changed, err)
		}
		if err := os.WriteFile(path, external, 0600); err != nil {
			t.Fatal(err)
		}
		changed, err = publishNativeAgentHookPlan(plan)
		if string(before) != string(external) && err == nil {
			t.Fatal("accepted unequal snapshot")
		}
		if changed {
			t.Fatal("conflict changed file")
		}
		got, e := os.ReadFile(path) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
		if e != nil || string(got) != string(external) {
			t.Fatalf("foreign bytes not preserved: %q %v", got, e)
		}
	})
}

func TestNativeAgentHookAbsentGuardKeepsParentAbsent(t *testing.T) {
	root := t.TempDir()
	absentParent := filepath.Join(root, "unused-discovery")
	plan := nativeAgentHookPlan{Changes: []nativeAgentHookChange{
		{Path: filepath.Join(root, "active-discovery", "hook.js"), Content: []byte("managed")},
		{Path: filepath.Join(absentParent, "hook.js"), Remove: true},
	}}
	changed, err := publishNativeAgentHookPlan(plan)
	if err != nil || !changed {
		t.Fatalf("publish: %t %v", changed, err)
	}
	if _, err := os.Stat(absentParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent guard created discovery directory: %v", err)
	}
}
