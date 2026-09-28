package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	errEmbeddingCredentialSymlink    = errors.New("embedding credential path contains a symbolic link")
	errEmbeddingCredentialUnsafeDir  = errors.New("embedding credential parent directory is not trusted")
	errEmbeddingCredentialWrongOwner = errors.New("embedding credential file is not owned by the daemon user")
)

// EmbeddingCredential separates a secret from safe operator diagnostics.
// Reason is nonempty when the selected source cannot supply a usable key.
type EmbeddingCredential struct {
	Key    string
	Source string
	Reason string
}

// ResolveCredential reads the selected credential at startup or reload.
// Precedence is inline > file > env; an unusable selected file never falls
// through to another source. Relative files resolve from the daemon cwd.
func (e EmbeddingsConfig) ResolveCredential() EmbeddingCredential {
	if key := strings.TrimSpace(e.APIKey); key != "" {
		return EmbeddingCredential{Key: key, Source: "inline"}
	}
	if file := strings.TrimSpace(e.APIKeyFile); file != "" {
		c := EmbeddingCredential{Source: "file:" + file}
		path := file
		if file == "~" || strings.HasPrefix(file, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				c.Reason = "no embedding API key (cannot resolve home for " + c.Source + ")"
				return c
			}
			path = filepath.Join(home, strings.TrimPrefix(file, "~/"))
			if file == "~" {
				path = home
			}
		}
		// Reject nonregular targets before open for a readable error. The Unix
		// opener rejects symlinks and rechecks each parent through descriptors so
		// path replacement cannot bypass its directory checks.
		info, err := os.Stat(path)
		if err != nil {
			c.Reason = "no embedding API key (cannot read " + c.Source + ")"
			return c
		}
		if !info.Mode().IsRegular() {
			c.Reason = "no embedding API key (" + c.Source + " is not a readable regular file)"
			return c
		}
		f, err := openEmbeddingCredentialFile(path) //nolint:gosec // G304: operator-configured key file; descriptor type, permissions, and size are validated.
		if err != nil {
			switch {
			case errors.Is(err, errEmbeddingCredentialSymlink):
				c.Reason = "no embedding API key (" + c.Source + " uses a symbolic link; configure the target file directly)"
			case errors.Is(err, errEmbeddingCredentialUnsafeDir):
				c.Reason = "no embedding API key (" + c.Source + " parent directories must be owned by root or the daemon user and not writable by other users)"
			case errors.Is(err, errEmbeddingCredentialWrongOwner):
				c.Reason = "no embedding API key (" + c.Source + " must be owned by the daemon user)"
			default:
				c.Reason = "no embedding API key (cannot read " + c.Source + ")"
			}
			return c
		}
		defer func() { _ = f.Close() }()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			c.Reason = "no embedding API key (" + c.Source + " is not a readable regular file)"
			return c
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			c.Reason = "no embedding API key (" + c.Source + " is group/world-readable; use chmod 600)"
			return c
		}
		// Secrets are small. Refuse oversized files rather than reading arbitrary
		// operator-selected files into daemon memory.
		if info.Size() > 64<<10 {
			c.Reason = "no embedding API key (" + c.Source + " exceeds 64 KiB)"
			return c
		}
		data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if err != nil || len(data) > 64<<10 {
			c.Reason = "no embedding API key (cannot read " + c.Source + ")"
			return c
		}
		c.Key = strings.TrimSpace(string(data))
		if c.Key == "" {
			c.Reason = "no embedding API key (" + c.Source + " is empty)"
		}
		return c
	}
	if name := strings.TrimSpace(e.APIKeyEnv); name != "" {
		c := EmbeddingCredential{Key: strings.TrimSpace(os.Getenv(name)), Source: "env:" + name}
		if c.Key == "" {
			c.Reason = fmt.Sprintf("no embedding API key (env %s is unset or empty)", name)
		}
		return c
	}
	return EmbeddingCredential{Source: "none", Reason: "no embedding API key (set api_key, api_key_file, or api_key_env)"}
}
