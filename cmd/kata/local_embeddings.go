package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/embedlocal"
)

var localEmbeddingsEndpoint = embedlocal.Endpoint
var localEmbeddingsInstall = embedlocal.Install
var localEmbeddingsInspect = embedlocal.Inspect

func resolveLocalEmbeddings(ctx context.Context, confirm func(context.Context, int64) (bool, error)) (string, error) {
	endpoint, err := localEmbeddingsEndpoint(ctx, embedlocal.Options{SkipOllama: true})
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return endpoint, err
	}
	installed, err := installLocalEmbeddings(ctx, confirm, nil)
	if err != nil {
		return "", err
	}
	return localEmbeddingsEndpoint(ctx, embedlocal.Options{SkipOllama: true, Executable: installed.Executable})
}

func installLocalEmbeddings(ctx context.Context, confirm func(context.Context, int64) (bool, error), progress func(int64, int64)) (embedlocal.Installation, error) {
	dir, err := embedlocal.DefaultDir()
	if err != nil {
		return embedlocal.Installation{}, err
	}
	return localEmbeddingsInstall(ctx, embedlocal.InstallOptions{DataDir: dir, Confirm: confirm, Progress: progress})
}

func daemonLocalEmbeddingsCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "local-embeddings", Short: "enable search by meaning with a local server", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requireCurrentHomeDaemonCommand(); err != nil {
			return err
		}
		confirm := func(_ context.Context, size int64) (bool, error) {
			if size > 0 {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Download about %.0f MB, shared by all Kenn apps.\n", float64(size)/1_000_000); err != nil {
					return false, err
				}
			}
			if !yes {
				if err := resolveYesNo(cmd, "Enable built-in embeddings? [y/N] "); err != nil {
					if cli, ok := errors.AsType[*cliError](err); ok && cli.Code == "confirm_mismatch" {
						cli.Message = "local embeddings setup cancelled"
					}
					return false, err
				}
			}
			return true, nil
		}
		index, err := enableLocalEmbeddings(cmd.Context(), localEmbeddingsRun{
			confirm: confirm,
			notice:  func(message string) error { _, err := fmt.Fprintln(cmd.ErrOrStderr(), message); return err },
			restart: func(ctx context.Context, apply func() error) error {
				ns, err := daemonruntime.NewNamespace()
				if err != nil {
					return err
				}
				running, release, err := lockLiveDaemons(ctx, ns, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				defer release()
				if err := requireRestartable(running); err != nil {
					return err
				}
				if err := apply(); err != nil {
					return err
				}
				_, _, err = restartRunningDaemons(ctx, ns, running, nil)
				return err
			},
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Service environment settings can override this configuration."); err != nil {
			return err
		}

		switch currentOutputMode() {
		case outputJSON:
			return emitJSON(cmd.OutOrStdout(), struct {
				Index    string `json:"index"`
				Progress string `json:"progress"`
			}{index, "kata health --json"})
		case outputAgent:
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK index=%s progress=%s\n", index, agentValue("kata health --json"))
		case outputHuman:
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Embeddings configured in config.toml, index %s. Check indexing progress with kata health --json.\n", index)
		}
		return err
	}}
	cmd.Flags().BoolVar(&yes, "yes", false, "approve the download and configuration change")
	return cmd
}

type localEmbeddingsRun struct {
	confirm  func(context.Context, int64) (bool, error)
	progress func(int64, int64)
	notice   func(string) error
	restart  func(context.Context, func() error) error
	// keepProvider leaves an already configured external provider in place
	// rather than switching it to the built-in server.
	keepProvider bool
}

func enableLocalEmbeddings(ctx context.Context, run localEmbeddingsRun) (string, error) {
	if strings.TrimSpace(os.Getenv("KATA_SEARCH_EMBEDDINGS_BASE_URL")) != "" {
		return "", errors.New("unset KATA_SEARCH_EMBEDDINGS_BASE_URL, then rerun to enable built-in embeddings")
	}
	current, err := config.ReadSearchConfig()
	if err != nil {
		return "", fmt.Errorf("read search configuration, including KATA_SEARCH_EMBEDDINGS_* environment overrides: %w", err)
	}
	if run.keepProvider && current.Embeddings.Enabled() && !current.Embeddings.UsesLocal() {
		// A provider saved since the caller last looked takes effect on restart.
		if err := run.restart(ctx, func() error { return nil }); err != nil {
			return "", err
		}
		return "kept", nil
	}
	prepared, err := prepareLocalEmbeddingsConfig(ctx, current.Embeddings)
	if err != nil {
		return "", err
	}
	path, original, originalExists := prepared.path, prepared.original, prepared.originalExists
	proposed, next, index := prepared.proposed, prepared.next, prepared.index
	if index != "new" {
		if err := run.notice(fmt.Sprintf("Search index will be %s.", index)); err != nil {
			return "", err
		}
		if index == "rebuilt" {
			if err := run.notice("Daemons sharing embeddings need the same model and settings before reusing them."); err != nil {
				return "", err
			}
		}
	}
	confirmed := false
	confirm := func(ctx context.Context, size int64) (bool, error) {
		yes, err := run.confirm(ctx, size)
		confirmed = yes && err == nil
		return yes, err
	}
	if next.Embeddings.UsesLocal() {
		installed, err := installLocalEmbeddings(ctx, confirm, run.progress)
		if err != nil {
			return "", err
		}
		if !confirmed && current.Embeddings.Enabled() && !current.Embeddings.UsesLocal() {
			if _, err := confirm(ctx, 0); err != nil {
				return "", err
			}
		}
		if _, err := localEmbeddingsEndpoint(ctx, embedlocal.Options{SkipOllama: true, Executable: installed.Executable}); err != nil {
			return "", err
		}
	}
	applied := false
	err = run.restart(ctx, func() error {
		latest, err := os.ReadFile(path) // #nosec G304 -- same config snapshot read before installation
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read configuration before writing: %w", err)
		}
		if (err == nil) != originalExists || !bytes.Equal(latest, original) {
			return fmt.Errorf("configuration %q changed during setup; rerun the command", path)
		}
		if !bytes.Equal(original, proposed) {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := atomicfile.WriteFile(path, proposed, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink()); err != nil {
				return err
			}
		}
		applied = true
		return nil
	})
	if err != nil {
		if applied && !bytes.Equal(original, proposed) {
			latest, readErr := os.ReadFile(path) // #nosec G304 -- compare the snapshot before restoring it
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return "", errors.Join(err, readErr)
			}
			if readErr == nil && bytes.Equal(latest, proposed) {
				var restoreErr error
				if originalExists {
					restoreErr = atomicfile.WriteFile(path, original, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink())
				} else {
					restoreErr = os.Remove(path)
				}
				if restoreErr != nil {
					return "", errors.Join(err, restoreErr)
				}
				return "", fmt.Errorf("%w; configuration restored", err)
			}
		}
		return "", err
	}
	if next.Embeddings.UsesLocal() {
		if err := embedlocal.RecordChoice(); err != nil {
			slog.Warn("could not confirm that the shared search by meaning choice was saved", "err", err)
		}
	}
	return index, nil
}

// applySharedEmbeddingChoice derives the active provider without changing the config file.
func applySharedEmbeddingChoice(ctx context.Context, current config.EmbeddingsConfig, dsn string) (config.EmbeddingsConfig, error) {
	if current.Enabled() {
		return current, nil
	}
	backend, err := storeopen.BackendForDSN(dsn)
	if err != nil {
		return current, err
	}
	if backend != storeopen.BackendSQLite {
		return current, nil
	}
	if err := ctx.Err(); err != nil {
		return current, err
	}
	chosen, err := embedlocal.ChoiceRecorded()
	if err != nil || !chosen {
		return current, err
	}
	next := current
	next.Local = true
	next.Model = "nomic-embed-text"
	if localEmbeddingRecipeNeedsReset(current, current) {
		next.Dims = 768
		next.DocumentPrefix, next.DocumentSuffix = "", ""
		next.QueryPrefix, next.QuerySuffix = "", ""
		next.FingerprintSalt = ""
		next.RequestDimensions = false
	}
	return next, nil
}

func localEmbeddingRecipeNeedsReset(written, current config.EmbeddingsConfig) bool {
	return !written.LocalRecipeCompatible() || current.FingerprintSalt != "" && !current.UsesLocal()
}

type preparedLocalEmbeddingsConfig struct {
	path           string
	original       []byte
	originalExists bool
	proposed       []byte
	next           config.SearchConfig
	index          string
}

func prepareLocalEmbeddingsConfig(ctx context.Context, current config.EmbeddingsConfig) (preparedLocalEmbeddingsConfig, error) {
	var prepared preparedLocalEmbeddingsConfig
	var err error
	prepared.path, err = config.DaemonConfigPath()
	if err != nil {
		return prepared, err
	}
	prepared.original, prepared.originalExists, err = readLocalEmbeddingsConfig(prepared.path)
	if err != nil {
		return prepared, err
	}
	baseURL := ""
	if !current.Enabled() {
		endpoint, err := localEmbeddingsEndpoint(ctx, embedlocal.Options{})
		if err != nil && !errors.Is(err, embedlocal.ErrNotFound) {
			return prepared, err
		}
		if err == nil {
			builtIn, _ := localEmbeddingsEndpoint(ctx, embedlocal.Options{SkipOllama: true})
			if endpoint != builtIn {
				baseURL = endpoint
			}
		}
	}
	prepared.proposed, err = localEmbeddingsConfig(prepared.original, current, baseURL)
	if err != nil {
		return prepared, err
	}
	prepared.next, err = config.ParseSearchConfig(prepared.proposed)
	if err != nil {
		return prepared, fmt.Errorf("cannot enable built-in embeddings; check KATA_SEARCH_EMBEDDINGS_* environment overrides: %w", err)
	}
	prepared.index, err = localEmbeddingsIndex(current, prepared.next.Embeddings)
	return prepared, err
}

// A dangling symlink reads as absent, but writing through it would create its target.
func readLocalEmbeddingsConfig(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path) // #nosec G304 G703 -- read the operator-selected KATA_HOME configuration
	if !errors.Is(err, os.ErrNotExist) {
		return data, err == nil, err
	}
	if _, err := os.Lstat(path); err == nil { // #nosec G703 -- inspect the same operator-selected configuration path
		return nil, false, fmt.Errorf("configuration %q is a symlink to a missing file; create its target or remove the link, then rerun", path)
	}
	return []byte{}, false, nil
}

// Rewrite only the embeddings table, preserving surrounding bytes and its full-line comments.
func localEmbeddingsConfig(data []byte, current config.EmbeddingsConfig, baseURL string) ([]byte, error) {
	bom := []byte{0xef, 0xbb, 0xbf}
	hasBOM := bytes.HasPrefix(data, bom)
	if hasBOM {
		data = data[len(bom):]
	}
	written, err := config.ParseSearchFile(data)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	_, _ = toml.Decode(string(data), &document) // ParseSearchFile accepted the same text
	search, _ := document["search"].(map[string]any)
	fields, ok := search["embeddings"].(map[string]any)
	if !ok {
		fields = map[string]any{}
	}
	previous := maps.Clone(fields)
	delete(fields, "base_url")
	// Environment overrides don't persist, so the written recipe must suit the built-in server.
	if localEmbeddingRecipeNeedsReset(written.Embeddings, current) {
		for _, key := range []string{"model", "dims", "document_prefix", "document_suffix", "query_prefix", "query_suffix", "fingerprint_salt", "request_dimensions"} {
			delete(fields, key)
		}
		fields["model"] = "nomic-embed-text"
		fields["dims"] = 768
	}
	fields["local"] = true
	if baseURL != "" {
		delete(fields, "local")
		fields["base_url"] = baseURL
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(fields); err != nil {
		return nil, err
	}
	for key, value := range previous {
		next, retained := fields[key]
		if key == "local" || retained && fmt.Sprint(value) == fmt.Sprint(next) {
			delete(previous, key)
		}
	}
	if len(previous) > 0 {
		var saved bytes.Buffer
		if err := toml.NewEncoder(&saved).Encode(previous); err != nil {
			return nil, err
		}
		encoded.WriteString("# " + strings.ReplaceAll(strings.TrimSuffix(saved.String(), "\n"), "\n", "\n# ") + "\n")
	}
	type edit struct {
		start, end int
		text       []byte
	}
	var edits []edit
	var table []string
	inserted, rootSearch := false, false
	parser := unstable.Parser{KeepComments: true}
	parser.Reset(data)
	for parser.NextExpression() {
		node := parser.Expression()
		if node.Kind == unstable.Table || node.Kind == unstable.ArrayTable {
			table = localTOMLKey(node)
			if slices.Equal(table, []string{"search", "embeddings"}) && !inserted {
				key := node.Key()
				key.Next()
				end := int(key.Node().Raw.Offset)
				if newline := bytes.IndexByte(data[end:], '\n'); newline >= 0 {
					end += newline + 1
				} else {
					end = len(data)
				}
				text := bytes.Clone(encoded.Bytes())
				if end > 0 && data[end-1] != '\n' {
					text = append([]byte("\n"), text...)
				}
				edits = append(edits, edit{end, end, text})
				inserted = true
			}
		}
		if node.Kind != unstable.KeyValue {
			continue
		}
		path := append(slices.Clone(table), localTOMLKey(node)...)
		isRoot := slices.Equal(path, []string{"search"}) && node.Value().Kind == unstable.InlineTable
		if !isRoot && (len(path) < 2 || path[0] != "search" || path[1] != "embeddings") {
			continue
		}
		rootSearch = rootSearch || isRoot
		var comments bytes.Buffer
		localTOMLComments(node.Value(), &comments)
		start := int(node.Raw.Offset)
		end := start + int(node.Raw.Length)
		if trailing := node.Next(); trailing != nil && trailing.Kind == unstable.Comment {
			commentEnd := int(trailing.Raw.Offset + trailing.Raw.Length)
			comments.Write(data[end:commentEnd])
			comments.WriteByte('\n')
			end = commentEnd
		}
		if newline := bytes.IndexByte(data[end:], '\n'); newline >= 0 {
			end += newline + 1
		}
		start = bytes.LastIndexByte(data[:start], '\n') + 1
		edits = append(edits, edit{start, end, comments.Bytes()})
	}
	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("parse config text: %w", err)
	}
	var result bytes.Buffer
	position := 0
	for _, edit := range edits {
		result.Write(data[position:edit.start])
		result.Write(edit.text)
		position = edit.end
	}
	result.Write(data[position:])
	if !inserted {
		if result.Len() > 0 && result.Bytes()[result.Len()-1] != '\n' {
			result.WriteByte('\n')
		}
		if rootSearch {
			other := make(map[string]any)
			for key, value := range search {
				if key != "embeddings" {
					other[key] = value
				}
			}
			if len(other) > 0 {
				if err := toml.NewEncoder(&result).Encode(map[string]any{"search": other}); err != nil {
					return nil, err
				}
			}
		}
		result.WriteString("[search.embeddings]\n")
		result.Write(encoded.Bytes())
	}
	var rewritten map[string]any
	if _, err := toml.Decode(result.String(), &rewritten); err != nil {
		return nil, fmt.Errorf("rewrite config text: %w", err)
	}
	for _, doc := range []map[string]any{document, rewritten} {
		if search, ok := doc["search"].(map[string]any); ok {
			delete(search, "embeddings")
			if len(search) == 0 {
				delete(doc, "search")
			}
		}
	}
	if !reflect.DeepEqual(document, rewritten) {
		return nil, errors.New("cannot rewrite config text without changing other settings")
	}
	if hasBOM {
		return append(bom, result.Bytes()...), nil
	}
	return result.Bytes(), nil
}

func localTOMLKey(node *unstable.Node) []string {
	var key []string
	it := node.Key()
	for it.Next() {
		key = append(key, string(it.Node().Data))
	}
	return key
}

func localTOMLComments(node *unstable.Node, out *bytes.Buffer) {
	if node.Kind == unstable.Comment {
		out.Write(node.Data)
		out.WriteByte('\n')
	}
	children := node.Children()
	for children.Next() {
		localTOMLComments(children.Node(), out)
	}
}

// localEmbeddingsSetup owns one setup job for the daemon's lifetime. start
// registers work with the daemon worker group using its shutdown context.
type localEmbeddingsSetup struct {
	embeddings  config.EmbeddingsConfig
	idle        daemon.IdleForegroundAdmission
	start       func(func(context.Context)) bool
	vectorReady func(context.Context) error
	restart     func() error
	mu          sync.Mutex
	job         *api.LocalEmbeddingsJob
	restarting  bool
}

func (s *localEmbeddingsSetup) Status(ctx context.Context) (api.LocalEmbeddingsStatus, error) {
	status := api.LocalEmbeddingsStatus{Provider: "none"}
	if s.embeddings.UsesLocal() {
		status.Provider = "built_in"
	} else if s.embeddings.Enabled() {
		status.Provider = "other"
		// Kit probes local availability; it doesn't classify configured endpoints.
		endpoint, err := url.Parse(s.embeddings.BaseURL)
		if err == nil && endpoint.Port() == "11434" && (endpoint.Hostname() == "localhost" || net.ParseIP(endpoint.Hostname()).IsLoopback()) {
			status.Provider = "ollama"
		}
	}
	// Only an unconfigured daemon depends on the shared installation files.
	if status.Provider == "none" {
		inspected, err := localEmbeddingsInspect(ctx, embedlocal.Options{})
		if err != nil {
			return status, err
		}
		status.DownloadBytes, status.OllamaAvailable = inspected.DownloadBytes, inspected.OllamaAvailable
	}
	s.mu.Lock()
	if s.job != nil {
		job := *s.job
		status.Job = &job
	}
	status.CanRestart, status.RestartInProgress = s.restart != nil && s.job != nil && s.job.State == "restart" && !s.restarting, s.restarting
	s.mu.Unlock()
	status.CanChange = status.Provider == "none" && (status.Job == nil || status.Job.State == "failed")
	return status, nil
}

func (s *localEmbeddingsSetup) Restart(_ context.Context) error {
	s.mu.Lock()
	if s.restarting {
		s.mu.Unlock()
		return nil
	}
	if s.restart == nil || s.job == nil || s.job.State != "restart" {
		s.mu.Unlock()
		return daemon.ErrLocalEmbeddingsBusy
	}
	s.restarting, s.job.Error = true, ""
	s.mu.Unlock()
	if err := s.restart(); err != nil {
		s.mu.Lock()
		s.restarting, s.job.Error = false, err.Error()
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *localEmbeddingsSetup) restartExited(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.restarting {
		s.restarting, s.job.Error = false, err.Error()
	}
}

func (s *localEmbeddingsSetup) TurnOn(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.State != "failed" {
		return daemon.ErrLocalEmbeddingsBusy
	}
	if s.embeddings.Enabled() {
		return daemon.ErrLocalEmbeddingsConfigured
	}
	var release func()
	if s.idle != nil {
		lease, ok := s.idle.TryForeground()
		if !ok {
			return errors.New("daemon is shutting down")
		}
		release = lease.Release
	}
	s.job = &api.LocalEmbeddingsJob{State: "downloading"}
	if !s.start(func(ctx context.Context) {
		if release != nil {
			defer release()
		}
		var err error
		if s.vectorReady != nil {
			err = s.vectorReady(ctx)
		}
		if err == nil {
			_, err = enableLocalEmbeddings(ctx, localEmbeddingsRun{
				confirm:      func(context.Context, int64) (bool, error) { return true, nil },
				notice:       func(string) error { return nil },
				keepProvider: true,
				progress: func(downloaded, total int64) {
					s.mu.Lock()
					s.job.Downloaded, s.job.Total = downloaded, total
					s.mu.Unlock()
				},
				restart: func(_ context.Context, apply func() error) error {
					if err := apply(); err != nil {
						return err
					}
					s.mu.Lock()
					s.job.State = "restart"
					s.mu.Unlock()
					return nil
				},
			})
		}
		if err != nil {
			s.mu.Lock()
			s.job.State, s.job.Error = "failed", err.Error()
			s.mu.Unlock()
		}
	}) {
		if release != nil {
			release()
		}
		s.job = nil
		return errors.New("daemon is shutting down")
	}
	return nil
}

func localEmbeddingsIndex(current, next config.EmbeddingsConfig) (string, error) {
	before, _, err := preflightEmbeddingStartup(current, "")
	if err != nil {
		return "", err
	}
	after, _, err := preflightEmbeddingStartup(next, "")
	if err != nil {
		return "", err
	}
	if before == nil || after == nil {
		return "new", nil
	}
	oldRecipe, err := before.ArtifactIdentity("", "", "")
	if err != nil {
		return "", err
	}
	newRecipe, err := after.ArtifactIdentity("", "", "")
	if err != nil {
		return "", err
	}
	if before.Generation().Fingerprint() == after.Generation().Fingerprint() && oldRecipe == newRecipe {
		return "kept", nil
	}
	return "rebuilt", nil
}
