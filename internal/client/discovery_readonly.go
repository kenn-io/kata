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

// readRuntimeRecords preserves RuntimeStore.List's ordering, filename/PID and
// ownership checks without its implicit directory creation or permission repair.
func readRuntimeRecords(dir string) ([]kitdaemon.RuntimeRecord, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("runtime dir %q must be absolute", dir)
	}
	if err := safefileio.ValidatePrivateDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var records []kitdaemon.RuntimeRecord
	for _, entry := range entries {
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
		data, readErr := io.ReadAll(io.LimitReader(file, 1<<20))
		_ = file.Close()
		var record kitdaemon.RuntimeRecord
		if readErr != nil || json.Unmarshal(data, &record) != nil || record.PID != pid || record.Address == "" {
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
