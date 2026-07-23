package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// FederationCredentialReservation describes one credential value that must be
// reserved under every listed project UID in a single atomic update.
type FederationCredentialReservation struct {
	ProjectUIDs []string
	Credential  FederationCredential
}

// FederationCredentialReservationMatch is one config-managed reservation and
// every credential key currently carrying its exact value.
type FederationCredentialReservationMatch struct {
	ProjectUIDs []string
	Credential  FederationCredential
}

// FederationCredentialReservationCleanup constrains alias cleanup to the
// exact managed reservation observed for one current local project.
type FederationCredentialReservationCleanup struct {
	SpokeProjectName  string
	CurrentProjectUID string
	Expected          FederationCredential
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

// FederationCredentialRekeyer is the optional atomic move capability required
// when config reconciliation adopts a standalone project under a different hub
// UID. Keeping it separate preserves compatibility for service-scoped stores
// that never perform that transition.
type FederationCredentialRekeyer interface {
	RekeyFederationCredential(context.Context, FederationCredentialRekey) error
}

// FederationCredentialReserver is the optional compare-and-store capability
// used to reserve a config-managed enrollment token without overwriting a
// concurrent manual credential.
type FederationCredentialReserver interface {
	ReserveFederationCredentials(context.Context, FederationCredentialReservation) error
}

// FederationCredentialReservationFinder is the optional lookup used to
// discover a config-managed reservation independently of its credential key.
type FederationCredentialReservationFinder interface {
	FederationCredentialReservationForProject(
		context.Context, string,
	) (FederationCredentialReservationMatch, bool, error)
}

// FederationCredentialReservationCleaner is the optional atomic cleanup used
// when leaving a project that may have config-reservation aliases.
type FederationCredentialReservationCleaner interface {
	DeleteFederationCredentialReservationForProject(
		context.Context, FederationCredentialReservationCleanup,
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

func (homeFederationCredentialStore) ReserveFederationCredentials(
	_ context.Context, reservation FederationCredentialReservation,
) error {
	return ReserveFederationCredentials(reservation)
}

func (homeFederationCredentialStore) FederationCredentialReservationForProject(
	_ context.Context, projectName string,
) (FederationCredentialReservationMatch, bool, error) {
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return FederationCredentialReservationMatch{}, false, nil
	}
	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	credentials, err := readFederationCredentials()
	if err != nil {
		return FederationCredentialReservationMatch{}, false, err
	}
	var match FederationCredentialReservationMatch
	found := false
	for projectUID, credential := range credentials.Projects {
		if !credential.ManagedByConfig ||
			strings.TrimSpace(credential.SpokeProjectName) != projectName {
			continue
		}
		if found && credential != match.Credential {
			return FederationCredentialReservationMatch{}, false, fmt.Errorf(
				"%w: managed reservations for project %s disagree",
				ErrFederationCredentialConflict,
				projectName,
			)
		}
		match.Credential = credential
		match.ProjectUIDs = append(match.ProjectUIDs, projectUID)
		found = true
	}
	sort.Strings(match.ProjectUIDs)
	return match, found, nil
}

func (homeFederationCredentialStore) DeleteFederationCredentialReservationForProject(
	_ context.Context, cleanup FederationCredentialReservationCleanup,
) error {
	cleanup.SpokeProjectName = strings.TrimSpace(cleanup.SpokeProjectName)
	cleanup.CurrentProjectUID = strings.TrimSpace(cleanup.CurrentProjectUID)
	if cleanup.SpokeProjectName == "" ||
		cleanup.CurrentProjectUID == "" ||
		!cleanup.Expected.ManagedByConfig ||
		strings.TrimSpace(cleanup.Expected.SpokeProjectName) != cleanup.SpokeProjectName {
		return fmt.Errorf("%w: invalid managed reservation cleanup", ErrFederationCredentialConflict)
	}

	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	credentials, err := readFederationCredentials()
	if err != nil {
		return err
	}
	if current, found := credentials.Projects[cleanup.CurrentProjectUID]; found &&
		current != cleanup.Expected {
		return fmt.Errorf(
			"%w: current project credential differs from managed reservation",
			ErrFederationCredentialConflict,
		)
	}
	aliases := make([]string, 0, 2)
	for projectUID, credential := range credentials.Projects {
		if !credential.ManagedByConfig ||
			strings.TrimSpace(credential.SpokeProjectName) != cleanup.SpokeProjectName {
			continue
		}
		if credential != cleanup.Expected {
			return fmt.Errorf(
				"%w: managed reservation changed before cleanup",
				ErrFederationCredentialConflict,
			)
		}
		aliases = append(aliases, projectUID)
	}
	if len(aliases) == 0 {
		return nil
	}
	for _, projectUID := range aliases {
		delete(credentials.Projects, projectUID)
	}
	return writeFederationCredentials(credentials)
}

// DefaultFederationCredentialStore returns the standalone daemon credential
// store. Embedded services supply their own isolated store.
func DefaultFederationCredentialStore() FederationCredentialStore {
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

// ReserveFederationCredential stores credential only when projectUID is
// absent. An exact existing value is an idempotent success; any distinct value
// is a conflict and remains unchanged.
func ReserveFederationCredential(projectUID string, credential FederationCredential) error {
	return ReserveFederationCredentials(FederationCredentialReservation{
		ProjectUIDs: []string{projectUID},
		Credential:  credential,
	})
}

// ReserveFederationCredentials stores one credential under every requested
// project UID in a single credentials-file update. Exact existing aliases are
// idempotent; any distinct alias conflicts before the file is changed.
func ReserveFederationCredentials(reservation FederationCredentialReservation) error {
	projectUIDs := make([]string, 0, len(reservation.ProjectUIDs))
	seen := make(map[string]struct{}, len(reservation.ProjectUIDs))
	for _, projectUID := range reservation.ProjectUIDs {
		if projectUID == "" {
			return fmt.Errorf("%w: reservation project UID is empty", ErrFederationCredentialConflict)
		}
		if _, ok := seen[projectUID]; ok {
			continue
		}
		seen[projectUID] = struct{}{}
		projectUIDs = append(projectUIDs, projectUID)
	}
	if len(projectUIDs) == 0 {
		return fmt.Errorf("%w: reservation project UIDs are empty", ErrFederationCredentialConflict)
	}

	federationCredentialsMu.Lock()
	defer federationCredentialsMu.Unlock()

	creds, err := readFederationCredentials()
	if err != nil {
		return err
	}
	writeRequired := false
	for _, projectUID := range projectUIDs {
		existing, ok := creds.Projects[projectUID]
		if !ok {
			writeRequired = true
			continue
		}
		if existing != reservation.Credential {
			return fmt.Errorf(
				"%w: reservation target credential differs for project %s",
				ErrFederationCredentialConflict,
				projectUID,
			)
		}
	}
	if !writeRequired {
		return nil
	}
	for _, projectUID := range projectUIDs {
		creds.Projects[projectUID] = reservation.Credential
	}
	return writeFederationCredentials(creds)
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
	return updateFederationCredentialsWithLock(&federationCredentialsMu, update)
}

func updateFederationCredentialsWithLock(
	lock sync.Locker, update func(*FederationCredentials) error,
) error {
	lock.Lock()
	defer lock.Unlock()

	creds, err := readFederationCredentials()
	if err != nil {
		return err
	}
	if err := update(creds); err != nil {
		return err
	}
	return writeFederationCredentials(creds)
}
