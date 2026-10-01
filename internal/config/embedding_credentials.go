package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/secretref"
)

var errEmbeddingCredentialWrongOwner = errors.New("embedding credential file is not owned by the daemon user")

// EmbeddingCredential separates a secret from safe operator diagnostics.
// Reason is nonempty when the selected source cannot supply a usable key.
type EmbeddingCredential struct {
	Key    string
	Source string
	Reason string
}

// ResolveCredential reads the selected credential at startup or reload.
// Precedence is inline > file > env; an unusable selected file never falls
// through to another source. Files must use an absolute path or ~/.
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
		if !filepath.IsAbs(path) {
			c.Reason = "no embedding API key (" + c.Source + " must use an absolute path or ~/ path)"
			return c
		}
		f, err := openEmbeddingCredentialFile(path) //nolint:gosec // G304: operator-configured key file; descriptor type, permissions, and size are validated.
		if err != nil {
			if errors.Is(err, errEmbeddingCredentialWrongOwner) {
				c.Reason = "no embedding API key (" + c.Source + " must be owned by the daemon user)"
			} else {
				c.Reason = "no embedding API key (cannot read " + c.Source + ")"
			}
			return c
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
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
		c := EmbeddingCredential{Source: "env:" + name}
		secret, err := (embedconfig.Embedder{APIKey: secretref.Ref{Env: name}}).ResolveAPIKey()
		if err != nil {
			c.Reason = fmt.Sprintf("no embedding API key (env %s is unset or empty)", name)
		} else {
			c.Key = strings.TrimSpace(secret.Value)
		}
		return c
	}
	return EmbeddingCredential{Source: "none"}
}
