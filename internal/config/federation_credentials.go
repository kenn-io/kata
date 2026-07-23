package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	HubURL           string `toml:"hub_url"`
	HubProjectID     int64  `toml:"hub_project_id"`
	Token            string `toml:"token"`
	Capabilities     string `toml:"capabilities,omitempty"`
	Actor            string `toml:"actor,omitempty"`
	AllowInsecure    bool   `toml:"allow_insecure,omitempty"`
	ManagedByConfig  bool   `toml:"managed_by_config,omitempty"`
	HubCatalog       string `toml:"hub_catalog,omitempty"`
	HubProjectName   string `toml:"hub_project_name,omitempty"`
	RequestedActor   string `toml:"requested_actor,omitempty"`
	SpokeProjectName string `toml:"spoke_project_name,omitempty"`
}

// FederationCredentialMetadata is the redacted credential information safe
// to expose in daemon status responses.
type FederationCredentialMetadata struct {
	Status           string
	HubURL           string
	HubProjectID     int64
	Capabilities     string
	Actor            string
	AllowInsecure    bool
	ManagedByConfig  bool
	HubCatalog       string
	HubProjectName   string
	RequestedActor   string
	SpokeProjectName string
}

// FederationCredentialRekey describes one compare-and-rekey transition from a
// local project UID to the stable hub project UID.
type FederationCredentialRekey struct {
	FromProjectUID string
	ToProjectUID   string
	Expected       FederationCredential
	Replacement    FederationCredential
}

// FederationManagedCredentialReservation is one config-managed credential
// reserved under its stable hub project UID.
type FederationManagedCredentialReservation struct {
	ProjectUID string
	Credential FederationCredential
}

// ErrFederationCredentialConflict classifies a rekey whose source or target
// changed after the caller inspected the credential file.
var ErrFederationCredentialConflict = errors.New("federation credential conflict")

var federationCredentialsMu sync.Mutex

var (
	writeFederationCredentialsTempFile = writeAllFederationCredentials
	renameFederationCredentialsFile    = replaceFederationCredentialsFileOnDisk
)

// FederationCredentialStore isolates secret-bearing federation credentials
// from the database and from other service instances in the same process.
type FederationCredentialStore interface {
	FederationCredential(context.Context, string) (FederationCredential, bool, error)
	StoreFederationCredential(context.Context, string, FederationCredential) error
	DeleteFederationCredential(context.Context, string) error
}

// FederationManagedCredentialStore is the managed credential boundary used by
// config-driven federation. Managed credentials have one durable hub UID key.
type FederationManagedCredentialStore interface {
	FederationCredentialStore
	ReserveManagedFederationCredential(
		context.Context, FederationManagedCredentialReservation,
	) error
	FindManagedFederationCredential(
		context.Context, string,
	) (FederationManagedCredentialReservation, bool, error)
	RekeyFederationCredential(context.Context, FederationCredentialRekey) error
	DeleteManagedFederationCredential(
		context.Context, FederationManagedCredentialReservation,
	) error
}

// homeFederationCredentialStore uses the standalone daemon's
// <KATA_HOME>/credentials.toml file.
type homeFederationCredentialStore struct{}

func (homeFederationCredentialStore) FederationCredential(
	_ context.Context, projectUID string,
) (FederationCredential, bool, error) {
	credentials, err := ReadFederationCredentials()
	if err != nil {
		return FederationCredential{}, false, err
	}
	credential, ok := credentials.Projects[projectUID]
	return credential, ok, nil
}

func (homeFederationCredentialStore) StoreFederationCredential(
	_ context.Context, projectUID string, credential FederationCredential,
) error {
	return WriteFederationCredential(projectUID, credential)
}

func (homeFederationCredentialStore) DeleteFederationCredential(
	_ context.Context, projectUID string,
) error {
	return DeleteFederationCredential(projectUID)
}

func (homeFederationCredentialStore) RekeyFederationCredential(
	_ context.Context, rekey FederationCredentialRekey,
) error {
	return RekeyFederationCredential(rekey)
}

func (homeFederationCredentialStore) ReserveManagedFederationCredential(
	_ context.Context, reservation FederationManagedCredentialReservation,
) error {
	return ReserveManagedFederationCredential(reservation)
}

func (homeFederationCredentialStore) FindManagedFederationCredential(
	_ context.Context, projectName string,
) (FederationManagedCredentialReservation, bool, error) {
	return FindManagedFederationCredential(projectName)
}

func (homeFederationCredentialStore) DeleteManagedFederationCredential(
	_ context.Context, reservation FederationManagedCredentialReservation,
) error {
	return DeleteManagedFederationCredential(reservation)
}

// DefaultFederationCredentialStore returns the standalone daemon credential
// store. Embedded services supply their own isolated store.
func DefaultFederationCredentialStore() FederationManagedCredentialStore {
	return homeFederationCredentialStore{}
}

// ReadFederationCredentials reads <KATA_HOME>/credentials.toml. Missing files
// return an empty credential set.
func ReadFederationCredentials() (*FederationCredentials, error) {
	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	return readFederationCredentials()
}

func readFederationCredentials() (*FederationCredentials, error) {
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
	return FederationCredentialMetadataFromStore(context.Background(), DefaultFederationCredentialStore(), projectUID)
}

// FederationCredentialMetadataFromStore returns redacted metadata from store.
func FederationCredentialMetadataFromStore(
	ctx context.Context, store FederationCredentialStore, projectUID string,
) FederationCredentialMetadata {
	c, ok, err := store.FederationCredential(ctx, projectUID)
	if err != nil {
		return FederationCredentialMetadata{Status: "unreadable"}
	}
	if !ok {
		return FederationCredentialMetadata{Status: "missing"}
	}
	return FederationCredentialMetadata{
		Status:           "present",
		HubURL:           c.HubURL,
		HubProjectID:     c.HubProjectID,
		Capabilities:     c.Capabilities,
		Actor:            c.Actor,
		AllowInsecure:    c.AllowInsecure,
		ManagedByConfig:  c.ManagedByConfig,
		HubCatalog:       c.HubCatalog,
		HubProjectName:   c.HubProjectName,
		RequestedActor:   c.RequestedActor,
		SpokeProjectName: c.SpokeProjectName,
	}
}

// DeleteFederationCredential removes one project credential from
// <KATA_HOME>/credentials.toml. It is idempotent: a missing entry or a missing
// file is not an error. Called from the daemon leave route, mirroring
// WriteFederationCredential.
func DeleteFederationCredential(projectUID string) error {
	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	creds, err := readFederationCredentials()
	if err != nil {
		return err
	}
	if _, ok := creds.Projects[projectUID]; !ok {
		return nil
	}
	delete(creds.Projects, projectUID)
	return writeFederationCredentials(creds)
}

func writeFederationCredentials(creds *FederationCredentials) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(creds); err != nil {
		return fmt.Errorf("encode federation credentials: %w", err)
	}
	path, err := FederationCredentialsPath()
	if err != nil {
		return err
	}
	return replaceFederationCredentialsFile(path, buf.Bytes())
}

func replaceFederationCredentialsFile(path string, data []byte) (retErr error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*") //nolint:gosec // owner-only mode is enforced before writing.
	if err != nil {
		return fmt.Errorf("create temporary federation credentials in %s: %w", dir, err)
	}
	tempPath := temp.Name()
	renamed := false
	closed := false
	defer func() {
		if renamed {
			return
		}
		if !closed {
			if closeErr := temp.Close(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close temporary federation credentials %s: %w", tempPath, closeErr))
			}
		}
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary federation credentials %s: %w", tempPath, removeErr))
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temporary federation credentials %s: %w", tempPath, err)
	}
	if err := writeFederationCredentialsTempFile(temp, data); err != nil {
		return fmt.Errorf("write temporary federation credentials %s: %w", tempPath, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary federation credentials %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary federation credentials %s: %w", tempPath, err)
	}
	closed = true
	if err := renameFederationCredentialsFile(tempPath, path); err != nil {
		return fmt.Errorf("replace federation credentials %s: %w", path, err)
	}
	renamed = true
	if err := syncFederationCredentialsDirectory(dir); err != nil {
		return fmt.Errorf("sync federation credentials directory %s: %w", dir, err)
	}
	return nil
}

func writeAllFederationCredentials(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// WriteFederationCredential upserts one project credential into
// <KATA_HOME>/credentials.toml with owner-only permissions.
func WriteFederationCredential(projectUID string, c FederationCredential) error {
	return updateFederationCredentials(func(creds *FederationCredentials) error {
		creds.Projects[projectUID] = c
		return nil
	})
}

// ReserveManagedFederationCredential atomically reserves a config-managed
// credential under exactly one stable hub project UID. An exact replay is
// idempotent; a different existing credential is left unchanged and conflicts.
func ReserveManagedFederationCredential(
	reservation FederationManagedCredentialReservation,
) error {
	projectUID := strings.TrimSpace(reservation.ProjectUID)
	if projectUID == "" {
		return fmt.Errorf("%w: reservation project UID is empty", ErrFederationCredentialConflict)
	}
	return updateFederationCredentials(func(creds *FederationCredentials) error {
		existing, found := creds.Projects[projectUID]
		if found && existing != reservation.Credential {
			return fmt.Errorf("%w: reservation target credential differs", ErrFederationCredentialConflict)
		}
		if !found {
			creds.Projects[projectUID] = reservation.Credential
		}
		return nil
	})
}

// FindManagedFederationCredential locates the one managed reservation for a
// spoke project. Multiple marked entries are ambiguous and conflict.
func FindManagedFederationCredential(
	projectName string,
) (FederationManagedCredentialReservation, bool, error) {
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return FederationManagedCredentialReservation{}, false, nil
	}
	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	creds, err := readFederationCredentials()
	if err != nil {
		return FederationManagedCredentialReservation{}, false, err
	}
	var match FederationManagedCredentialReservation
	found := false
	for projectUID, credential := range creds.Projects {
		if !credential.ManagedByConfig ||
			strings.TrimSpace(credential.SpokeProjectName) != projectName {
			continue
		}
		if found {
			return FederationManagedCredentialReservation{}, false, fmt.Errorf(
				"%w: multiple managed reservations for project %s",
				ErrFederationCredentialConflict, projectName,
			)
		}
		match = FederationManagedCredentialReservation{
			ProjectUID: projectUID,
			Credential: credential,
		}
		found = true
	}
	return match, found, nil
}

// DeleteManagedFederationCredential removes an observed managed reservation
// only if its durable hub UID and credential value are unchanged.
func DeleteManagedFederationCredential(
	match FederationManagedCredentialReservation,
) error {
	return updateFederationCredentials(func(creds *FederationCredentials) error {
		current, found := creds.Projects[match.ProjectUID]
		if !found {
			spokeProject := strings.TrimSpace(match.Credential.SpokeProjectName)
			for projectUID, credential := range creds.Projects {
				if projectUID != match.ProjectUID &&
					credential.ManagedByConfig &&
					strings.TrimSpace(credential.SpokeProjectName) == spokeProject {
					return fmt.Errorf(
						"%w: managed reservation moved before cleanup",
						ErrFederationCredentialConflict,
					)
				}
			}
			return nil
		}
		if current != match.Credential || !current.ManagedByConfig {
			return fmt.Errorf("%w: managed reservation changed before cleanup", ErrFederationCredentialConflict)
		}
		delete(creds.Projects, match.ProjectUID)
		return nil
	})
}

// RekeyFederationCredential atomically replaces FromProjectUID with
// ToProjectUID inside one serialized credentials-file read-modify-write.
func RekeyFederationCredential(rekey FederationCredentialRekey) error {
	if rekey.FromProjectUID == "" ||
		rekey.ToProjectUID == "" ||
		rekey.FromProjectUID == rekey.ToProjectUID {
		return fmt.Errorf("%w: invalid rekey project UIDs", ErrFederationCredentialConflict)
	}
	return updateFederationCredentials(func(creds *FederationCredentials) error {
		source, sourceFound := creds.Projects[rekey.FromProjectUID]
		target, targetFound := creds.Projects[rekey.ToProjectUID]
		if !sourceFound {
			if targetFound && target == rekey.Replacement {
				return nil
			}
			return fmt.Errorf("%w: source credential is missing", ErrFederationCredentialConflict)
		}
		if source != rekey.Expected {
			return fmt.Errorf("%w: source credential changed", ErrFederationCredentialConflict)
		}
		if targetFound && target != rekey.Expected && target != rekey.Replacement {
			return fmt.Errorf("%w: target credential differs", ErrFederationCredentialConflict)
		}
		creds.Projects[rekey.ToProjectUID] = rekey.Replacement
		delete(creds.Projects, rekey.FromProjectUID)
		return nil
	})
}

func updateFederationCredentials(update func(*FederationCredentials) error) error {
	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	creds, err := readFederationCredentials()
	if err != nil {
		return err
	}
	if err := update(creds); err != nil {
		return err
	}
	return writeFederationCredentials(creds)
}
