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
	HubURL       string `toml:"hub_url"`
	HubProjectID int64  `toml:"hub_project_id"`
	Token        string `toml:"token"`
	Capabilities string `toml:"capabilities,omitempty"`
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
