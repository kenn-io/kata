package client

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

// PrivateDirError reports a local runtime directory that failed
// private-directory validation. Ordinary discovery repairs that directory;
// read-only discovery returns this error instead, so callers can tell it apart
// from a stopped or unreachable daemon. Its message is the validation error's
// message.
type PrivateDirError struct {
	Err error
}

func (e *PrivateDirError) Error() string { return e.Err.Error() }

func (e *PrivateDirError) Unwrap() error { return e.Err }

// readRuntimeRecords preserves RuntimeStore.List's ordering, filename/PID and
// ownership checks without its implicit directory creation or permission repair.
// Kit has no read-only listing API. Replace this reader when one is available;
// Kata requires JSON v2, so malformed JSON handling can still differ from Kit.
func readRuntimeRecords(dir string) ([]kitdaemon.RuntimeRecord, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("runtime dir %q must be absolute", dir)
	}
	if err := safefileio.ValidatePrivateDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, &PrivateDirError{Err: err}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var records []kitdaemon.RuntimeRecord
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "daemon.") || !strings.HasSuffix(name, ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "daemon."), ".json"))
		if err != nil || pid <= 0 {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := safefileio.OpenCurrentUserFile(path)
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(file)
		_ = file.Close()
		var record kitdaemon.RuntimeRecord
		// Match Kit's case-insensitive field names while using Kata's JSON decoder.
		if readErr != nil || json.Unmarshal(data, &record, json.MatchCaseInsensitiveNames(true)) != nil || record.PID != pid || record.Address == "" {
			continue
		}
		record.SourcePath = path
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].PID < records[j].PID
		}
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}
