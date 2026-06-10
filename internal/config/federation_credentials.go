package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// FederationCredentials is the local secret-bearing credentials.toml shape.
type FederationCredentials struct {
	Projects map[string]FederationCredential `toml:"projects"`
}

// FederationCredential stores the hub secret material for one local project
// UID. Tokens intentionally live outside SQLite and outside committed
// workspace config.
type FederationCredential struct {
	HubURL        string `toml:"hub_url"`
	HubProjectID  int64  `toml:"hub_project_id"`
	Token         string `toml:"token"`
	Capabilities  string `toml:"capabilities,omitempty"`
	Actor         string `toml:"actor,omitempty"`
	AllowInsecure bool   `toml:"allow_insecure,omitempty"`
}

// FederationCredentialMetadata is the redacted credential information safe
// to expose in daemon status responses.
type FederationCredentialMetadata struct {
	Status        string
	HubURL        string
	HubProjectID  int64
	Capabilities  string
	Actor         string
	AllowInsecure bool
}

// ReadFederationCredentials reads <KATA_HOME>/credentials.toml. Missing files
// return an empty credential set.
func ReadFederationCredentials() (*FederationCredentials, error) {
	path, err := FederationCredentialsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from KATA_HOME
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &FederationCredentials{Projects: map[string]FederationCredential{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var creds FederationCredentials
	if _, err := toml.Decode(string(data), &creds); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if creds.Projects == nil {
		creds.Projects = map[string]FederationCredential{}
	}
	return &creds, nil
}

// FederationCredentialMetadataFor returns redacted federation credential
// metadata for projectUID without exposing the stored token.
func FederationCredentialMetadataFor(projectUID string) FederationCredentialMetadata {
	creds, err := ReadFederationCredentials()
	if err != nil {
		return FederationCredentialMetadata{Status: "unreadable"}
	}
	c, ok := creds.Projects[projectUID]
	if !ok {
		return FederationCredentialMetadata{Status: "missing"}
	}
	return FederationCredentialMetadata{
		Status:        "present",
		HubURL:        c.HubURL,
		HubProjectID:  c.HubProjectID,
		Capabilities:  c.Capabilities,
		Actor:         c.Actor,
		AllowInsecure: c.AllowInsecure,
	}
}

// DeleteFederationCredential removes one project credential from
// <KATA_HOME>/credentials.toml. It is idempotent: a missing entry or a missing
// file is not an error. Called from the daemon leave route, mirroring
// WriteFederationCredential.
func DeleteFederationCredential(projectUID string) error {
	creds, err := ReadFederationCredentials()
	if err != nil {
		return err
	}
	if _, ok := creds.Projects[projectUID]; !ok {
		return nil
	}
	delete(creds.Projects, projectUID)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(creds); err != nil {
		return fmt.Errorf("encode federation credentials: %w", err)
	}
	path, err := FederationCredentialsPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil { //nolint:gosec // credentials file must be owner-only
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// WriteFederationCredential upserts one project credential into
// <KATA_HOME>/credentials.toml with owner-only permissions.
func WriteFederationCredential(projectUID string, c FederationCredential) error {
	creds, err := ReadFederationCredentials()
	if err != nil {
		return err
	}
	creds.Projects[projectUID] = c
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(creds); err != nil {
		return fmt.Errorf("encode federation credentials: %w", err)
	}
	path, err := FederationCredentialsPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil { //nolint:gosec // credentials file must be owner-only
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
