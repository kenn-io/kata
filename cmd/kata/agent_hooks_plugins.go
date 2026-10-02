package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const pluginAgentHookMetadataPrefix = "// kata-native-plugin "

type pluginAgentHookMetadata struct {
	Format         string `json:"format"`
	Version        int    `json:"version"`
	Agent          string `json:"agent"`
	API            string `json:"api"`
	Scope          string `json:"scope"`
	Workspace      string `json:"workspace"`
	Executable     string `json:"executable"`
	Source         string `json:"source"`
	SourceSet      bool   `json:"sourceSet"`
	Contract       bool   `json:"contract"`
	Attention      bool   `json:"attention"`
	ConfigPath     string `json:"configPath"`
	ConfigOwned    bool   `json:"configOwned"`
	ConfigProperty bool   `json:"configProperty"`
}

// nativePluginFileURL accepts a directory with native separators normalized to
// slashes. UNC servers belong in the URL authority; drive paths need a leading
// slash so Node's native file URL parser can resolve them on Windows.
func nativePluginFileURL(directory string) string {
	u := url.URL{Scheme: "file", Path: directory}
	if remainder, unc := strings.CutPrefix(directory, "//"); unc {
		host, tail, _ := strings.Cut(remainder, "/")
		u.Host, u.Path = host, "/"+tail
	} else if len(directory) >= 3 && directory[1] == ':' && directory[2] == '/' {
		u.Path = "/" + directory
	}
	return u.String()
}

func planAmpAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	return planPluginAgentHooks(opts, "amp", remove)
}
func planOpenCodeAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	return planPluginAgentHooks(opts, "opencode", remove)
}

func validatePluginAgentHookTypeScript(readFile func(string) ([]byte, bool, error), paths ...string) error {
	for _, path := range paths {
		sibling := strings.TrimSuffix(path, ".js") + ".ts"
		if _, exists, err := readFile(sibling); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("preserving authored TypeScript plugin %q; move it aside before installing", sibling)
		}
	}
	return nil
}

func planPluginAgentHooks(opts nativeAgentHookOptions, agent string, remove bool) (nativeAgentHookPlan, error) {
	plan := nativeAgentHookPlan{}
	// Every inspected native loader input is a transaction precondition, even
	// when its bytes are unchanged or it is absent. Cache the first read so
	// selection, ownership checks and explicit writes share the same preimage.
	var snapshots []nativeAgentHookChange
	readSnapshot := func(path string) ([]byte, bool, error) {
		for _, snapshot := range snapshots {
			if snapshot.Path == path {
				return snapshot.Original, snapshot.OriginalExists, nil
			}
		}
		data, exists, e := readNativeAgentHookFile(path)
		if e == nil {
			snapshots = append(snapshots, nativeAgentHookChange{Path: path, Original: data, OriginalExists: exists, Content: data, Remove: !exists})
		}
		return data, exists, e
	}
	finalize := func() nativeAgentHookPlan {
		if len(plan.Changes) == 0 {
			return plan
		}
		for _, snapshot := range snapshots {
			index := slices.IndexFunc(plan.Changes, func(change nativeAgentHookChange) bool { return change.Path == snapshot.Path })
			if index < 0 {
				plan.Changes = append(plan.Changes, snapshot)
			} else {
				plan.Changes[index].Original = snapshot.Original
				plan.Changes[index].OriginalExists = snapshot.OriginalExists
			}
		}
		return plan
	}
	scope := opts.Scope
	if scope == "" {
		scope = "user"
	}
	if scope != "user" && scope != "project" {
		return plan, fmt.Errorf("unsupported %s scope %q", agent, scope)
	}
	if agent == "amp" && (opts.API != "" || opts.ConfigPath != "") {
		return plan, fmt.Errorf("Amp auto-discovered plugins do not support --api or --config") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
	}
	workspace := ""
	var err error
	configRoot := os.Getenv("XDG_CONFIG_HOME")
	if configRoot == "" {
		configRoot = filepath.Join(opts.Home, ".config")
	}
	if !filepath.IsAbs(configRoot) {
		return plan, fmt.Errorf("native plugin config home must be absolute")
	}
	root := filepath.Join(configRoot, agent)
	if scope == "project" {
		workspace, err = filepath.Abs(opts.Dir)
		if err != nil {
			return plan, err
		}
		root = filepath.Join(workspace, "."+agent)
	}
	if scope == "project" && opts.ConfigPath != "" {
		return plan, fmt.Errorf("project OpenCode plugins are auto-discovered; --config is unsupported")
	}
	singleRoot := filepath.Join(root, "plugins")
	if agent == "opencode" {
		singleRoot = filepath.Join(root, "plugin")
	}
	file := filepath.Join(singleRoot, "kata-"+scope+".js")
	directoryFile := filepath.Join(root, "plugins", "kata-"+scope, "index.js")
	api := opts.API
	if agent == "amp" {
		api = "amp"
	} else if api != "" && api != "v1" && api != "v2" {
		return plan, fmt.Errorf("unsupported OpenCode API %q; choose v1 or v2", api)
	}
	// Until runtime selection resolves the API, both discovery layouts are
	// installation inputs. Check them before any fresh-install API default.
	if !remove && agent == "opencode" && api == "" {
		if err := validatePluginAgentHookTypeScript(readSnapshot, file, directoryFile); err != nil {
			return plan, err
		}
	}
	// Find the existing owned selection before defaulting a new installation.
	data, exists, err := readSnapshot(file)
	if err != nil {
		return plan, err
	}
	directoryData, directoryExists, err := readSnapshot(directoryFile)
	if err != nil {
		return plan, err
	}
	if exists && directoryExists {
		return plan, fmt.Errorf("preserving conflicting native plugin entries; move one aside before installing")
	}
	if directoryExists {
		file = directoryFile
		data = directoryData
		exists = true
	}
	recovered := false
	var recoveryMeta pluginAgentHookMetadata
	var recoveryPackage []byte
	if agent == "opencode" && !exists {
		manifest, present, e := readSnapshot(filepath.Join(filepath.Dir(directoryFile), "package.json"))
		if e != nil {
			return plan, e
		}
		if present {
			recoveryPackage = manifest
			recoveryMeta, e = parsePluginAgentHookPackage(manifest)
			if e != nil {
				return plan, fmt.Errorf("preserving authored plugin package: %w", e)
			}
			recovered = true
		}
	}
	meta := pluginAgentHookMetadata{Format: "kata-plugin", Version: 1, Agent: agent, API: api, Scope: scope, Workspace: workspace, Executable: opts.Executable}
	if recovered {
		meta = recoveryMeta
	}
	if exists || recovered {
		if exists {
			meta, err = parsePluginAgentHookMetadata(data)
			if err != nil {
				return plan, fmt.Errorf("preserving native plugin %q: %w", file, err)
			}
		}
		if meta.Agent != agent || meta.Scope != scope || meta.Workspace != workspace {
			return plan, fmt.Errorf("preserving native plugin: scope or workspace differs")
		}
		if api != "" && api != meta.API {
			return plan, fmt.Errorf("preserving installed OpenCode %s plugin; uninstall it before selecting %s", meta.API, api)
		}
		plan.CurrentContract = exists && meta.Contract
		plan.CurrentAttentionStart = exists && meta.Attention
		plan.CurrentAPI = meta.API
	}
	if meta.API == "" {
		meta.API = "v2"
	}
	if meta.API == "v2" {
		file = directoryFile
	}
	plan.Path = file
	if !remove {
		if err := validatePluginAgentHookTypeScript(readSnapshot, file); err != nil {
			return plan, err
		}
	}
	plan.Warnings = []string{"Offline status reports configured files, not runtime loading. Reload plugins or restart the agent after installation.", "No native terminal attention event is available; use launcher process-exit cleanup for attention end."}
	if agent == "amp" {
		plan.Warnings = append(plan.Warnings, "Hidden contract messages require @ampcode/plugin 0.0.0-20260526003443-g9106a62 or newer; no separate CLI version floor is established. Native event shell permissions must permit workspace discovery.")
	} else if meta.API == "v1" {
		plan.Warnings = append(plan.Warnings, "Requires OpenCode v1 1.0.154 or newer. The v1 plugin cannot run in OpenCode v2.")
	} else {
		plan.Warnings = append(plan.Warnings, "Requires OpenCode v2 and @opencode/plugin 2.0.0 or newer. Provision the plugin's declared dependencies with Bun before loading; v1 hooks are incompatible.")
	}
	if recovered {
		plan.Warnings = append(plan.Warnings, "Owned plugin entrypoint is missing; package metadata permits recovery, but no contract or attention start is currently configured.")
	}
	originalMeta := meta
	if !remove {
		if opts.Executable != "" {
			meta.Executable = opts.Executable
		}
		if meta.Executable == "" {
			meta.Executable = "kata"
		}
		if opts.SourceSet {
			meta.Source = opts.Source
			meta.SourceSet = true
		}
		meta.Contract = meta.Contract || opts.Contract
		meta.Attention = meta.Attention || opts.Attention
	} else {
		if opts.Contract {
			meta.Contract = false
		}
		if opts.Attention {
			meta.Attention = false
		}
	}
	active := meta.Contract || meta.Attention
	if agent == "amp" {
		changes, warning, e := planAmpAgentHookScopeIndex(opts, meta, file, remove)
		if e != nil {
			return plan, e
		}
		plan.Changes = append(plan.Changes, changes...)
		plan.Warnings = append(plan.Warnings, "Amp runs plugins in separate processes. Kata user and project installs cannot coexist; choose one scope for the complete bundle. Multiple project-only installs are supported.")
		if warning != "" {
			plan.Warnings = append(plan.Warnings, warning)
		}
	}
	if meta.API == "v1" && opts.ConfigPath != "" {
		return plan, fmt.Errorf("OpenCode v1 uses auto-discovery; --config is unsupported")
	}
	if meta.API == "v2" {
		manifestPath := filepath.Join(filepath.Dir(file), "package.json")
		manifest, manifestExists := recoveryPackage, recovered
		if !manifestExists {
			var e error
			manifest, manifestExists, e = readSnapshot(manifestPath)
			if e != nil {
				return plan, e
			}
		}
		if manifestExists {
			packageMeta, e := parsePluginAgentHookPackage(manifest)
			if e != nil || packageMeta != originalMeta {
				return plan, fmt.Errorf("preserving authored plugin package %q", manifestPath)
			}
		}
		if scope == "user" {
			cfg := meta.ConfigPath
			if cfg == "" {
				cfg = opts.ConfigPath
				if cfg == "" {
					cfg = filepath.Join(root, "opencode.json")
					_, jsonExists, e := readSnapshot(cfg)
					if e != nil {
						return plan, e
					}
					jsonc := filepath.Join(root, "opencode.jsonc")
					_, jsoncExists, e := readSnapshot(jsonc)
					if e != nil {
						return plan, e
					}
					if jsonExists && jsoncExists {
						return plan, fmt.Errorf("both OpenCode user config formats exist; select the active file with --config")
					}
					if jsoncExists {
						cfg = jsonc
					}
				}
			}
			if opts.ConfigPath != "" && opts.ConfigPath != cfg {
				return plan, fmt.Errorf("preserving native plugin registered through %q; uninstall before changing --config", cfg)
			}
			cfg, err = filepath.Abs(cfg)
			if err != nil {
				return plan, err
			}
			original, cfgExists, e := readSnapshot(cfg)
			if e != nil {
				return plan, e
			}
			registration := nativePluginFileURL(filepath.ToSlash(filepath.Dir(file)))
			next, present, propertyAdded, e := editPluginAgentHookConfig(original, registration, active && !remove, !active && meta.ConfigOwned)
			if e != nil {
				return plan, fmt.Errorf("preserving OpenCode config %q: %w", cfg, e)
			}
			if exists && !present {
				plan.CurrentContract = false
				plan.CurrentAttentionStart = false
				plan.Warnings = append(plan.Warnings, "Owned user plugin is absent from the native plugins list; reinstall to restore registration.")
			}
			if active && !remove {
				meta.ConfigPath = cfg
				if !present {
					meta.ConfigOwned = true
					meta.ConfigProperty = propertyAdded
				}
			}
			// Remove a newly introduced empty property without touching authored fields.
			if !active && meta.ConfigOwned && meta.ConfigProperty {
				next, e = removeEmptyPluginAgentHookProperty(next)
				if e != nil {
					return plan, e
				}
			}
			if !bytes.Equal(original, next) {
				plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: cfg, Original: original, OriginalExists: cfgExists, Content: next})
			}
		}
		if active && (!remove || opts.Contract || opts.Attention) {
			content, e := generatePluginAgentHookPackage(meta)
			if e != nil {
				return plan, e
			}
			plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: manifestPath, Original: manifest, OriginalExists: manifestExists, Content: content})
		} else if !active && (exists || recovered) && manifestExists {
			plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: manifestPath, Original: manifest, OriginalExists: true, Remove: true})
		}
	}
	plan.Contract = meta.Contract
	plan.AttentionStart = meta.Attention
	if !exists && remove {
		if len(plan.Changes) > 0 {
			plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: file, OriginalExists: false, Remove: true})
		}
		return finalize(), nil
	}
	change := nativeAgentHookChange{Path: file, Original: data, OriginalExists: exists}
	if !active {
		if exists {
			change.Remove = true
			plan.Changes = append(plan.Changes, change)
		}
		return finalize(), nil
	}
	if remove && !opts.Contract && !opts.Attention {
		change.Content = data
		plan.Changes = append(plan.Changes, change)
		return finalize(), nil
	}
	change.Content, err = generatePluginAgentHooks(meta)
	if err != nil {
		return plan, err
	}
	plan.Changes = append(plan.Changes, change)
	return finalize(), nil
}

// The scope index records only product-owned adapter paths. It is not a
// repository sweep or a runtime lease: publication uses the same snapshots as
// the adapter, and no credentials or issue references are stored here.
type ampAgentHookScopeIndex struct {
	Format   string   `json:"format"`
	Version  int      `json:"version"`
	Projects []string `json:"projects"`
}

func planAmpAgentHookScopeIndex(opts nativeAgentHookOptions, meta pluginAgentHookMetadata, target string, remove bool) ([]nativeAgentHookChange, string, error) {
	configRoot := os.Getenv("XDG_CONFIG_HOME")
	if configRoot == "" {
		configRoot = filepath.Join(opts.Home, ".config")
	}
	indexPath := filepath.Join(configRoot, "amp", "kata-agent-hook-scopes.json")
	original, indexExists, err := readNativeAgentHookFile(indexPath)
	if err != nil {
		return nil, "", err
	}
	index := ampAgentHookScopeIndex{Format: "kata-amp-scopes", Version: 1, Projects: []string{}}
	if indexExists {
		if err = json.Unmarshal(original, &index, json.RejectUnknownMembers(true)); err != nil {
			return nil, "", fmt.Errorf("preserving Amp scope index: %w", err)
		}
		encoded, _ := json.Marshal(index, nativeAgentHookOwnedJSONOptions, jsontext.WithIndent("  "))
		encoded = append(encoded, '\n')
		if index.Format != "kata-amp-scopes" || index.Version != 1 || !bytes.Equal(original, encoded) {
			return nil, "", fmt.Errorf("preserving edited or unrecognized Amp scope index %q", indexPath)
		}
	}
	mutate := !remove || opts.Contract || opts.Attention
	active := meta.Contract || meta.Attention
	projects := []string{}
	var changes []nativeAgentHookChange
	liveProject := false
	seen := map[string]bool{}
	for _, path := range index.Projects {
		if !filepath.IsAbs(path) || filepath.Base(path) != "kata-project.js" || seen[path] {
			return nil, "", fmt.Errorf("preserving invalid Amp project scope record")
		}
		seen[path] = true
		if path == target && meta.Scope == "project" {
			if active {
				projects = append(projects, path)
			}
			continue
		}
		data, exists, e := readNativeAgentHookFile(path)
		if e != nil {
			return nil, "", e
		}
		if exists {
			previous, e := parsePluginAgentHookMetadata(data)
			if e != nil || previous.Agent != "amp" || previous.Scope != "project" {
				return nil, "", fmt.Errorf("preserving edited or authored tracked Amp project artifact %q", path)
			}
			projects = append(projects, path)
			liveProject = liveProject || previous.Contract || previous.Attention
		}
		// Include even missing tracked artifacts as constraints. A concurrent
		// project creation cannot slip past a global install's missing check.
		changes = append(changes, nativeAgentHookChange{Path: path, Original: data, OriginalExists: exists, Content: data, Remove: !exists})
	}
	if meta.Scope == "project" && active && !seen[target] {
		projects = append(projects, target)
	}
	userPath := filepath.Join(configRoot, "amp", "plugins", "kata-user.js")
	userData, userExists, e := readNativeAgentHookFile(userPath)
	if e != nil {
		return nil, "", e
	}
	userActive := false
	if meta.Scope != "user" && userExists {
		previous, e := parsePluginAgentHookMetadata(userData)
		if e != nil || previous.Agent != "amp" || previous.Scope != "user" {
			return nil, "", fmt.Errorf("preserving authored Amp user artifact %q", userPath)
		}
		userActive = previous.Contract || previous.Attention
	}
	conflict := (meta.Scope == "user" && active && liveProject) || (meta.Scope == "project" && active && userActive)
	warning := ""
	if conflict {
		if mutate {
			return nil, "", fmt.Errorf("Amp plugins run in separate processes: uninstall the other Kata scope with --attention before installing the complete bundle in one scope") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
		}
		warning = "Conflicting owned Amp scopes are configured; native per-capability precedence is unavailable. Remove one complete bundle with uninstall --attention."
	}
	if meta.Scope == "project" {
		changes = append(changes, nativeAgentHookChange{Path: userPath, Original: userData, OriginalExists: userExists, Content: userData, Remove: !userExists})
	}
	// Status inspects preimages only; it never reconciles or creates the index.
	if !mutate {
		return nil, warning, nil
	}
	slices.Sort(projects)
	index.Projects = projects
	next, err := json.Marshal(index, nativeAgentHookOwnedJSONOptions, jsontext.WithIndent("  "))
	if err != nil {
		return nil, "", err
	}
	next = append(next, '\n')
	if !indexExists && len(projects) == 0 {
		changes = append(changes, nativeAgentHookChange{Path: indexPath, Original: original, OriginalExists: false, Remove: true})
		return changes, warning, nil
	}
	changes = append(changes, nativeAgentHookChange{Path: indexPath, Original: original, OriginalExists: indexExists, Content: next})
	return changes, warning, nil
}

var pluginAgentHookPackage = []byte("{\n  \"name\": \"kata-native-agent-hooks\",\n  \"version\": \"1.0.0\",\n  \"type\": \"module\",\n  \"exports\": \"./index.js\",\n  \"dependencies\": {\"@opencode/plugin\": \"^2.0.0\"}\n}\n")

// The package independently retains config ownership if index.js is missing.
// Its digest covers exact package bytes, excluding only the digest member.
func generatePluginAgentHookPackage(meta pluginAgentHookMetadata) ([]byte, error) {
	encoded, err := json.Marshal(meta, nativeAgentHookOwnedJSONOptions)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(pluginAgentHookPackage[:len(pluginAgentHookPackage)-3])
	out.WriteString(",\n  \"kataNativeAgentHooks\": ")
	out.Write(encoded)
	out.WriteString("\n}\n")
	data := out.Bytes()
	return []byte("{\n  \"kataNativeAgentHooksSHA256\": \"" + nativeAgentHookDigest(data) + "\",\n" + string(data[2:])), nil
}
func parsePluginAgentHookPackage(data []byte) (pluginAgentHookMetadata, error) {
	var value struct {
		Metadata pluginAgentHookMetadata `json:"kataNativeAgentHooks"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value.Metadata, err
	}
	meta := value.Metadata
	unsigned, ok := bytes.CutPrefix(data, []byte("{\n  \"kataNativeAgentHooksSHA256\": \""))
	digest, remainder, found := bytes.Cut(unsigned, []byte("\",\n"))
	if !ok || !found || string(digest) != nativeAgentHookDigest(append([]byte("{\n"), remainder...)) {
		return meta, fmt.Errorf("generated plugin package was edited or has no ownership digest")
	}
	if meta.Format != "kata-plugin" || meta.Version != 1 || meta.Agent != "opencode" || meta.API != "v2" || meta.Executable == "" || (meta.Scope != "user" && meta.Scope != "project") {
		return meta, fmt.Errorf("authored or unrecognized plugin package metadata")
	}
	return meta, nil
}

func parsePluginAgentHookMetadata(data []byte) (pluginAgentHookMetadata, error) {
	var meta pluginAgentHookMetadata
	first, _, ok := bytes.Cut(data, []byte("\n"))
	if !ok || !bytes.HasPrefix(first, []byte(pluginAgentHookMetadataPrefix)) {
		return meta, fmt.Errorf("authored or unrecognized plugin; move it aside before installing")
	}
	if err := json.Unmarshal(bytes.TrimPrefix(first, []byte(pluginAgentHookMetadataPrefix)), &meta, json.RejectUnknownMembers(true)); err != nil {
		return meta, err
	}
	if meta.Format != "kata-plugin" || meta.Version != 1 || meta.Executable == "" || (meta.Scope != "user" && meta.Scope != "project") || (meta.Agent != "amp" && meta.Agent != "opencode") || (meta.Agent == "amp" && meta.API != "amp") || (meta.Agent == "opencode" && meta.API != "v1" && meta.API != "v2") {
		return meta, fmt.Errorf("unrecognized plugin metadata")
	}
	if !nativeAgentHookCodeUnedited(data) {
		return meta, fmt.Errorf("generated plugin was edited; preserve edits and move it aside before rerunning")
	}
	return meta, nil
}
func generatePluginAgentHooks(meta pluginAgentHookMetadata) ([]byte, error) {
	encoded, err := json.Marshal(meta, nativeAgentHookOwnedJSONOptions)
	if err != nil {
		return nil, err
	}
	body := pluginAgentHookCommonJS
	switch meta.API {
	case "amp":
		body += ampAgentHookJS
	case "v1":
		body += openCodeV1AgentHookJS
	case "v2":
		body += "\nimport { Plugin } from \"@opencode/plugin\";\n" + openCodeV2AgentHookJS
	default:
		return nil, fmt.Errorf("unsupported native plugin API")
	}
	return sealNativeAgentHookCode([]byte(pluginAgentHookMetadataPrefix + string(encoded) + "\nconst options = " + string(encoded) + ";\n" + body)), nil
}

// Strip only JSONC comments and trailing commas, retaining byte offsets for
// edits. The standard decoder validates syntax; authored bytes are never reencoded.
func pluginAgentHookJSONC(data []byte) ([]byte, error) {
	out := bytes.Clone(data)
	inString := false
	escaped := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c == '/' && i+1 < len(out) {
			if out[i+1] == '/' {
				out[i] = ' '
				i++
				for i < len(out) && out[i] != '\n' {
					out[i] = ' '
					i++
				}
				i--
				continue
			}
			if out[i+1] == '*' {
				out[i] = ' '
				i++
				out[i] = ' '
				closed := false
				for i+1 < len(out) {
					i++
					if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
						out[i] = ' '
						i++
						out[i] = ' '
						closed = true
						break
					}
					if out[i] != '\n' && out[i] != '\r' {
						out[i] = ' '
					}
				}
				if !closed {
					return nil, fmt.Errorf("unterminated JSONC comment")
				}
			}
		}
	}
	inString = false
	escaped = false
	for i, c := range out {
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(out) && strings.ContainsRune(" \t\r\n", rune(out[j])) {
				j++
			}
			if j < len(out) && (out[j] == ']' || out[j] == '}') {
				out[i] = ' '
			}
		}
	}
	if !jsontext.Value(out).IsValid() {
		return nil, fmt.Errorf("invalid JSON/JSONC configuration")
	}
	return out, nil
}

type pluginAgentHookJSONProperty struct {
	Key                    string
	Start, ValueStart, End int
}

func pluginAgentHookProperties(data []byte) ([]byte, []pluginAgentHookJSONProperty, error) {
	normalized, err := pluginAgentHookJSONC(data)
	if err != nil {
		return nil, nil, err
	}
	d := jsontext.NewDecoder(bytes.NewReader(normalized))
	token, err := d.ReadToken()
	if err != nil || token.Kind() != '{' {
		return nil, nil, fmt.Errorf("configuration must be an object")
	}
	var result []pluginAgentHookJSONProperty
	seen := map[string]bool{}
	for d.PeekKind() != '}' {
		start := int(d.InputOffset())
		for start < len(normalized) && normalized[start] != '"' {
			start++
		}
		keyToken, e := d.ReadToken()
		if e != nil {
			return nil, nil, e
		}
		key := keyToken.String()
		if seen[key] {
			return nil, nil, fmt.Errorf("duplicate configuration key %q", key)
		}
		seen[key] = true
		valueStart := int(d.InputOffset())
		for valueStart < len(normalized) && (normalized[valueStart] == ':' || strings.ContainsRune(" \t\r\n", rune(normalized[valueStart]))) {
			valueStart++
		}
		if _, e = d.ReadValue(); e != nil {
			return nil, nil, e
		}
		result = append(result, pluginAgentHookJSONProperty{key, start, valueStart, int(d.InputOffset())})
	}
	if _, err = d.ReadToken(); err != nil {
		return nil, nil, err
	}
	return normalized, result, nil
}
func editPluginAgentHookConfig(data []byte, registration string, add, remove bool) ([]byte, bool, bool, error) {
	if len(data) == 0 {
		if !add {
			return data, false, false, nil
		}
		data = []byte("{}\n")
	}
	normalized, properties, err := pluginAgentHookProperties(data)
	if err != nil {
		return nil, false, false, err
	}
	var prop *pluginAgentHookJSONProperty
	for i := range properties {
		if properties[i].Key == "plugins" {
			prop = &properties[i]
		}
	}
	encoded, _ := json.Marshal(registration, nativeAgentHookOwnedJSONOptions)
	if prop == nil {
		if !add {
			return data, false, false, nil
		}
		at := bytes.IndexByte(normalized, '{') + 1
		insert := append([]byte("\"plugins\":["), encoded...)
		insert = append(insert, ']')
		if len(properties) > 0 {
			insert = append(insert, ',')
		}
		return splicePluginAgentHookBytes(data, at, at, insert), false, true, nil
	}
	if normalized[prop.ValueStart] != '[' {
		return nil, false, false, fmt.Errorf("plugins must be an array")
	}
	d := jsontext.NewDecoder(bytes.NewReader(normalized[prop.ValueStart:prop.End]))
	_, _ = d.ReadToken()
	for d.PeekKind() != ']' {
		start := prop.ValueStart + int(d.InputOffset())
		for start < prop.End && (normalized[start] == ',' || strings.ContainsRune(" \t\r\n", rune(normalized[start]))) {
			start++
		}
		raw, err := d.ReadValue()
		if err != nil {
			return nil, false, false, err
		}
		end := prop.ValueStart + int(d.InputOffset())
		var entry string
		if json.Unmarshal(raw, &entry) != nil || entry != registration {
			continue
		}
		if !remove {
			return data, true, false, nil
		}
		right := end
		for right < prop.End && strings.ContainsRune(" \t\r\n", rune(normalized[right])) {
			right++
		}
		if right < prop.End && data[right] == ',' {
			end = right + 1
		} else {
			left := start - 1
			for left > prop.ValueStart && strings.ContainsRune(" \t\r\n", rune(normalized[left])) {
				left--
			}
			if data[left] == ',' {
				start = left
			}
		}
		return splicePluginAgentHookBytes(data, start, end, nil), true, false, nil
	}
	if !add {
		return data, false, false, nil
	}
	at := prop.ValueStart + 1
	insert := bytes.Clone(encoded)
	if d.InputOffset() > 1 {
		var values []jsontext.Value
		_ = json.Unmarshal(normalized[prop.ValueStart:prop.End], &values)
		if len(values) > 0 {
			insert = append(insert, ',')
		}
	}
	return splicePluginAgentHookBytes(data, at, at, insert), false, false, nil
}
func removeEmptyPluginAgentHookProperty(data []byte) ([]byte, error) {
	normalized, props, err := pluginAgentHookProperties(data)
	if err != nil {
		return nil, err
	}
	for _, prop := range props {
		if prop.Key != "plugins" {
			continue
		}
		var values []jsontext.Value
		if json.Unmarshal(normalized[prop.ValueStart:prop.End], &values) != nil || len(values) != 0 {
			return data, nil
		}
		start, end := prop.Start, prop.End
		right := end
		for right < len(data) && strings.ContainsRune(" \t\r\n", rune(normalized[right])) {
			right++
		}
		if right < len(data) && data[right] == ',' {
			end = right + 1
		} else {
			left := start - 1
			for left >= 0 && strings.ContainsRune(" \t\r\n", rune(normalized[left])) {
				left--
			}
			if left >= 0 && data[left] == ',' {
				start = left
			}
		}
		return splicePluginAgentHookBytes(data, start, end, nil), nil
	}
	return data, nil
}
func splicePluginAgentHookBytes(data []byte, start, end int, insert []byte) []byte {
	var out bytes.Buffer
	_, _ = out.Write(data[:start])
	_, _ = out.Write(insert)
	_, _ = out.Write(data[end:])
	return out.Bytes()
}

const pluginAgentHookCommonJS = `import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

// OpenCode hosts can load both scopes and reload registrations. Amp uses a
// single scope in its private plugin process. OpenCode capability precedence
// considers active registrations, never a file's existence.
const registry = globalThis[Symbol.for("kata." + options.agent + ".native.plugins.v1")] ||= {
  adapters: new Map(), starts: new Map(), contracts: new Set(), contractReads: new Map(), context: new WeakMap()
};
registry.contractReads ||= new Map();
function register(location = "", route = "") {
  const key = options.scope + ":" + options.workspace + ":" + location;
  const adapter = { options, key, route, enabled: { contract: options.contract, attention: options.attention } };
  registry.adapters.set(key, adapter);
  return adapter;
}
function selected(adapter, cwd, capability) {
  if (registry.adapters.get(adapter.key) !== adapter) return false;
  let winner;
  for (const candidate of registry.adapters.values()) {
    if (!candidate.enabled[capability] || (candidate.route && candidate.route !== cwd)) continue;
    if (candidate.options.scope === "project") {
      const relative = path.relative(candidate.options.workspace, cwd);
      if (relative === "" || (relative !== ".." && !relative.startsWith(".." + path.sep) && !path.isAbsolute(relative))) {
        if (!winner || winner.options.scope !== "project" || candidate.options.workspace.length > winner.options.workspace.length) winner = candidate;
      }
    } else if (!winner) winner = candidate;
  }
  return winner === adapter;
}
function nativeDirectory(value) {
  if (typeof value !== "string" || !path.isAbsolute(value)) throw new Error("native workspace unavailable");
  return path.resolve(value);
}
const maxOutputBytes = 16 * 1024 * 1024;
function run(args, cwd, env = process.env) {
  return new Promise((resolve, reject) => {
    const child = spawn(options.executable, ["--workspace", cwd, ...args],
      { cwd, env: { ...env }, timeout: 10000, windowsHide: true });
    const stdout = [];
    let outputBytes = 0;
    let outputExceeded = false;
    child.stdout.on("data", chunk => {
      if (outputExceeded) return;
      outputBytes += chunk.length;
      if (outputBytes > maxOutputBytes) {
        outputExceeded = true;
        stdout.length = 0;
        child.kill();
        return;
      }
      stdout.push(chunk);
    });
    // Drain stderr without buffering it; callers only consume successful stdout.
    child.stderr.resume();
    child.once("error", reject);
    child.once("close", (code, signal) => {
      if (outputExceeded) {
        reject(new Error("native command output exceeded 16 MiB"));
        return;
      }
      if (code !== 0) {
        reject(new Error("native command exited with " + (signal ?? "code " + code)));
        return;
      }
      resolve(Buffer.concat(stdout).toString("utf8"));
    });
    // Native bridges never require event stdin. Closing it also prevents a
    // malformed executable from indefinitely waiting for input.
    child.stdin?.end();
  });
}
async function start(adapter, id, cwd) {
  if (typeof id !== "string" || !id || !selected(adapter, cwd, "attention")) return;
  const key = cwd + "\u0000" + id;
  if (registry.starts.has(key)) return;
  const baseline = { id, cwd, ref: process.env.KATA_REF || "", hostPID: String(options.agent === "amp" ? process.ppid : process.pid), env: { ...process.env } };
  registry.starts.set(key, baseline);
  try { await run(["agent-hook", "attention-native", options.agent, "start", "--session", baseline.id,
    "--host-pid", baseline.hostPID, "--ref", baseline.ref], baseline.cwd, baseline.env); }
  catch { if (registry.starts.get(key) === baseline) registry.starts.delete(key); }
}
async function readContext(cwd, includeContract, onContractLoaded) {
  let contract = "";
  if (includeContract) {
    const args = ["agent-contract-hook"];
    if (options.sourceSet) args.push("--source", options.source);
    try {
      const sourceCwd = options.scope === "project" && options.sourceSet && !path.isAbsolute(options.source)
        ? options.workspace : cwd;
      const text = JSON.parse(await run(args, sourceCwd))?.hookSpecificOutput?.additionalContext;
      if (typeof text !== "string") throw new Error("invalid contract response");
      contract = text;
      onContractLoaded?.();
    } catch { contract = "Kata contract context is unavailable for this prompt."; }
  }
  let inbox = "No pending Kata inbox requests.";
  const recipient = process.env.KATA_INBOX_USER;
  if (recipient) {
    try { inbox = await run(["inbox", "--context", "--for", recipient], cwd) || inbox; }
    catch { inbox = "Kata inbox context is unavailable for this prompt."; }
  }
  // Amp messages remain in thread history. Explicit replacement makes cleared
  // or failed current requests authoritative over earlier message snapshots.
  return (contract ? "<kata_contract>\n" + contract + "\n</kata_contract>\n\n" : "") +
    "<kata_inbox>\nCurrent Kata inbox snapshot replaces earlier snapshots.\n" + inbox + "\n</kata_inbox>";
}
function appendContext(system, text, structured) {
  const old = registry.context.get(system);
  if (old) { const index = system.indexOf(old); if (index !== -1) system.splice(index, 1); }
  const item = structured ? { type: "text", text } : text;
  system.push(item);
  registry.context.set(system, item);
}
`
const ampAgentHookJS = `
export default function kataHooks(amp) {
  const adapter = register();
  async function workspace(ctx) {
    const uri = ctx.system && Reflect.get(ctx.system,"workspaceRoot");
    if (uri) return nativeDirectory(fileURLToPath(String(uri)));
    // The May26 API has only a tagged event runner, with exitCode/stdout/stderr.
    // It has no Bun chaining or cwd modifier. Ask that native runner for its cwd.
    const result = await ctx.$` + "`pwd`" + `;
    if (result.exitCode !== 0) throw new Error("native workspace unavailable");
    return nativeDirectory(result.stdout.replace(/\r?\n$/, ""));
  }
  if (options.attention) amp.on("session.start", async (event, ctx) => {
    try { await start(adapter, event.thread.id, await workspace(ctx)); } catch { /* best effort */ }
  });
  if (options.contract) amp.on("agent.start", async (event, ctx) => {
    let cwd;
    try { cwd = await workspace(ctx); } catch { return { message: { content: "Kata context unavailable: native workspace discovery failed.", display: false } }; }
    if (!selected(adapter, cwd, "contract")) return {};
    const id = event.thread?.id;
    if (typeof id !== "string" || !id) return {};
    const key = cwd + "\u0000" + id;
    let text;
    if (registry.contracts.has(key)) {
      text = await readContext(cwd, false);
    } else {
      const inFlight = registry.contractReads.get(key);
      if (inFlight) {
        await inFlight;
        text = await readContext(cwd, false);
      } else {
        let releaseRead;
        const pending = new Promise(resolve => { releaseRead = resolve; });
        registry.contractReads.set(key, pending);
        let contractLoaded = false;
        try {
          text = await readContext(cwd, true, () => { contractLoaded = true; });
          if (contractLoaded && selected(adapter, cwd, "contract")) registry.contracts.add(key);
        } finally {
          if (registry.contractReads.get(key) === pending) registry.contractReads.delete(key);
          releaseRead();
        }
      }
    }
    if (!selected(adapter, cwd, "contract")) return {};
    return { message: { content: text, display: false } };
  });
  // agent.end is a turn boundary. There is no native session.end event.
}
`
const openCodeV1AgentHookJS = `
export const KataPlugin = async ({ directory, client }) => {
  const cwd = nativeDirectory(directory);
  const adapter = register(cwd, cwd);
  const hooks = {};
  if (options.attention) hooks["chat.message"] = async input => {
    // TaskTool prompts also reach chat.message. Only a verified root session
    // may claim the launcher-owned attention ref; failed lookups skip writes.
    try {
      const result = await client.session.get({ path: { id: input.sessionID }, query: { directory: cwd } });
      const session = result.data;
      if (!session || session.id !== input.sessionID || session.parentID) return;
      await start(adapter, input.sessionID, cwd);
    } catch { /* best effort: never guess session ownership */ }
  };
  if (options.contract) hooks["experimental.chat.system.transform"] = async (_input, output) => {
    if (!selected(adapter, cwd, "contract")) return;
    const text = await readContext(cwd, true);
    if (selected(adapter, cwd, "contract")) appendContext(output.system, text, false);
  };
  // session.idle and session.deleted do not indicate terminal host shutdown.
  return hooks;
};
`
const openCodeV2AgentHookJS = `
export default Plugin.define({
  id: "kata-" + options.scope + (options.workspace ? ":" + options.workspace : ""),
  async setup(ctx) {
    const adapter = register(ctx.location.directory);
    adapter.enabled = { contract: false, attention: false };
    const registrations = [];
    async function sessionInfo(id) {
      // Location belongs to the plugin instance. Every session can have a
      // different native location, so resolve the session rather than guess.
      const session = await ctx.session.get({ sessionID: id });
      if (!session || session.id !== id) throw new Error("Native session metadata unavailable");
      return session;
    }
    async function workspace(id) {
      return nativeDirectory((await sessionInfo(id)).location.directory);
    }
    try {
      if (options.attention) { registrations.push(await ctx.session.hook("prompt", async event => {
        try {
          const session = await sessionInfo(event.sessionID);
          if (session.parentID) return;
          await start(adapter, event.sessionID, nativeDirectory(session.location.directory));
        } catch { /* best effort: unresolved sessions cannot claim attention */ }
      })); adapter.enabled.attention = true; }
      if (options.contract) { registrations.push(await ctx.session.hook("context", async event => {
        let cwd;
        try { cwd = await workspace(event.sessionID); }
        catch { return; /* Unresolved adapters must leave existing context intact. */ }
        if (!selected(adapter, cwd, "contract")) return;
        const text = await readContext(cwd, true);
        if (selected(adapter, cwd, "contract")) appendContext(event.system, text, true);
      })); adapter.enabled.contract = true; }
    } catch (error) {
      for (const registration of registrations) await registration.dispose();
      if (registry.adapters.get(adapter.key) === adapter) registry.adapters.delete(adapter.key);
      throw error;
    }
    return async () => {
      if (registry.adapters.get(adapter.key) === adapter) registry.adapters.delete(adapter.key);
      for (const registration of registrations) await registration.dispose();
      // Plugin unload is not terminal session cleanup; attention end belongs to
      // the launcher. Preserve same-host start baselines across plugin reloads.
    };
  }
});
`
