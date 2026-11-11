package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/safefileio"
)

var errEmbeddingCredentialWrongOwner = errors.New("credential file is not owned by the daemon user")

// CredentialSourceError reports why the selected source cannot supply a key.
// Reason never names the source, so it may reach clients that must not learn
// daemon-host paths or environment variable names.
type CredentialSourceError struct {
	Source string
	Reason string
}

func (e *CredentialSourceError) Error() string {
	return fmt.Sprintf("%v: %s: %s", ErrCredentialSource, e.Source, e.Reason)
}

func (e *CredentialSourceError) Unwrap() error { return ErrCredentialSource }

// ResolveSecret selects inline > file > named environment variable. An unusable
// selected source is a *CredentialSourceError, never a reason to fall back.
// Source is safe to log.
func ResolveSecret(inline, file, env string) (key, source string, err error) {
	if key = strings.TrimSpace(inline); key != "" {
		return key, "inline", nil
	}
	if file = strings.TrimSpace(file); file != "" {
		source = "file:" + file
		key, err = readSecretFile(file)
		if err != nil {
			return "", source, &CredentialSourceError{Source: source, Reason: err.Error()}
		}
		return key, source, nil
	}
	if env = strings.TrimSpace(env); env != "" {
		source = "env:" + env
		key = strings.TrimSpace(os.Getenv(env))
		if key == "" {
			return "", source, &CredentialSourceError{Source: source, Reason: "environment variable is unset or empty"}
		}
		return key, source, nil
	}
	return "", "none", nil
}

func readSecretFile(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("cannot resolve home")
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("must use an absolute path or ~/ path")
	}
	f, err := openEmbeddingCredentialFile(path)
	if err != nil {
		if errors.Is(err, errEmbeddingCredentialWrongOwner) {
			return "", errors.New("must be owned by the daemon user")
		}
		return "", errors.New("cannot read credential file")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not a readable regular file")
	}
	if runtime.GOOS == "windows" {
		if err := safefileio.ValidatePrivateCurrentUserFile(f); err != nil {
			return "", errors.New("must be a private current-user file: turn off inheritance and allow only the current user, SYSTEM and Administrators")
		}
	} else if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("group/world-readable; use chmod 600")
	}
	// Bound both the reported size and the read, since the file may grow.
	if info.Size() > 64<<10 {
		return "", errors.New("exceeds 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return "", errors.New("cannot read credential file")
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", errors.New("credential file is empty")
	}
	return key, nil
}
