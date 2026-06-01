package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// readStorageConfig returns the [storage] block from <KATA_HOME>/config.toml
// using a narrow pre-pass that extracts only [storage] (and [storage.*])
// section lines before decoding. The narrowing is deliberate: KataDSN runs
// on every legacy KATA_DB call site, and routing those through the full
// ReadDaemonConfig would let a typo in any unrelated section (auth, listen,
// close, ...) break callers that never cared about the daemon config.
//
// An absent file returns a zero StorageConfig and nil error. Lines that
// are not part of the [storage] section are skipped entirely; only the
// extracted subset is fed to toml.Decode.
//
// Limitations: heading detection is line-based, so a TOML multi-line
// string containing a leading "[" on its own line could confuse the
// extractor. Operators carrying multi-line DSNs are not a real population;
// the env path remains authoritative in that case.
func readStorageConfig() (StorageConfig, error) {
	path, err := DaemonConfigPath()
	if err != nil {
		return StorageConfig{}, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path derived from KATA_HOME
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StorageConfig{}, nil
	case err != nil:
		return StorageConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	subset := extractStorageSection(data)
	if len(bytes.TrimSpace(subset)) == 0 {
		return StorageConfig{}, nil
	}
	// Decode against a single-field shadow struct so a future [storage]
	// addition we don't yet know about does not leak through meta.Undecoded.
	var shadow struct {
		Storage StorageConfig `toml:"storage"`
	}
	if _, err := toml.Decode(string(subset), &shadow); err != nil {
		return StorageConfig{}, fmt.Errorf("parse %s [storage]: %w", path, err)
	}
	shadow.Storage.DSN = strings.TrimSpace(shadow.Storage.DSN)
	return shadow.Storage, nil
}

// extractStorageSection returns the lines of data that belong to the
// [storage] section (and any [storage.*] subsections). Lines outside any
// section, or inside other top-level sections, are dropped. The result is
// suitable for piping into toml.Decode without dragging in unrelated
// parse errors.
func extractStorageSection(data []byte) []byte {
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Allow long lines (e.g. base64-encoded DSN fragments).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inStorage := false
	for scanner.Scan() {
		line := scanner.Bytes()
		trimmed := bytes.TrimSpace(line)
		if isSectionHeading(trimmed) {
			inStorage = isStorageHeading(trimmed)
		}
		if inStorage {
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// isSectionHeading reports whether the trimmed line is a TOML table heading
// of the form "[name]" or "[name.sub]" or array-of-table "[[name]]". The
// extractor doesn't need to distinguish the two — both reset section state.
func isSectionHeading(trimmed []byte) bool {
	return len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']'
}

// isStorageHeading reports whether the heading is "[storage]" or
// "[storage.<sub>]" (and the array-of-table equivalents). A close-but-not-
// equal heading like "[storaged]" returns false so unrelated sections never
// match.
func isStorageHeading(trimmed []byte) bool {
	// Strip leading "[" / "[[" and trailing "]" / "]]" so the comparison is
	// against the bare name. We don't care which form ([] vs [[]]) was used.
	body := bytes.TrimLeft(trimmed, "[")
	body = bytes.TrimRight(body, "]")
	body = bytes.TrimSpace(body)
	if bytes.Equal(body, []byte("storage")) {
		return true
	}
	return bytes.HasPrefix(body, []byte("storage."))
}
