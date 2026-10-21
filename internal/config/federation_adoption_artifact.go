package config

import (
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const federationAdoptionEnvelopeLimit = 64 << 20

// WriteFederationAdoptionEnvelope durably saves opaque publication bytes in an
// owner-only file. The caller validates and canonicalizes the envelope first,
// and must never replace an artifact after transmission may have begun.
func WriteFederationAdoptionEnvelope(home, operationUID string, contents []byte) (string, error) {
	if len(contents) > federationAdoptionEnvelopeLimit {
		return "", errors.New("federation adoption envelope exceeds 64 MiB")
	}
	path, err := federationAdoptionEnvelopePath(home, operationUID, true)
	if err != nil {
		return "", err
	}
	// Kit fsyncs the private staged file before renaming it, then syncs its
	// directory. In particular, propagate ErrNotDurable rather than treating a
	// visible replacement as a successfully frozen artifact.
	if err := atomicfile.WriteFile(path, contents, atomicfile.WithPrivate()); err != nil {
		return "", fmt.Errorf("save federation adoption envelope: %w", err)
	}
	return path, nil
}

// ReadFederationAdoptionEnvelope reads at most 64 MiB from a private regular
// file and checks its declared digest against the saved descriptor. This is
// only a header consistency check: the daemon must Decode the returned bytes
// with federationpublication to validate the full envelope and canonical digest
// before every transmission. The digest is not a hash of the entire file.
func ReadFederationAdoptionEnvelope(home, operationUID, digest string) ([]byte, error) {
	if len(digest) != 64 {
		return nil, errors.New("invalid federation adoption digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, errors.New("invalid federation adoption digest")
	}
	path, err := federationAdoptionEnvelopePath(home, operationUID, false)
	if err != nil {
		return nil, err
	}
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > federationAdoptionEnvelopeLimit {
		return nil, errors.New("federation adoption envelope exceeds 64 MiB")
	}
	contents, err := io.ReadAll(io.LimitReader(file, federationAdoptionEnvelopeLimit+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > federationAdoptionEnvelopeLimit {
		return nil, errors.New("federation adoption envelope exceeds 64 MiB")
	}
	var header struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(contents, &header); err != nil || header.Digest != digest {
		return nil, errors.New("federation adoption envelope digest does not match the saved operation")
	}
	return contents, nil
}

// RemoveFederationAdoptionEnvelope removes only the named regular artifact,
// without reading it or creating directories. Missing artifacts are harmless.
// Call only after the native operation fence has been released or cancelled.
func RemoveFederationAdoptionEnvelope(home, operationUID string) error {
	path, err := federationAdoptionEnvelopePath(home, operationUID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("federation adoption envelope is not a regular file")
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(path))
}

func federationAdoptionEnvelopePath(home, operationUID string, create bool) (string, error) {
	if strings.TrimSpace(home) == "" || !uid.Valid(operationUID) {
		return "", errors.New("federation adoption requires a home and valid operation UID")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	dir := home
	for _, component := range []string{"federation", "adoptions"} {
		dir = filepath.Join(dir, component)
		if create {
			if err := safefileio.EnsurePrivateDirShallow(dir); err != nil {
				return "", err
			}
			// Persist each directory entry as well as the final file. Repeating
			// this on retries also covers a crash during directory creation.
			if err := atomicfile.SyncDir(filepath.Dir(dir)); err != nil {
				return "", err
			}
		} else if err := safefileio.ValidatePrivateDir(dir); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, operationUID+".json"), nil
}
