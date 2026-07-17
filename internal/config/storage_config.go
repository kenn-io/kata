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
	// Decode against a single-field shadow struct. Since the pre-pass removed
	// unrelated sections, every undecoded key here is an unsafe storage typo.
	var shadow struct {
		Storage StorageConfig `toml:"storage"`
	}
	metadata, err := toml.Decode(string(subset), &shadow)
	if err != nil {
		return StorageConfig{}, fmt.Errorf("parse %s [storage]: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		return StorageConfig{}, fmt.Errorf("parse %s [storage]: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	shadow.Storage.DSN = strings.TrimSpace(shadow.Storage.DSN)
	shadow.Storage.Postgres.Schema = strings.TrimSpace(shadow.Storage.Postgres.Schema)
	shadow.Storage.Postgres.Mode = strings.TrimSpace(shadow.Storage.Postgres.Mode)
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
		if heading, ok := sectionHeading(trimmed); ok {
			inStorage = isStorageHeading(heading)
		}
		if inStorage {
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// sectionHeading returns a TOML table heading without its trailing comment.
// It recognizes closing brackets only outside quoted key segments so valid
// headings such as [storage."regional]db"] remain intact.
func sectionHeading(trimmed []byte) ([]byte, bool) {
	if len(trimmed) < 2 || trimmed[0] != '[' {
		return nil, false
	}
	arrayTable := len(trimmed) > 1 && trimmed[1] == '['
	start := 1
	if arrayTable {
		start = 2
	}
	var inBasic, inLiteral, escaped bool
	for index := start; index < len(trimmed); index++ {
		char := trimmed[index]
		if inBasic {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				inBasic = false
			}
			continue
		}
		if inLiteral {
			if char == '\'' {
				inLiteral = false
			}
			continue
		}
		switch char {
		case '"':
			inBasic = true
		case '\'':
			inLiteral = true
		case ']':
			end := index + 1
			if arrayTable {
				if end >= len(trimmed) || trimmed[end] != ']' {
					continue
				}
				end++
			}
			remainder := bytes.TrimSpace(trimmed[end:])
			if len(remainder) != 0 && remainder[0] != '#' {
				return nil, false
			}
			return trimmed[:end], true
		}
	}
	return nil, false
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
