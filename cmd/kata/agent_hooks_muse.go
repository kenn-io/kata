package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"encoding/json/v2"
)

var errMuseAttentionPermission = errors.New("Muse attention needs permission to forward Kata variables; run kata agent-hooks install muse --managed-attention in user scope, or use launcher attention") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing permission error.

var extraMuseEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Muse rejects the whole source for malformed documented field types, unlike
// unknown fields or missing commands, which only skip their own handler.
func extraMuseRegistrationTypesValid(registrations []extraJSONRegistration) bool {
	for _, registration := range registrations {
		if !extraMuseHandlerTypesValid(registration.handler) {
			return false
		}
	}
	return true
}

func extraMuseHandlerTypesValid(handler map[string]any) bool {
	for field, value := range handler {
		switch field {
		case "type", "command", "commandWindows", "command_windows", "statusMessage":
			if _, ok := value.(string); !ok {
				return false
			}
		case "timeout":
			encoded, err := json.Marshal(value)
			if err != nil {
				return false
			}
			if _, err := strconv.ParseUint(string(encoded), 10, 64); err != nil {
				return false
			}
		case "async":
			if _, ok := value.(bool); !ok {
				return false
			}
		case "onFailure":
			fallback, ok := value.(map[string]any)
			if !ok || !extraMuseHandlerTypesValid(fallback) {
				return false
			}
		case "outputCapabilities":
			capabilities, ok := value.([]any)
			if !ok {
				return false
			}
			for _, capability := range capabilities {
				if _, ok := capability.(string); !ok {
					return false
				}
			}
		}
	}
	return true
}

// Names, never values, are added to Muse's documented managed-only allowlist.
// Workspace selection comes from native cwd, not an inherited workspace flag.
var extraMuseAttentionEnv = []string{"KATA_REF", "KATA_HOME", "KATA_SERVER", "KATA_DAEMON", "KATA_AUTH_TOKEN", "KATA_AUTHOR", "KATA_TEAMMATE", "KATA_INBOX_USER", "KATA_SESSION_ID", "KATA_TRUST_PRIVATE_NETWORK", "KATA_ALLOW_INSECURE"}

func planMuseAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	if opts.ManagedAttention && opts.Scope != "user" {
		return nativeAgentHookPlan{}, errors.New("Muse --managed-attention requires user scope; managed environment policy belongs to user settings") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
	}
	if !remove && opts.ManagedAttention && !opts.Attention {
		return nativeAgentHookPlan{}, errors.New("--managed-attention requires --attention")
	}
	path, err := extraAgentHookPath(opts)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	data, exists, err := readNativeAgentHookFile(path)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	settings, err := parseExtraJSON(data, exists)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	if opts.Scope == "user" {
		if exists {
			version, ok := settings["schema_version"]
			if !ok || !extraMuseSchemaOne(version) {
				return nativeAgentHookPlan{}, errors.New("Muse user settings require schema_version 1") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
			}
		} else if !remove {
			settings["schema_version"] = 1
		}
	}
	allowlist, err := validateExtraMuseAllowlist(settings)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	if opts.ManagedAttention && !remove {
		for _, name := range allowlist {
			for _, required := range extraMuseAttentionEnv {
				if strings.EqualFold(name, required) && name != required {
					return nativeAgentHookPlan{}, fmt.Errorf("Muse managed environment name %q has incompatible case; use %s before opting in", name, required) //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
				}
			}
		}
	}
	managedPath := ""
	configuredManaged := false
	if raw, exists := settings["managed_hooks_path"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nativeAgentHookPlan{}, errors.New("Muse managed_hooks_path must be a nonempty path") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
		}
		configuredManaged = true
		managedPath = value
		if !filepath.IsAbs(managedPath) {
			managedPath = filepath.Join(filepath.Dir(path), managedPath)
		}
		managedPath, err = filepath.Abs(managedPath)
		if err != nil {
			return nativeAgentHookPlan{}, err
		}
	} else if opts.ManagedAttention && !remove {
		managedPath = filepath.Join(filepath.Dir(path), "kata-managed-hooks.json")
		settings["managed_hooks_path"] = "kata-managed-hooks.json"
	}
	if managedPath == path {
		return nativeAgentHookPlan{}, errors.New("Muse managed_hooks_path cannot refer to its settings file") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
	}
	reuseManaged := opts.Scope == "user" && configuredManaged && extraMuseForwardsAttention(allowlist)
	if !remove && opts.Attention && !opts.ManagedAttention && !reuseManaged {
		return nativeAgentHookPlan{}, errMuseAttentionPermission
	}
	// Contract stays in its requested ordinary scope. No ordinary attention is
	// added, and an existing managed bundle is inspected even without opt-in.
	ordinary := opts
	ordinary.Attention = remove && opts.Attention
	settingsData, err := renderExtraJSON(data, settings)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	ordinaryExists := exists || len(settings) > 0
	plan, content, err := planExtraJSONHooks(ordinary, remove, path, settingsData, ordinaryExists, false)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	plan.CurrentAttentionStart, plan.CurrentAttentionEnd = false, false
	plan.AttentionStart, plan.AttentionEnd = false, false
	plan.Warnings = append(plan.Warnings, "Muse ordinary hooks use a cleared environment; attention needs the Kata launcher or authorized user managed forwarding. Project hooks also require Muse workspace trust")
	if managedPath != "" {
		managedData, managedExists, err := readNativeAgentHookFile(managedPath)
		if err != nil {
			return nativeAgentHookPlan{}, err
		}
		managedRoot, err := parseExtraJSON(managedData, managedExists)
		if err != nil {
			return nativeAgentHookPlan{}, err
		}
		if err := validateExtraMuseManagedFile(managedRoot); err != nil {
			return nativeAgentHookPlan{}, err
		}
		managedOpts := opts
		managedOpts.Contract = remove && opts.Contract
		managedOpts.Attention = opts.Attention && (remove || opts.ManagedAttention || reuseManaged)
		managedPlan, managedContent, err := planExtraJSONHooks(managedOpts, remove, managedPath, managedData, managedExists, false)
		if err != nil {
			return nativeAgentHookPlan{}, err
		}
		managedCurrentContract := managedPlan.CurrentContract
		plan.Warnings = append(plan.Warnings, managedPlan.Warnings...)
		if !remove && opts.Contract && extraJSONHasOwnedContract("muse", managedRoot) {
			if extraJSONHasOwnedContract("muse", settings) {
				// Reinstall reconciles duplicate owned context contributions to the
				// already-present ordinary contract, preserving managed attention.
				cleanup := opts
				cleanup.Contract = true
				cleanup.Attention = false
				_, cleaned, err := planExtraJSONHooks(cleanup, true, managedPath, managedData, managedExists, false)
				if err != nil {
					return nativeAgentHookPlan{}, err
				}
				managedPlan, managedContent, err = planExtraJSONHooks(managedOpts, false, managedPath, cleaned, managedExists, false)
				if err != nil {
					return nativeAgentHookPlan{}, err
				}
			} else {
				// A direct contract already in the selected managed file is owned
				// too. Repair it there instead of adding a second ordinary copy.
				managedOpts.Contract = true
				managedPlan, managedContent, err = planExtraJSONHooks(managedOpts, false, managedPath, managedData, managedExists, false)
				if err != nil {
					return nativeAgentHookPlan{}, err
				}
				ordinary.Contract = false
				_, content, err = planExtraJSONHooks(ordinary, true, path, settingsData, ordinaryExists, false)
				if err != nil {
					return nativeAgentHookPlan{}, err
				}
				plan.Contract = false
			}
		}
		plan.CurrentContract = plan.CurrentContract || managedCurrentContract
		plan.Contract = plan.Contract || managedPlan.Contract
		forwarding := extraMuseForwardsAttention(allowlist)
		plan.CurrentAttentionStart = managedPlan.CurrentAttentionStart && configuredManaged && forwarding
		plan.CurrentAttentionEnd = managedPlan.CurrentAttentionEnd && configuredManaged && forwarding
		if !remove && opts.ManagedAttention {
			seen := map[string]bool{}
			for _, name := range allowlist {
				seen[strings.ToLower(name)] = true
			}
			for _, name := range extraMuseAttentionEnv {
				if !seen[strings.ToLower(name)] {
					allowlist = append(allowlist, name)
					seen[strings.ToLower(name)] = true
				}
			}
			values := make([]any, len(allowlist))
			for i, name := range allowlist {
				values[i] = name
			}
			settings["managed_hooks_env_vars"] = values
			rendered, err := parseExtraJSON(content, true)
			if err != nil {
				return nativeAgentHookPlan{}, err
			}
			rendered["managed_hooks_path"] = settings["managed_hooks_path"]
			rendered["managed_hooks_env_vars"] = values
			content, err = renderExtraJSON(data, rendered)
			if err != nil {
				return nativeAgentHookPlan{}, err
			}
			forwarding = true
		}
		plan.AttentionStart = managedPlan.AttentionStart && forwarding
		plan.AttentionEnd = managedPlan.AttentionEnd && forwarding
		plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: managedPath, Original: managedData, OriginalExists: managedExists, Content: managedContent, Remove: remove && !managedExists})
		if plan.AttentionStart || plan.AttentionEnd {
			plan.Warnings = append(plan.Warnings, "Muse managed attention forwards only allowlisted variable names from the launching environment; existing operator policy is retained on uninstall")
		}
	}
	plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: path, Original: data, OriginalExists: exists, Content: content, Remove: remove && !exists})
	return plan, nil
}

func extraMuseSchemaOne(value any) bool {
	encoded, err := json.Marshal(value)
	return err == nil && string(encoded) == "1"
}

func validateExtraMuseAllowlist(settings map[string]any) ([]string, error) {
	raw, exists := settings["managed_hooks_env_vars"]
	if !exists {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, errors.New("Muse managed_hooks_env_vars must be an array of names") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
	}
	var names []string
	seen := map[string]bool{}
	for _, raw := range values {
		name, ok := raw.(string)
		if !ok || !extraMuseEnvName.MatchString(name) {
			return nil, errors.New("invalid Muse managed_hooks_env_vars name")
		}
		folded := strings.ToLower(name)
		if seen[folded] {
			return nil, errors.New("duplicate Muse managed_hooks_env_vars name")
		}
		seen[folded] = true
		if strings.HasSuffix(strings.ToUpper(name), "_API_KEY") {
			return nil, fmt.Errorf("Muse provider credential name %q cannot be forwarded", name) //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
		}
		names = append(names, name)
	}
	return names, nil
}

func extraMuseForwardsAttention(names []string) bool {
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	for _, name := range extraMuseAttentionEnv {
		if !seen[name] {
			return false
		}
	}
	return true
}

func validateExtraMuseManagedFile(root map[string]any) error {
	if _, exists := root["managed_hooks_env_vars"]; exists {
		return errors.New("Muse managed_hooks_env_vars belongs only in user settings") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
	}
	var walk func(any) error
	walk = func(value any) error {
		switch node := value.(type) {
		case map[string]any:
			if _, exists := node["name"]; exists {
				return errors.New("unsupported strict Muse managed hook layout (name/timeout_ms); ordinary managed hooks are required")
			}
			if _, exists := node["timeout_ms"]; exists {
				return errors.New("unsupported strict Muse managed hook layout (name/timeout_ms)")
			}
			for _, child := range node {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range node {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root["hooks"])
}
