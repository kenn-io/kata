package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/pathresolve"
)

// Native providers only inspect files. Publication is shared so one target's
// package and config updates use the same snapshot and rollback rules.
type nativeAgentHookOptions struct {
	Agent, Scope, Home, Dir, ConfigPath, Executable, Source string
	API                                                     string
	ManagedAttention                                        bool
	SourceSet, Contract, Attention                          bool
}
type nativeAgentHookChange struct {
	Path                   string
	Original, Content      []byte
	OriginalExists, Remove bool
}
type nativeAgentHookPlan struct {
	Path                                                        string
	Changes                                                     []nativeAgentHookChange
	Contract, AttentionStart, AttentionEnd                      bool
	CurrentContract, CurrentAttentionStart, CurrentAttentionEnd bool
	CurrentOwnedContract                                        bool
	CurrentAPI                                                  string
	Warnings                                                    []string
}

// Configs may be managed through dotfile symlinks, including a linked home.
// Publication resolves the destination before replacing it to preserve the link.
func inspectNativeAgentHookFile(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("native hook path %q is not a regular file", path)
	}
	return info, nil
}

// Resolve existing ancestors as well as the final file. New configs and dangling
// link targets may be created, while the selected link itself stays intact.
func resolveNativeAgentHookPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := pathresolve.EvalSymlinks(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return "", readErr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return resolveNativeAgentHookPath(target)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveNativeAgentHookPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func nativeAgentHookLockPath(path string) (string, error) {
	resolved, err := resolveNativeAgentHookPath(path)
	if err != nil {
		return "", err
	}
	root := filepath.Join(os.TempDir(), fmt.Sprintf("kata-agent-hook-locks-%d", os.Getuid()))
	return filepath.Join(root, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(resolved)))), nil
}

func readNativeAgentHookFile(path string) ([]byte, bool, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, false, err
	}
	info, err := inspectNativeAgentHookFile(path)
	if err != nil || info == nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: provider-selected hook artifact after regular-file validation.
	return data, true, err
}

func verifyNativeAgentHookChange(change nativeAgentHookChange) error {
	data, exists, err := readNativeAgentHookFile(change.Path)
	if err != nil {
		return err
	}
	if exists != change.OriginalExists || !bytes.Equal(data, change.Original) {
		return fmt.Errorf("native hook file %q changed after planning; rerun the command", change.Path)
	}
	return nil
}

func publishNativeAgentHookPlan(plan nativeAgentHookPlan) (bool, error) {
	return publishNativeAgentHookPlanWithRename(plan, os.Rename)
}

func publishNativeAgentHookPlanWithRename(plan nativeAgentHookPlan, rename func(string, string) error) (bool, error) {
	return publishNativeAgentHookPlanWithFileOps(plan, stageNativeAgentHookFile, rename)
}

func publishNativeAgentHookPlanWithFileOps(plan nativeAgentHookPlan, stage func(string, []byte, os.FileMode) (string, error), rename func(string, string) error) (changed bool, err error) {
	preimages := make([]nativeAgentHookChange, 0, len(plan.Changes))
	changes := make([]nativeAgentHookChange, 0, len(plan.Changes))
	paths := make([]string, 0, len(plan.Changes))
	seen := map[string]bool{}
	for _, change := range plan.Changes {
		path, e := resolveNativeAgentHookPath(change.Path)
		if e != nil {
			return false, e
		}
		change.Path = path
		if seen[path] {
			return false, fmt.Errorf("duplicate native hook artifact %q", path)
		}
		seen[path] = true
		if e := verifyNativeAgentHookChange(change); e != nil {
			return false, e
		}
		preimages = append(preimages, change)
		paths = append(paths, path)
		if (!change.Remove && change.OriginalExists && bytes.Equal(change.Content, change.Original)) || (change.Remove && !change.OriginalExists) {
			continue
		}
		changes = append(changes, change)
	}
	if len(changes) == 0 {
		return false, nil
	}
	// Locks serialize cooperating Kata writers. Snapshot checks detect external
	// edits before publication, but cannot exclude noncooperating writers in the
	// final check/rename window.
	// Create mutation parents before staging. Absent read-only guards must not
	// create discovery directories that a harness could interpret as installed
	// plugins.
	for _, change := range changes {
		if e := os.MkdirAll(filepath.Dir(change.Path), 0700); e != nil {
			return false, e
		}
	}
	slices.Sort(paths)
	locks := make([]*flock.Flock, 0, len(paths))
	defer func() {
		for _, lock := range slices.Backward(locks) {
			err = errors.Join(err, lock.Unlock())
		}
	}()
	for _, path := range paths {
		if _, e := os.Stat(filepath.Dir(path)); errors.Is(e, os.ErrNotExist) {
			continue // Absence remains guarded by every preimage recheck.
		} else if e != nil {
			return false, e
		}
		lockPath, e := nativeAgentHookLockPath(path)
		if e != nil {
			return false, e
		}
		if e := os.MkdirAll(filepath.Dir(lockPath), 0700); e != nil {
			return false, e
		}
		lock := flock.New(lockPath)
		locked, e := lock.TryLock()
		if e != nil {
			return false, e
		}
		if !locked {
			return false, fmt.Errorf("native hook file %q is being updated; rerun the command", path)
		}
		locks = append(locks, lock)
	}
	// Unchanged artifacts still constrain the bundle and must remain protected.
	for _, change := range preimages {
		if e := verifyNativeAgentHookChange(change); e != nil {
			return false, e
		}
	}
	staged := make([]string, len(changes))
	// Removed files no longer expose their mode when rollback recreates them.
	originalModes := make([]os.FileMode, len(changes))
	defer func() {
		for _, path := range staged {
			if path != "" {
				_ = os.Remove(path)
			}
		}
	}()
	for i, change := range changes {
		if e := verifyNativeAgentHookChange(change); e != nil {
			return false, e
		}
		originalModes[i] = 0600
		if change.OriginalExists {
			info, e := os.Stat(change.Path)
			if e != nil {
				return false, e
			}
			originalModes[i] = info.Mode().Perm()
		}
		if change.Remove {
			continue
		}
		staged[i], err = stage(change.Path, change.Content, originalModes[i])
		if err != nil {
			return false, err
		}
	}
	// All payloads are staged before publication. Recheck every preimage so a
	// failed second plan cannot leave the first artifact newly installed.
	for _, change := range preimages {
		if e := verifyNativeAgentHookChange(change); e != nil {
			return false, e
		}
	}
	published := 0
	for i, change := range changes {
		if err = verifyNativeAgentHookChange(change); err == nil {
			if change.Remove {
				err = os.Remove(change.Path)
			} else {
				err = rename(staged[i], change.Path)
			}
		}
		if err != nil {
			retained, rollbackErr := rollbackNativeAgentHookChanges(changes[:published], originalModes[:published], rename)
			return retained, errors.Join(err, rollbackErr)
		}
		staged[i] = ""
		published++
	}
	return true, nil
}

func stageNativeAgentHookFile(path string, data []byte, mode os.FileMode) (staged string, err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".kata-hook-*")
	if err != nil {
		return "", err
	}
	staged = file.Name()
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(staged)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return staged, err
	}
	if err = file.Chmod(mode); err != nil {
		return staged, err
	}
	if err = file.Sync(); err != nil {
		return staged, err
	}
	err = file.Close()
	return staged, err
}

func rollbackNativeAgentHookChanges(changes []nativeAgentHookChange, originalModes []os.FileMode, rename func(string, string) error) (retained bool, err error) {
	for i, change := range slices.Backward(changes) {
		current, exists, e := readNativeAgentHookFile(change.Path)
		if e != nil || exists == change.Remove || (!change.Remove && !bytes.Equal(current, change.Content)) {
			retained = true
			err = errors.Join(err, fmt.Errorf("retained changed artifact %q during rollback: %w", change.Path, eOrConflict(e)))
			continue
		}
		if !change.OriginalExists {
			e = os.Remove(change.Path)
		} else {
			var staged string
			staged, e = stageNativeAgentHookFile(change.Path, change.Original, originalModes[i])
			if e == nil {
				e = rename(staged, change.Path)
				if e != nil {
					_ = os.Remove(staged)
				}
			}
		}
		if e != nil {
			retained = true
			err = errors.Join(err, fmt.Errorf("retained artifact %q after rollback failed: %w", change.Path, e))
		}
	}
	return retained, err
}
func eOrConflict(err error) error {
	if err != nil {
		return err
	}
	return errors.New("file changed outside this publication")
}
