package config

import "fmt"

// EmbeddingCredential separates a secret from safe operator diagnostics.
// Reason is nonempty when the selected source cannot supply a usable key.
type EmbeddingCredential struct {
	Key    string
	Source string
	Reason string
}

// ResolveCredential reads the selected embedding key at startup or reload.
// An unusable selected source never falls through to another source.
func (e EmbeddingsConfig) ResolveCredential() EmbeddingCredential {
	key, source, err := ResolveSecret(e.APIKey, e.APIKeyFile, e.APIKeyEnv)
	credential := EmbeddingCredential{Key: key, Source: source}
	if err != nil {
		credential.Reason = fmt.Sprintf("no embedding API key (%v)", err)
	}
	return credential
}
