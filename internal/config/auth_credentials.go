package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrCredentialSource distinguishes a selected secret failure from unrelated
// config parsing errors. Clients must not silently drop selected credentials.
var ErrCredentialSource = errors.New("selected credential is unavailable")

// ReadPersistedAuthToken is only for a positively identified local daemon.
// Global auth resolution must never discover an implicit owner credential.
func ReadPersistedAuthToken(home string) (string, error) {
	path, err := filepath.Abs(filepath.Join(home, "auth-token"))
	if err != nil {
		return "", fmt.Errorf("%w: resolve persisted token path: %v", ErrCredentialSource, err)
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	key, _, err := ResolveSecret("", path, "")
	return key, err
}

func resolveAuthCredential(auth *AuthConfig) error {
	key, source, err := ResolveSecret(auth.Token, auth.TokenFile, auth.TokenEnv)
	if err != nil {
		return err
	}
	auth.Token, auth.Source = key, source
	return nil
}
