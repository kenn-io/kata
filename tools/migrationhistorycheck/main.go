// Package main protects released PostgreSQL migration history at commit time.
package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

const (
	defaultBaseRef      = "origin/main"
	defaultMigrationDir = "internal/db/pgstore/migrations"
)

var gitEnv []string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Stderr))
}

func run(ctx context.Context, stderr io.Writer) int {
	baseRef := getenvDefault("KATA_MIGRATION_BASE_REF", defaultBaseRef)
	migrationDir := strings.TrimRight(getenvDefault("KATA_MIGRATION_DIR", defaultMigrationDir), "/")

	if _, err := git(ctx, "rev-parse", "--git-dir"); err != nil {
		writef(stderr, "migration history check must run inside a git worktree\n")
		return 1
	}
	if _, err := git(ctx, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		writef(stderr, "Cannot verify migration history because %s is unavailable.\n", baseRef)
		writef(stderr, "Fetch the main branch or set KATA_MIGRATION_BASE_REF to the main-branch ref to compare against.\n")
		return 1
	}

	diff, err := git(ctx, "diff", "--cached", "--name-status", "--", migrationDir)
	if err != nil {
		writef(stderr, "failed to inspect staged PostgreSQL migrations: %v\n", err)
		return 1
	}

	changedViolations := changedMainBranchMigrations(ctx, baseRef, migrationDir, diff)
	invalidNameViolations := invalidMigrationNameViolations(diff, migrationDir)
	duplicateViolations, err := duplicateMigrationVersionViolations(ctx, baseRef, migrationDir)
	if err != nil {
		writef(stderr, "failed to verify PostgreSQL migration versions: %v\n", err)
		return 1
	}
	if len(changedViolations) == 0 && len(invalidNameViolations) == 0 && len(duplicateViolations) == 0 {
		return 0
	}

	writef(stderr, "Refusing to commit staged PostgreSQL migration history changes.\n")
	if len(changedViolations) > 0 {
		writef(stderr, "\nEdits to migrations that already exist on %s are not allowed.\n", baseRef)
		writef(stderr, "Released migrations are append-only. Add a new numbered forward migration instead.\n")
		writef(stderr, "\nBlocked files:\n")
		for _, path := range changedViolations {
			writef(stderr, "  %s\n", path)
		}
	}
	if len(invalidNameViolations) > 0 {
		writef(stderr, "\nInvalid migration filenames; use NNNNNN_description.up.sql for forward migrations:\n")
		for _, path := range invalidNameViolations {
			writef(stderr, "  %s\n", path)
		}
	}
	if len(duplicateViolations) > 0 {
		writef(stderr, "\nEach target schema version may identify only one migration. Found duplicate migration version assignments:\n")
		for _, violation := range duplicateViolations {
			writef(stderr, "  %s: %s\n", violation.version, strings.Join(violation.names, ", "))
		}
	}
	return 1
}

func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func invalidMigrationNameViolations(diff, migrationDir string) []string {
	var violations []string
	for _, change := range parseNameStatus(diff, migrationDir) {
		if change.dst == "" {
			continue
		}
		if _, _, ok := migrationIdentityFromPath(change.dst); !ok {
			violations = append(violations, change.dst)
		}
	}
	slices.Sort(violations)
	return violations
}

func changedMainBranchMigrations(ctx context.Context, baseRef, migrationDir, diff string) []string {
	violations := map[string]struct{}{}
	for _, change := range parseNameStatus(diff, migrationDir) {
		paths := []string{change.baseRefPath()}
		if change.status == 'R' && change.dst != "" && change.dst != change.src {
			paths = append(paths, change.dst)
		}
		for _, path := range paths {
			if path == "" {
				continue
			}
			if _, err := git(ctx, "cat-file", "-e", baseRef+":"+path); err == nil {
				if stagedPathMatchesBase(ctx, baseRef, path) {
					continue
				}
				violations[path] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(violations))
}

func stagedPathMatchesBase(ctx context.Context, baseRef, path string) bool {
	baseContent, err := git(ctx, "show", baseRef+":"+path)
	if err != nil {
		return false
	}
	stagedContent, err := git(ctx, "show", ":"+path)
	if err != nil {
		return false
	}
	return stagedContent == baseContent
}

type duplicateVersionViolation struct {
	version string
	names   []string
}

func (v duplicateVersionViolation) Compare(other duplicateVersionViolation) int {
	return strings.Compare(v.version, other.version)
}

func duplicateMigrationVersionViolations(ctx context.Context, baseRef, migrationDir string) ([]duplicateVersionViolation, error) {
	indexByVersion, err := migrationNamesByVersionOnIndex(ctx, migrationDir)
	if err != nil {
		return nil, err
	}
	baseByVersion, err := migrationNamesByVersionOnRef(ctx, baseRef, migrationDir)
	if err != nil {
		return nil, err
	}
	for version, baseNames := range baseByVersion {
		if _, exists := indexByVersion[version]; !exists {
			indexByVersion[version] = map[string]struct{}{}
		}
		maps.Copy(indexByVersion[version], baseNames)
	}

	var violations []duplicateVersionViolation
	for version, names := range indexByVersion {
		if len(names) <= 1 {
			continue
		}
		violations = append(violations, duplicateVersionViolation{
			version: version,
			names:   slices.Sorted(maps.Keys(names)),
		})
	}
	slices.SortFunc(violations, duplicateVersionViolation.Compare)
	return violations, nil
}

func migrationNamesByVersionOnRef(ctx context.Context, ref, migrationDir string) (map[string]map[string]struct{}, error) {
	output, err := git(ctx, "ls-tree", "-r", "--name-only", ref, "--", migrationDir)
	if err != nil {
		return nil, err
	}
	return migrationNamesByVersion(output), nil
}

func migrationNamesByVersionOnIndex(ctx context.Context, migrationDir string) (map[string]map[string]struct{}, error) {
	output, err := git(ctx, "ls-files", "--cached", "--", migrationDir)
	if err != nil {
		return nil, err
	}
	return migrationNamesByVersion(output), nil
}

func migrationNamesByVersion(output string) map[string]map[string]struct{} {
	byVersion := map[string]map[string]struct{}{}
	for line := range strings.SplitSeq(output, "\n") {
		version, name, ok := migrationIdentityFromPath(line)
		if !ok {
			continue
		}
		if _, exists := byVersion[version]; !exists {
			byVersion[version] = map[string]struct{}{}
		}
		byVersion[version][name] = struct{}{}
	}
	return byVersion
}

type stagedChange struct {
	status byte
	src    string
	dst    string
}

// baseRefPath chooses the path that can exist on the base revision. Copies
// deliberately use the destination: checking the untouched source would make
// copying a released migration look like a history edit.
func (change stagedChange) baseRefPath() string {
	if change.status == 'C' {
		return change.dst
	}
	if change.src != "" {
		return change.src
	}
	return change.dst
}

func parseNameStatus(diff, migrationDir string) []stagedChange {
	var changes []stagedChange
	prefix := migrationDir + "/"
	for line := range strings.SplitSeq(diff, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		status := fields[0][0]
		filtered := make([]string, len(fields)-1)
		for i, path := range fields[1:] {
			if strings.HasPrefix(path, prefix) && filepath.Ext(path) == ".sql" {
				filtered[i] = path
			}
		}
		change := stagedChange{status: status}
		switch status {
		case 'R', 'C':
			if len(filtered) >= 2 {
				change.src, change.dst = filtered[0], filtered[1]
			}
		case 'D':
			change.src = filtered[0]
		default:
			change.dst = filtered[0]
		}
		if change.src != "" || change.dst != "" {
			changes = append(changes, change)
		}
	}
	return changes
}

func migrationIdentityFromPath(path string) (string, string, bool) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".up.sql") {
		return "", "", false
	}
	base = strings.TrimSuffix(base, ".up.sql")
	version, name, ok := strings.Cut(base, "_")
	if !ok || len(version) != 6 || strings.Trim(version, "0123456789") != "" || name == "" {
		return "", "", false
	}
	return version, base, true
}

func getenvDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func git(ctx context.Context, args ...string) (string, error) {
	runner := gitcmd.New()
	runner.Env = gitHookEnv(os.Environ())
	runner.StripEnv = false
	output, err := runner.Output(ctx, "", args...)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func gitHookEnv(env []string) []string {
	if gitEnv != nil {
		env = gitEnv
	}
	cleaned := gitenv.StripAll(env)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if isGitHookContextVar(key) {
			cleaned = append(cleaned, entry)
		}
	}
	return cleaned
}

func isGitHookContextVar(key string) bool {
	switch key {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE":
		return true
	default:
		return false
	}
}
