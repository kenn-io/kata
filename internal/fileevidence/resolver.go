package fileevidence

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kata/internal/processtree"
)

// Source is an operator-owned local root or read-only adapter command.
type Source struct {
	Root    string   `toml:"root"`
	Command []string `toml:"command"`
	SHA256  bool     `toml:"sha256"`
	// Env names explicitly grant daemon environment variables to the adapter.
	Env []string `toml:"env"`
}

// Config grants source publication and resolution authority to this daemon.
type Config struct {
	Sources map[string]Source `toml:"sources"`
}

// Validate rejects incomplete source configuration without contacting providers.
func (c Config) Validate() error {
	for alias, s := range c.Sources {
		if !aliasPattern.MatchString(alias) || (s.Root == "") == (len(s.Command) == 0) || (s.Root != "" && !filepath.IsAbs(s.Root)) {
			return errors.New("file_evidence sources require valid aliases and exactly one absolute root or command argv")
		}
		if len(s.Command) > 0 && strings.TrimSpace(s.Command[0]) == "" {
			return errors.New("file_evidence command must not be empty")
		}
		for _, name := range s.Env {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return errors.New("file_evidence env must contain variable names")
			}
		}
	}
	return nil
}

// Result preserves the cited reference separately from the current read's bytes.
// Content is returned only for successful reads; callers decide whether to expose it.
type Result struct {
	Status    string    `json:"status"`
	Reference Reference `json:"reference"`
	Content   []byte    `json:"content,omitzero"`
}

// Capture resolves complete bytes once and freezes their metadata for closure.
func (c Config) Capture(ctx context.Context, r Reference, origin string) (Reference, error) {
	if err := r.ValidateRequest(); err != nil {
		return Reference{}, err
	}
	if err := c.Validate(); err != nil {
		return Reference{}, err
	}
	source, ok := c.Sources[r.Source]
	if !ok {
		return Reference{}, errors.New("file evidence source is not configured")
	}
	raw, rev, status := source.read(ctx, r, false)
	if status != "ok" {
		return Reference{}, errors.New("file evidence " + status)
	}
	hash := source.SHA256
	if r.CaptureSHA256 != nil {
		hash = *r.CaptureSHA256
	}
	r = describe(r, raw, hash)
	r.Origin = origin
	r.Revision = rev
	if err := r.ValidateRetained(); err != nil {
		return Reference{}, err
	}
	return r, nil
}

// Read compares current or historical bytes against the original retained reference.
func (c Config) Read(ctx context.Context, r Reference, origin string, historical bool) Result {
	result := Result{Reference: r}
	if r.ValidateRetained() != nil {
		result.Status = "invalid"
		return result
	}
	// Origin fencing precedes lookup: same-named aliases on replicas are unrelated.
	if r.Origin != origin {
		result.Status = "foreign-origin"
		return result
	}
	source, ok := c.Sources[r.Source]
	if !ok || c.Validate() != nil {
		result.Status = "unreachable"
		return result
	}
	if historical && r.Revision == "" && r.SHA256 == "" {
		result.Status = "history-unavailable"
		return result
	}
	raw, _, status := source.read(ctx, r, historical)
	result.Status = status
	if status != "ok" {
		return result
	}
	result.Content = raw
	result.Status = "unverified"
	if r.SHA256 != "" {
		got := describe(r, raw, true)
		result.Status = "unchanged"
		if got.SHA256 != r.SHA256 || *got.Size != *r.Size {
			result.Status = "changed"
		}
	}
	return result
}

func (s Source) read(ctx context.Context, r Reference, historical bool) ([]byte, string, string) {
	if ctx.Err() != nil {
		return nil, "", "unreachable"
	}
	if s.Root != "" {
		if historical {
			return nil, "", "history-unavailable"
		}
		return readLocal(s.Root, r.Path)
	}
	return s.readAdapter(ctx, r, historical)
}

func readLocal(root, name string) ([]byte, string, string) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", "unreachable"
	}
	defer dir.Close()
	// Reject observed symlinks and nonregular final nodes before opening (FIFO).
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := dir.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, "", localStatus(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return nil, "", "invalid-file"
		}
	}
	f, err := openLocalFile(dir, name)
	if err != nil {
		return nil, "", localStatus(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > MaxBytes {
		return nil, "", "invalid-file"
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, "", "unreachable"
	}
	after, err := f.Stat()
	if err != nil {
		return nil, "", "unreachable"
	}
	current, err := dir.Stat(name)
	if err != nil || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != after.Size() {
		return nil, "", "changed-during-read"
	}
	if len(raw) > MaxBytes {
		return nil, "", "too-large"
	}
	return raw, "", "ok"
}

func localStatus(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	return "unreachable"
}

type resolverRequest struct {
	Version  int    `json:"version"`
	Source   string `json:"source"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
	Revision string `json:"revision,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

type resolverResponse struct {
	Version       int     `json:"version"`
	Source        string  `json:"source"`
	Path          string  `json:"path"`
	Status        string  `json:"status"`
	Revision      string  `json:"revision,omitempty"`
	ContentBase64 *string `json:"content_base64,omitempty"`
}

type boundedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	const maxEnvelope = ((MaxBytes+2)/3)*4 + 16384
	n := len(p)
	if b.Len()+n > maxEnvelope {
		b.exceeded = true
		p = p[:max(0, maxEnvelope-b.Len())]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (s Source) readAdapter(parent context.Context, r Reference, historical bool) ([]byte, string, string) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	request := resolverRequest{Version: 1, Source: r.Source, Path: r.Path, Mode: "current"}
	if historical {
		request.Mode = "historical"
		request.Revision = r.Revision
		request.SHA256 = r.SHA256
	}
	input, _ := json.Marshal(request)
	// Only operator configuration chooses this argv. No shell, no issue-provided executable.
	cmd := exec.Command(s.Command[0], s.Command[1:]...) //nolint:gosec,noctx // process tree owns timeout and argv is trusted config.
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = []string{} // A nil environment would inherit every daemon variable.
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "WINDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	for _, name := range s.Env {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		} else {
			return nil, "", "unreachable"
		}
	}
	out := &boundedOutput{}
	cmd.Stdout = out
	tree, err := processtree.New(cmd)
	if err != nil {
		return nil, "", "unreachable"
	}
	defer tree.Close()
	if ctx.Err() != nil || tree.Start() != nil {
		return nil, "", "unreachable"
	}
	done := make(chan error, 1)
	go func() { done <- tree.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = tree.TerminateWithGrace(100 * time.Millisecond)
		<-done
		return nil, "", "unreachable"
	}
	if err != nil {
		return nil, "", "unreachable"
	}
	if out.exceeded {
		return nil, "", "invalid-response"
	}
	var response resolverResponse
	if json.Unmarshal(out.Bytes(), &response, json.RejectUnknownMembers(true)) != nil || response.Version != 1 || response.Source != r.Source || response.Path != r.Path || len(response.Revision) > 4096 {
		return nil, "", "invalid-response"
	}
	switch response.Status {
	case "missing", "unreachable", "history-unavailable":
		if response.ContentBase64 != nil || response.Revision != "" {
			return nil, "", "invalid-response"
		}
		return nil, "", response.Status
	case "ok":
		if response.ContentBase64 == nil {
			return nil, "", "invalid-response"
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(*response.ContentBase64)
		if err != nil || len(raw) > MaxBytes {
			return nil, "", "invalid-response"
		}
		if historical && r.Revision != "" && response.Revision != r.Revision {
			return nil, "", "invalid-response"
		}
		return raw, response.Revision, "ok"
	default:
		return nil, "", "invalid-response"
	}
}
