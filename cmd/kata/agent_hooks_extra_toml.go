package main

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

type extraTOMLBlock struct {
	start, end int
	fields     map[string]any
}

var extraTOMLHookHeader = regexp.MustCompile(`^\s*\[\[hooks\]\]\s*(?:#.*)?$`)

func planExtraTOMLHooks(opts nativeAgentHookOptions, remove bool, path string) (nativeAgentHookPlan, error) {
	data, exists, err := readNativeAgentHookFile(path)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	blocks, err := parseExtraTOMLHooks(data)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	var current [3]bool
	contract, start, end := extraHookCapabilities(opts.Agent)
	capabilities := [3]bool{contract, start, end}
	for _, block := range blocks {
		kind := extraHookKind(opts.Agent, block.fields)
		if index := extraKindIndex(kind); index >= 0 && capabilities[index] && block.fields["event"] == extraHookEvent(opts.Agent, kind) && extraTOMLUnconditional(block.fields) {
			current[index] = true
		}
	}
	desired := extraHookDesired(opts, remove, current)
	plan := extraHookPlan(path, current, desired, opts.Agent)
	keep := make([]bool, len(blocks))
	for i := range keep {
		keep[i] = true
	}
	var appended bytes.Buffer
	for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
		selected := opts.Contract
		if kind != contractHook {
			selected = opts.Attention
		}
		if !selected {
			continue
		}
		var owned []int
		for i, block := range blocks {
			if extraHookKind(opts.Agent, block.fields) == kind {
				owned = append(owned, i)
			}
		}
		want := desired[extraKindIndex(kind)]
		var handler map[string]any
		if want {
			handler, err = extraHookHandler(opts, kind)
			if err != nil {
				return nativeAgentHookPlan{}, err
			}
			delete(handler, "type")
			handler["event"] = extraHookEvent(opts.Agent, kind)
		}
		if want && blocks != nil && len(owned) == 1 && extraTOMLUnconditional(blocks[owned[0]].fields) && extraJSONHandlerMatches(blocks[owned[0]].fields, handler) {
			continue
		}
		for _, index := range owned {
			keep[index] = false
		}
		if want {
			fmt.Fprintln(&appended, "[[hooks]]")
			if err := toml.NewEncoder(&appended).Encode(handler); err != nil {
				return nativeAgentHookPlan{}, err
			}
		}
	}
	var content bytes.Buffer
	// Slicing nil at zero is valid; make the absent-file no-op explicit while
	// retaining nil preimages and the parser's nil result for absent hooks.
	if data != nil {
		position := 0
		for i, block := range blocks {
			if keep[i] {
				continue
			}
			content.Write(data[position:block.start])
			content.Write(extraTOMLRetainedComments(data[block.start:block.end]))
			position = block.end
		}
		content.Write(data[position:])
	}
	if appended.Len() > 0 {
		if content.Len() > 0 && content.Bytes()[content.Len()-1] != '\n' {
			content.WriteByte('\n')
		}
		content.Write(appended.Bytes())
	}
	// Parse the rendered result before exposing a plan. This also catches
	// source layouts that a line scanner cannot reconcile without changing data.
	if _, err := parseExtraTOMLHooks(content.Bytes()); err != nil {
		return nativeAgentHookPlan{}, fmt.Errorf("cannot safely reconcile native TOML hooks: %w", err)
	}
	plan.Changes = []nativeAgentHookChange{{Path: path, Original: data, OriginalExists: exists, Content: bytes.Clone(content.Bytes()), Remove: remove && !exists}}
	return plan, nil
}

func extraTOMLUnconditional(fields map[string]any) bool {
	matcher, exists := fields["matcher"]
	return !exists || matcher == ""
}

func parseExtraTOMLHooks(data []byte) ([]extraTOMLBlock, error) {
	if data == nil {
		return nil, nil
	}
	var document map[string]any
	if _, err := toml.Decode(string(data), &document); err != nil {
		return nil, err
	}
	raw, exists := document["hooks"]
	if !exists {
		return nil, nil
	}
	hooks, ok := raw.([]map[string]any)
	if !ok {
		return nil, errors.New("native TOML hooks require [[hooks]] arrays")
	}
	headers := extraTOMLHeaders(data)
	var blocks []extraTOMLBlock
	for i, header := range headers {
		if !extraTOMLHookHeader.MatchString(header.text) {
			continue
		}
		end := len(data)
		if i+1 < len(headers) {
			end = headers[i+1].start
		}
		var blockDocument map[string]any
		if _, err := toml.Decode(string(data[header.start:end]), &blockDocument); err != nil {
			return nil, fmt.Errorf("unsupported native TOML hook layout: %w", err)
		}
		values, ok := blockDocument["hooks"].([]map[string]any)
		if !ok || len(values) != 1 || len(blockDocument) != 1 {
			return nil, errors.New("unsupported native TOML hook layout; use simple [[hooks]] blocks")
		}
		if err := validateExtraTOMLHook(values[0]); err != nil {
			return nil, err
		}
		blocks = append(blocks, extraTOMLBlock{start: header.start, end: end, fields: values[0]})
	}
	if len(blocks) != len(hooks) {
		return nil, errors.New("unsupported native TOML hook layout; use simple [[hooks]] blocks")
	}
	for i, block := range blocks {
		if !reflect.DeepEqual(block.fields, hooks[i]) {
			return nil, errors.New("unsupported nested native TOML hook layout")
		}
	}
	return blocks, nil
}

func validateExtraTOMLHook(fields map[string]any) error {
	for key, value := range fields {
		switch key {
		case "event", "command", "matcher":
			if _, ok := value.(string); !ok {
				return fmt.Errorf("native TOML hook %s must be text", key)
			}
		case "timeout":
			seconds, ok := value.(int64)
			if !ok || seconds < 1 || seconds > 600 {
				return errors.New("native TOML hook timeout must be 1 through 600 seconds")
			}
		default:
			return fmt.Errorf("unsupported native TOML hook field %q", key)
		}
	}
	for _, key := range []string{"event", "command"} {
		if value, ok := fields[key].(string); !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("native TOML hook requires %s", key)
		}
	}
	return nil
}

type extraTOMLHeader struct {
	start int
	text  string
}

// TOML permits table-looking text inside multiline strings and multiline
// arrays. Track both before treating a line as a table delimiter; the TOML
// parser remains authoritative for syntax and the recovered hook blocks.
func extraTOMLHeaders(data []byte) []extraTOMLHeader {
	var headers []extraTOMLHeader
	var quote byte
	multiline := false
	depth := 0
	offset := 0
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if quote == 0 && depth == 0 && strings.HasPrefix(trimmed, "[") {
			headers = append(headers, extraTOMLHeader{offset, trimmed})
			offset += len(line)
			continue
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			if quote != 0 {
				if quote == '"' && c == '\\' {
					i++
					continue
				}
				if c == quote {
					if !multiline {
						quote = 0
					} else if i+2 < len(line) && line[i+1] == quote && line[i+2] == quote {
						quote = 0
						multiline = false
						i += 2
					}
				}
				continue
			}
			if c == '#' {
				break
			}
			if c == '\'' || c == '"' {
				quote = c
				if i+2 < len(line) && line[i+1] == c && line[i+2] == c {
					multiline = true
					i += 2
				}
				continue
			}
			if c == '[' || c == '{' {
				depth++
			}
			if c == ']' || c == '}' {
				depth--
			}
		}
		offset += len(line)
	}
	return headers
}

func extraTOMLRetainedComments(block []byte) []byte {
	var output bytes.Buffer
	for _, line := range bytes.SplitAfter(block, []byte("\n")) {
		if strings.HasPrefix(strings.TrimSpace(string(line)), "#") {
			output.Write(line)
		}
	}
	return output.Bytes()
}
