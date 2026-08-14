// Package storageadmin implements explicitly enabled, root-contained JSONL
// export and restore operations for the MCP server.
package storageadmin

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/jsonl"
)

// Config defines the storage root, active source, and approved targets.
type Config struct {
	Root      string
	SourceDSN string
	Targets   map[string]string
}

// Admin enforces the startup storage boundary.
type Admin struct {
	root      string
	files     *os.Root
	sourceDSN string
	targets   map[string]string
}

// ExportOptions selects a root-contained artifact and optional project.
type ExportOptions struct {
	Artifact       string
	ProjectID      int64
	IncludeDeleted bool
	Force          bool
	Confirm        string
}

// ExportResult reports an installed JSONL artifact.
type ExportResult struct {
	Artifact string `json:"artifact"`
	Bytes    int64  `json:"bytes"`
}

// ImportOptions selects an artifact and approved restore target.
type ImportOptions struct {
	Artifact    string
	Target      string
	Force       bool
	Confirm     string
	NewInstance bool
}

// ImportResult reports a completed JSONL restore.
type ImportResult struct {
	Artifact string `json:"artifact"`
	Target   string `json:"target"`
	Backend  string `json:"backend"`
}

// New validates and fixes the storage administration boundary.
func New(config Config) (*Admin, error) {
	root := strings.TrimSpace(config.Root)
	if root == "" {
		return nil, errors.New("storage root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("stat storage root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("storage root must be a directory")
	}
	files, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("open storage root: %w", err)
	}
	targets := make(map[string]string, len(config.Targets))
	for rawAlias, rawTarget := range config.Targets {
		alias := strings.TrimSpace(rawAlias)
		target := strings.TrimSpace(rawTarget)
		if alias == "" || target == "" {
			_ = files.Close()
			return nil, errors.New("storage target aliases and values must not be empty")
		}
		if _, exists := targets[alias]; exists {
			_ = files.Close()
			return nil, fmt.Errorf("duplicate storage target alias %q", alias)
		}
		targets[alias] = target
	}
	admin := &Admin{root: canonical, files: files, sourceDSN: strings.TrimSpace(config.SourceDSN), targets: targets}
	if err := admin.validateSQLiteTargets(); err != nil {
		_ = files.Close()
		return nil, err
	}
	return admin, nil
}

// Close releases the descriptor that anchors storage operations to the
// configured root.
func (a *Admin) Close() error {
	if a == nil || a.files == nil {
		return nil
	}
	return a.files.Close()
}

// ResolveArtifact resolves a safe relative artifact under the configured root.
func (a *Admin) ResolveArtifact(name string, mustExist bool) (string, error) {
	name, err := a.resolveArtifactName(name, mustExist)
	if err != nil {
		return "", err
	}
	return filepath.Join(a.root, name), nil
}

func (a *Admin) resolveArtifactName(name string, mustExist bool) (string, error) {
	if a == nil || a.root == "" || a.files == nil {
		return "", errors.New("storage administration is not configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("artifact is required")
	}
	if filepath.IsAbs(name) {
		return "", errors.New("artifact must be relative to the storage root")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact escapes the storage root")
	}
	current := "."
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := a.files.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if mustExist || index < len(parts)-1 {
				return "", fmt.Errorf("artifact path does not exist: %s", filepath.Join(parts[:index+1]...))
			}
			continue
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect artifact path: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("artifact path must not contain a symbolic link")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", errors.New("artifact parent must be a directory")
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return "", errors.New("artifact must be a regular file")
		}
	}
	return clean, nil
}

// Export writes an atomic JSONL artifact from the active storage.
func (a *Admin) Export(ctx context.Context, options ExportOptions) (ExportResult, error) {
	if a.sourceDSN == "" {
		return ExportResult{}, errors.New("active storage source is not configured")
	}
	outputName, err := a.resolveArtifactName(options.Artifact, false)
	if err != nil {
		return ExportResult{}, err
	}
	output := filepath.Join(a.root, outputName)
	protected, err := a.protectedExportPaths()
	if err != nil {
		return ExportResult{}, err
	}
	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	for _, path := range protected {
		if storagePathsEquivalent(output, path, caseInsensitive) {
			return ExportResult{}, errors.New("export artifact is a protected storage path")
		}
	}
	_, statErr := a.files.Lstat(outputName)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return ExportResult{}, fmt.Errorf("inspect export artifact: %w", statErr)
	}
	if exists && !options.Force {
		return ExportResult{}, errors.New("export artifact exists; force and exact confirmation are required")
	}
	if exists {
		expected := "OVERWRITE ARTIFACT " + filepath.Clean(strings.TrimSpace(options.Artifact))
		if options.Confirm != expected {
			return ExportResult{}, fmt.Errorf("confirm must equal %q", expected)
		}
	}
	store, err := storeopen.OpenReadOnly(ctx, a.sourceDSN)
	if err != nil {
		return ExportResult{}, err
	}
	defer func() { _ = store.Close() }()
	file, temporaryName, err := a.createTempArtifact(filepath.Dir(outputName), filepath.Base(outputName))
	if err != nil {
		return ExportResult{}, fmt.Errorf("create export artifact: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = file.Close()
			_ = a.files.Remove(temporaryName)
		}
	}()
	writer := bufio.NewWriter(file)
	if err := jsonl.Export(ctx, store, writer, jsonl.ExportOptions{ProjectID: options.ProjectID, IncludeDeleted: options.IncludeDeleted}); err != nil {
		return ExportResult{}, err
	}
	if err := writer.Flush(); err != nil {
		return ExportResult{}, fmt.Errorf("flush export artifact: %w", err)
	}
	if err := file.Sync(); err != nil {
		return ExportResult{}, fmt.Errorf("sync export artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return ExportResult{}, fmt.Errorf("close export artifact: %w", err)
	}
	if exists {
		if err := a.files.Rename(temporaryName, outputName); err != nil {
			return ExportResult{}, fmt.Errorf("replace export artifact: %w", err)
		}
	} else {
		if err := a.files.Link(temporaryName, outputName); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ExportResult{}, errors.New("export artifact appeared concurrently; force and exact confirmation are required")
			}
			return ExportResult{}, fmt.Errorf("install export artifact without replacement: %w", err)
		}
		if err := a.files.Remove(temporaryName); err != nil {
			return ExportResult{}, fmt.Errorf("remove temporary export artifact: %w", err)
		}
	}
	committed = true
	info, err := a.files.Stat(outputName)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Artifact: options.Artifact, Bytes: info.Size()}, nil
}

func (a *Admin) createTempArtifact(directory, base string) (*os.File, string, error) {
	for range 100 {
		name, err := temporaryArtifactName(directory, base)
		if err != nil {
			return nil, "", err
		}
		file, err := a.files.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", errors.New("create temporary artifact: name collisions exhausted")
}

func temporaryArtifactName(directory, base string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate temporary artifact name: %w", err)
	}
	return filepath.Join(directory, "."+base+".tmp-"+hex.EncodeToString(random)), nil
}

func (a *Admin) stageSQLiteSet(source, directory, base string) (string, error) {
	suffixes := []string{"", "-wal", "-shm", "-journal"}
	for range 100 {
		name, err := temporaryArtifactName(directory, base)
		if err != nil {
			return "", err
		}
		staged := false
		collision := false
		created := make([]string, 0, len(suffixes))
		cleanupCreated := func() {
			for _, suffix := range created {
				_ = a.files.Remove(name + suffix)
			}
		}
		for _, suffix := range suffixes {
			sourcePath := source + suffix
			sourceFile, openErr := os.Open(sourcePath) //nolint:gosec // source is a private temporary directory
			if errors.Is(openErr, os.ErrNotExist) {
				if suffix == "" {
					return "", errors.New("imported SQLite file set has no main database")
				}
				continue
			}
			if openErr != nil {
				cleanupCreated()
				return "", openErr
			}
			destination, createErr := a.files.OpenFile(name+suffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(createErr, os.ErrExist) {
				_ = sourceFile.Close()
				collision = true
				break
			}
			if createErr != nil {
				_ = sourceFile.Close()
				cleanupCreated()
				return "", createErr
			}
			created = append(created, suffix)
			_, copyErr := io.Copy(destination, sourceFile)
			syncErr := destination.Sync()
			closeDestinationErr := destination.Close()
			closeSourceErr := sourceFile.Close()
			if copyErr != nil || syncErr != nil || closeDestinationErr != nil || closeSourceErr != nil {
				cleanupCreated()
				return "", errors.Join(copyErr, syncErr, closeDestinationErr, closeSourceErr)
			}
			staged = true
		}
		if collision {
			cleanupCreated()
			continue
		}
		if staged {
			return name, nil
		}
	}
	return "", errors.New("stage imported SQLite file set: name collisions exhausted")
}

func (a *Admin) protectedExportPaths() ([]string, error) {
	protected := make([]string, 0, 4*(len(a.targets)+1))
	addSQLiteSet := func(path string) {
		path = filepath.Clean(path)
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			protected = append(protected, path+suffix)
		}
	}
	active, sqlite, err := a.activeSQLitePath()
	if err != nil {
		return nil, err
	}
	if sqlite {
		addSQLiteSet(active)
	}
	for alias, targetValue := range a.targets {
		targetBackend, targetErr := storeopen.BackendForDSN(targetValue)
		if targetErr != nil {
			return nil, fmt.Errorf("resolve storage target %q: %w", alias, targetErr)
		}
		if targetBackend != storeopen.BackendSQLite {
			continue
		}
		targetName, targetErr := sqliteTargetName(targetValue)
		if targetErr != nil {
			return nil, fmt.Errorf("resolve storage target %q: %w", alias, targetErr)
		}
		target, targetErr := a.ResolveArtifact(targetName, false)
		if targetErr != nil {
			return nil, fmt.Errorf("resolve storage target %q: %w", alias, targetErr)
		}
		addSQLiteSet(target)
	}
	return protected, nil
}

func sqliteTargetName(value string) (string, error) {
	identity, err := config.CanonicalDSNIdentity(value)
	if err != nil {
		return "", err
	}
	return identity, nil
}

func storagePathsEquivalent(left, right string, caseInsensitive bool) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right || caseInsensitive && strings.EqualFold(left, right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func (a *Admin) activeSQLitePath() (string, bool, error) {
	backend, err := storeopen.BackendForDSN(a.sourceDSN)
	if err != nil {
		return "", false, err
	}
	if backend != storeopen.BackendSQLite {
		return "", false, nil
	}
	identity, err := config.CanonicalDSNIdentity(a.sourceDSN)
	if err != nil {
		return "", false, err
	}
	absolute, err := filepath.Abs(identity)
	if err != nil {
		return "", false, fmt.Errorf("resolve active storage path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		absolute = canonical
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("canonicalize active storage path: %w", err)
	}
	return filepath.Clean(absolute), true, nil
}

func (a *Admin) validateSQLiteTargets() error {
	active, sourceIsSQLite, err := a.activeSQLitePath()
	if err != nil {
		return err
	}
	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	aliases := make([]string, 0, len(a.targets))
	for alias := range a.targets {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	resolved := make(map[string]string, len(aliases))
	for _, alias := range aliases {
		backend, backendErr := storeopen.BackendForDSN(a.targets[alias])
		if backendErr != nil {
			return fmt.Errorf("resolve storage target %q: %w", alias, backendErr)
		}
		if backend != storeopen.BackendSQLite {
			continue
		}
		targetName, targetErr := sqliteTargetName(a.targets[alias])
		if targetErr != nil {
			return fmt.Errorf("resolve storage target %q: %w", alias, targetErr)
		}
		target, targetErr := a.ResolveArtifact(targetName, false)
		if targetErr != nil {
			return fmt.Errorf("resolve storage target %q: %w", alias, targetErr)
		}
		if sourceIsSQLite && sqliteFileSetsOverlap(target, active, caseInsensitive) {
			return fmt.Errorf("storage target %q overlaps active SQLite storage", alias)
		}
		// Force-importing one alias moves and deletes the target's whole
		// sidecar set, so two aliases may not share any of those paths.
		for _, otherAlias := range aliases {
			other, seen := resolved[otherAlias]
			if !seen {
				continue
			}
			if sqliteFileSetsOverlap(target, other, caseInsensitive) {
				return fmt.Errorf("storage targets %q and %q overlap", otherAlias, alias)
			}
		}
		resolved[alias] = target
	}
	return nil
}

func sqliteFileSetsOverlap(left, right string, caseInsensitive bool) bool {
	suffixes := []string{"", "-wal", "-shm", "-journal"}
	for _, leftSuffix := range suffixes {
		for _, rightSuffix := range suffixes {
			if storagePathsEquivalent(left+leftSuffix, right+rightSuffix, caseInsensitive) {
				return true
			}
		}
	}
	return false
}

// Import restores a JSONL artifact into an approved target.
func (a *Admin) Import(ctx context.Context, options ImportOptions) (ImportResult, error) {
	inputName, err := a.resolveArtifactName(options.Artifact, true)
	if err != nil {
		return ImportResult{}, err
	}
	input, err := a.files.Open(inputName)
	if err != nil {
		return ImportResult{}, fmt.Errorf("open import artifact: %w", err)
	}
	defer func() { _ = input.Close() }()
	targetValue, ok := a.targets[strings.TrimSpace(options.Target)]
	if !ok {
		return ImportResult{}, fmt.Errorf("storage target alias %q is not configured", options.Target)
	}
	backend, err := storeopen.BackendForDSN(targetValue)
	if err != nil {
		return ImportResult{}, err
	}
	if backend == storeopen.BackendPostgres {
		if config.DBHash(a.sourceDSN) == config.DBHash(targetValue) {
			return ImportResult{}, errors.New("storage import target must differ from the active daemon storage")
		}
		if options.Force {
			return ImportResult{}, errors.New("force replacement of PostgreSQL storage is not available through MCP")
		}
		pgConfig, err := postgresRestoreConfig(ctx)
		if err != nil {
			return ImportResult{}, err
		}
		// Preflight without mutation: an initialized target is rejected
		// before any schema work can touch it, and a missing schema is
		// created under the explicit restore bootstrap configuration
		// rather than the daemon's ambient schema mode.
		version, err := pgstore.PeekSchemaVersionWithConfig(ctx, targetValue, pgConfig)
		if err != nil {
			return ImportResult{}, err
		}
		if version != 0 {
			same, sameErr := a.targetIsActiveStorage(ctx, targetValue)
			if sameErr != nil {
				return ImportResult{}, sameErr
			}
			if same {
				return ImportResult{}, errors.New("storage import target must differ from the active daemon storage")
			}
			return ImportResult{}, errors.New("storage target already contains a Kata schema; PostgreSQL restore requires a fresh target")
		}
		return a.importFresh(ctx, input, targetValue, options, "postgres", &pgConfig)
	}
	targetName, err := sqliteTargetName(targetValue)
	if err != nil {
		return ImportResult{}, fmt.Errorf("resolve storage target %q: %w", options.Target, err)
	}
	targetName, err = a.resolveArtifactName(targetName, false)
	if err != nil {
		return ImportResult{}, fmt.Errorf("resolve storage target %q: %w", options.Target, err)
	}
	target := filepath.Join(a.root, targetName)
	active, sourceIsSQLite, activeErr := a.activeSQLitePath()
	if activeErr != nil {
		return ImportResult{}, activeErr
	}
	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if (sourceIsSQLite && sqliteFileSetsOverlap(target, active, caseInsensitive)) ||
		config.DBHash(a.sourceDSN) == config.DBHash(target) {
		return ImportResult{}, errors.New("storage import target must differ from the active daemon storage")
	}
	// Forced replacement moves the target set through a backup and deletes
	// it; an artifact overlapping the target would destroy the only JSONL
	// copy under a confirmation that covers target replacement only.
	if sqliteFileSetsOverlap(target, filepath.Join(a.root, inputName), caseInsensitive) {
		return ImportResult{}, errors.New("storage import artifact must differ from the SQLite target and its sidecar paths")
	}
	_, statErr := a.files.Lstat(targetName)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return ImportResult{}, statErr
	}
	if exists && !options.Force {
		return ImportResult{}, errors.New("storage target exists; force and exact confirmation are required")
	}
	if options.Force {
		expected := "REPLACE STORAGE " + options.Target
		if options.Confirm != expected {
			return ImportResult{}, fmt.Errorf("confirm must equal %q", expected)
		}
	}
	privateDirectory, err := os.MkdirTemp("", "kata-mcp-import-*")
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { _ = os.RemoveAll(privateDirectory) }() //nolint:gosec // directory was created above
	temporary := filepath.Join(privateDirectory, "restore.db")
	result, err := a.importFresh(ctx, input, temporary, options, "sqlite", nil)
	if err != nil {
		return ImportResult{}, err
	}
	temporaryName, err := a.stageSQLiteSet(temporary, filepath.Dir(targetName), filepath.Base(targetName)+".import")
	if err != nil {
		return ImportResult{}, err
	}
	installed := false
	defer func() {
		if !installed {
			_ = a.cleanupSQLiteSet(temporaryName)
		}
	}()
	backupName := targetName + ".mcp-import-backup"
	if exists {
		if _, err := a.files.Lstat(backupName); !errors.Is(err, os.ErrNotExist) {
			return ImportResult{}, errors.New("storage target backup already exists")
		}
		if err := a.moveSQLiteSet(targetName, backupName); err != nil {
			return ImportResult{}, err
		}
	}
	if err := a.moveSQLiteSet(temporaryName, targetName); err != nil {
		if exists {
			restoreErr := a.moveSQLiteSet(backupName, targetName)
			if restoreErr != nil {
				return ImportResult{}, errors.Join(err, fmt.Errorf(
					"restore original SQLite target from %s: %w", backupName, restoreErr,
				))
			}
		}
		return ImportResult{}, err
	}
	installed = true
	if exists {
		if err := a.cleanupSQLiteSet(backupName); err != nil {
			return ImportResult{}, fmt.Errorf("replacement succeeded but cleanup of retained backup %s failed: %w", backupName, err)
		}
	}
	result.Target = options.Target
	return result, nil
}

func (a *Admin) importFresh(ctx context.Context, input io.Reader, target string, options ImportOptions, backend string, pgConfig *pgstore.Config) (ImportResult, error) {
	var store db.Storage
	var err error
	if pgConfig != nil {
		store, err = pgstore.OpenWithConfig(ctx, target, *pgConfig)
	} else {
		store, err = storeopen.Open(ctx, target)
	}
	if err != nil {
		return ImportResult{}, err
	}
	installedFreshPostgres := storeopen.InstalledFreshPostgresSchema(store)
	instanceUID := store.InstanceUID()
	fail := func(importErr error) (ImportResult, error) {
		closeErr := store.Close()
		if installedFreshPostgres && pgConfig != nil {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			cleanupErr := pgstore.RemoveFreshSchemaWithConfig(cleanupContext, target, instanceUID, *pgConfig)
			return ImportResult{}, errors.Join(importErr, closeErr, cleanupErr)
		}
		return ImportResult{}, errors.Join(importErr, closeErr)
	}
	if err := jsonl.ImportWithOptions(ctx, input, store, jsonl.ImportOptions{RequireFreshTarget: true, NewInstance: options.NewInstance}); err != nil {
		return fail(err)
	}
	if err := store.Close(); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Artifact: options.Artifact, Target: options.Target, Backend: backend}, nil
}

// postgresRestoreConfig mirrors the CLI import flow: restore is an explicit
// schema-owner operation that must prepare a fresh target even when the
// daemon runtime is validation-only.
func postgresRestoreConfig(ctx context.Context) (pgstore.Config, error) {
	storageConfig, err := config.KataPostgresStorageConfig(ctx)
	if err != nil {
		return pgstore.Config{}, err
	}
	pgConfig := pgstore.ConfigFromValues(
		storageConfig.Schema, storageConfig.Mode, storageConfig.SchemaOwner,
		storageConfig.AllowInsecure,
	)
	pgConfig.SchemaMode = pgstore.SchemaModeBootstrap
	return pgConfig, nil
}

// targetIsActiveStorage compares persisted instance identities so an
// equivalent DSN spelling of the active database is named as such. Both
// handles open read-only; nothing is mutated.
func (a *Admin) targetIsActiveStorage(ctx context.Context, target string) (bool, error) {
	sourceUID, err := instanceUIDOf(ctx, a.sourceDSN)
	if err != nil || sourceUID == "" {
		return false, err
	}
	targetUID, err := instanceUIDOf(ctx, target)
	if err != nil {
		return false, err
	}
	return targetUID != "" && targetUID == sourceUID, nil
}

// instanceUIDOf reads a storage target's persisted instance identity over a
// read-only handle.
func instanceUIDOf(ctx context.Context, dsn string) (string, error) {
	store, err := storeopen.OpenReadOnly(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("open storage identity: %w", err)
	}
	defer func() { _ = store.Close() }()
	uid := store.InstanceUID()
	if uid == "" {
		// A read-only handle may not have cached the identity; a refresh
		// failure means no persisted identity exists yet.
		if refreshErr := store.RefreshInstanceUID(ctx); refreshErr == nil {
			uid = store.InstanceUID()
		}
	}
	return uid, nil
}

func moveSQLiteSetWithLink(from, to string, link func(string, string) error) error {
	return moveSQLiteSetWithOps(from, to, sqliteSetOps{
		lstat:  os.Lstat,
		link:   link,
		remove: os.Remove,
	})
}

type sqliteSetOps struct {
	lstat  func(string) (os.FileInfo, error)
	link   func(string, string) error
	remove func(string) error
}

func (a *Admin) moveSQLiteSet(from, to string) error {
	return moveSQLiteSetWithOps(from, to, sqliteSetOps{
		lstat:  a.files.Lstat,
		link:   a.files.Link,
		remove: a.files.Remove,
	})
}

func moveSQLiteSetWithOps(from, to string, operations sqliteSetOps) error {
	suffixes := []string{"", "-wal", "-shm", "-journal"}
	sources := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		path := from + suffix
		if _, err := operations.lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		sources = append(sources, suffix)
	}
	if len(sources) == 0 || sources[0] != "" {
		return errors.New("SQLite file set has no main database")
	}
	for _, suffix := range suffixes {
		if _, err := operations.lstat(to + suffix); err == nil {
			return fmt.Errorf("SQLite destination already exists: %s", filepath.Base(to+suffix))
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	moved := make([]string, 0, len(sources))
	rollback := func(cause error) error {
		errorsOut := []error{cause}
		for index := len(moved) - 1; index >= 0; index-- {
			suffix := moved[index]
			oldPath, newPath := from+suffix, to+suffix
			if err := operations.link(newPath, oldPath); err != nil {
				errorsOut = append(errorsOut, fmt.Errorf("restore SQLite source %s: %w", filepath.Base(oldPath), err))
				continue
			}
			if err := operations.remove(newPath); err != nil {
				errorsOut = append(errorsOut, fmt.Errorf("remove rolled-back SQLite destination %s: %w", filepath.Base(newPath), err))
			}
		}
		return errors.Join(errorsOut...)
	}
	for _, suffix := range sources {
		oldPath, newPath := from+suffix, to+suffix
		if err := operations.link(oldPath, newPath); err != nil {
			return rollback(fmt.Errorf("install SQLite file %s without replacement: %w", filepath.Base(newPath), err))
		}
		if err := operations.remove(oldPath); err != nil {
			cleanupErr := operations.remove(newPath)
			return rollback(errors.Join(
				fmt.Errorf("remove SQLite source %s: %w", filepath.Base(oldPath), err),
				cleanupErr,
			))
		}
		moved = append(moved, suffix)
	}
	return nil
}

func (a *Admin) cleanupSQLiteSet(path string) error {
	var cleanupErrors []error
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		name := path + suffix
		if err := a.files.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove SQLite file %s: %w", filepath.Base(name), err))
		}
	}
	return errors.Join(cleanupErrors...)
}
