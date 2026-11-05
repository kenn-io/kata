package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/BurntSushi/toml"
	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/embedlocal"
)

var localEmbeddingsEndpoint = embedlocal.Endpoint
var localEmbeddingsInstall = embedlocal.Install

func resolveLocalEmbeddings(ctx context.Context, confirm func(context.Context, int64) (bool, error)) (string, error) {
	endpoint, err := localEmbeddingsEndpoint(ctx, embedlocal.Options{SkipOllama: true})
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return endpoint, err
	}
	dir, err := embedlocal.DefaultDir()
	if err != nil {
		return "", err
	}
	installed, err := localEmbeddingsInstall(ctx, embedlocal.InstallOptions{DataDir: dir, Confirm: confirm})
	if err != nil {
		return "", err
	}
	return localEmbeddingsEndpoint(ctx, embedlocal.Options{SkipOllama: true, Executable: installed.Executable})
}

func daemonLocalEmbeddingsCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "local-embeddings", Short: "enable search by meaning with the built-in server", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requireCurrentHomeDaemonCommand(); err != nil {
			return err
		}
		path, err := config.DaemonConfigPath()
		if err != nil {
			return err
		}
		original, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
		originalExists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		current, err := config.ReadSearchConfig()
		if err != nil {
			return fmt.Errorf("read search configuration, including KATA_SEARCH_EMBEDDINGS_* environment overrides: %w", err)
		}
		proposed, err := localEmbeddingsConfig(original, current.Embeddings)
		if err != nil {
			return err
		}
		next, err := config.ParseSearchConfig(proposed)
		if err != nil {
			return fmt.Errorf("cannot enable built-in embeddings; check KATA_SEARCH_EMBEDDINGS_* environment overrides: %w", err)
		}
		before, _, err := preflightEmbeddingStartup(current.Embeddings, "")
		if err != nil {
			return err
		}
		after, _, err := preflightEmbeddingStartup(next.Embeddings, "")
		if err != nil {
			return err
		}
		index := "new"
		if before != nil {
			oldRecipe, err := before.ArtifactIdentity("", "", "")
			if err != nil {
				return err
			}
			newRecipe, err := after.ArtifactIdentity("", "", "")
			if err != nil {
				return err
			}
			index = "rebuilt"
			if before.Generation().Fingerprint() == after.Generation().Fingerprint() && oldRecipe == newRecipe {
				index = "kept"
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Search index will be %s.\n", index); err != nil {
				return err
			}
			if index == "rebuilt" {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "If this daemon shares embeddings, replicas need the same recipe before they can reuse its vectors."); err != nil {
					return err
				}
			}
		}
		confirmed := false
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
			confirmed = true
			return true, nil
		}
		if _, err := resolveLocalEmbeddings(cmd.Context(), confirm); err != nil {
			return err
		}
		if !confirmed && before != nil && !current.Embeddings.Local {
			if _, err := confirm(cmd.Context(), 0); err != nil {
				return err
			}
		}
		ns, err := daemonruntime.NewNamespace()
		if err != nil {
			return err
		}
		running, release, err := lockLiveDaemons(cmd.Context(), ns, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		defer release()
		if err := requireRestartable(running); err != nil {
			return err
		}
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
			if err := atomicfile.WriteFile(path, proposed, atomicfile.WithPreserveMode()); err != nil {
				return err
			}
		}
		if _, _, err := restartRunningDaemons(cmd.Context(), ns, running, nil); err != nil {
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
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Built-in embeddings enabled, index %s. Check indexing progress with kata health --json.\n", index)
		}
		return err
	}}
	cmd.Flags().BoolVar(&yes, "yes", false, "approve the download and configuration change")
	return cmd
}

// Rewrite only the embeddings table, preserving surrounding bytes and its full-line comments.
func localEmbeddingsConfig(data []byte, current config.EmbeddingsConfig) ([]byte, error) {
	var document map[string]any
	if _, err := toml.Decode(string(data), &document); err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if search, ok := document["search"].(map[string]any); ok {
		if existing, ok := search["embeddings"].(map[string]any); ok {
			fields = existing
		}
	}
	for _, key := range []string{"base_url", "api_key", "api_key_file", "api_key_env"} {
		delete(fields, key)
	}
	if !current.LocalRecipeCompatible() {
		for _, key := range []string{"model", "dims", "document_prefix", "document_suffix", "query_prefix", "query_suffix", "fingerprint_salt", "request_dimensions"} {
			delete(fields, key)
		}
		fields["model"] = "nomic-embed-text"
		fields["dims"] = 768
	}
	fields["local"] = true
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(fields); err != nil {
		return nil, err
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
			search := document["search"].(map[string]any)
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
