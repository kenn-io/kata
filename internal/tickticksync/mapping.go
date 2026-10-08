package tickticksync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
)

// ValidateProject refuses archived/non-task sources and explicitly read-only writes.
func ValidateProject(c Config, p Project) error {
	if p.ID != c.ProjectID || ValidateID(p.ID) != nil || p.Closed || p.Kind != "TASK" {
		return fmt.Errorf("TickTick project is unavailable, archived, or not a task project")
	}
	switch p.Permission {
	case "", "read", "write", "comment":
	default:
		return fmt.Errorf("unknown TickTick project permission")
	}
	if c.StatusSync == "two-way" && p.Permission != "" && p.Permission != "write" {
		return fmt.Errorf("TickTick two-way sync requires project write permission")
	}
	return nil
}
func validateTask(c Config, t Task) error {
	if ValidateID(t.ID) != nil || t.ProjectID != c.ProjectID || t.Status == nil {
		return fmt.Errorf("TickTick task identity or status is missing or outside the project")
	}
	if *t.Status != 0 && *t.Status != 2 && *t.Status != -1 {
		return fmt.Errorf("unknown TickTick task status")
	}
	if t.Kind != "" && t.Kind != "TEXT" && t.Kind != "CHECKLIST" && t.Kind != "NOTE" {
		return fmt.Errorf("unknown TickTick task kind")
	}
	for _, v := range []string{t.Title, t.Content, t.Desc, t.Assignee, t.RepeatFlag, t.StartDate, t.DueDate, t.TimeZone, t.CompletedTime} {
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) || len(v) > 1<<20 {
			return fmt.Errorf("invalid or oversized TickTick task content")
		}
	}
	for _, i := range t.Items {
		if !utf8.ValidString(i.Title) || strings.ContainsRune(i.Title, 0) || len(i.Title) > 1<<20 || (i.Status != 0 && i.Status != 1) {
			return fmt.Errorf("invalid TickTick checklist")
		}
	}
	return nil
}

// BuildImportBatch stages stable source versions before the adapter imports them.
// One-way status changes update the fingerprint and durable status observation
// without advancing the source-content timestamp.
func BuildImportBatch(c Config, data ProjectData, old Checkpoint, at time.Time) (db.ImportBatchParams, Checkpoint, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return db.ImportBatchParams{}, old, err
	}
	if err = ValidateProject(c, data.Project); err != nil {
		return db.ImportBatchParams{}, old, err
	}
	if !validTime(at) {
		return db.ImportBatchParams{}, old, fmt.Errorf("TickTick observation time is invalid")
	}
	// The public collection is bounded separately by the client. Preparation may
	// append its bounded recovery window; duplicate rows share one tracked ID.
	if len(data.Tasks) > maxTasks+missingTaskLookupLimit {
		return db.ImportBatchParams{}, old, fmt.Errorf("TickTick observations exceed the collection and recovery limits")
	}
	at = at.UTC().Truncate(time.Millisecond)
	cp := Checkpoint{Versions: map[string]TaskVersion{}, MissingAfter: old.MissingAfter}
	maps.Copy(cp.Versions, old.Versions)
	batch := db.ImportBatchParams{Source: c.SourceKey(), Actor: "ticktick-sync", Items: []db.ImportItem{}, ReconcileLabelsForUnchanged: map[string][]string{}, ReconcileStatusForUnchanged: true, ReconcileStatusForUnchangedContent: map[string]bool{}, ImportStatusObservations: map[string]db.IssueStatusObservation{}}
	seen := map[string]string{}
	total := 0
	for _, task := range data.Tasks {
		if err = validateTask(c, task); err != nil {
			return db.ImportBatchParams{}, old, err
		}
		full, _ := json.Marshal(task, json.Deterministic(true))
		if prev, ok := seen[task.ID]; ok {
			if prev != string(full) {
				return db.ImportBatchParams{}, old, fmt.Errorf("conflicting duplicate TickTick task")
			}
			continue
		}
		seen[task.ID] = string(full)
		if *task.Status == -1 || task.Kind == "NOTE" {
			continue
		}
		fingerprint, err := taskFingerprint(c, task)
		if err != nil {
			return db.ImportBatchParams{}, old, err
		}
		v, exists := cp.Versions[task.ID]
		if !exists {
			v.FirstSeen = at
		}
		advance := !exists
		statusObservationChanged := false
		pendingStatus := false
		if exists && v.Hash != fingerprint {
			if previous, content, ok := decodeTaskFingerprint(v.Hash); ok {
				currentContent, err := taskContentFingerprint(task)
				if err != nil {
					return db.ImportBatchParams{}, old, err
				}
				currentCompletedTime := completedTimeFingerprint(task.CompletedTime)
				statusObservationChanged = previous.status != *task.Status || previous.completedTime != currentCompletedTime
				pendingStatus = previous.pendingStatus
				advance = content != currentContent
			} else {
				previous, unchanged, err := matchingLegacyTaskFingerprint(v.Hash, task)
				if err != nil {
					return db.ImportBatchParams{}, old, err
				}
				statusObservationChanged = previous.status != *task.Status || previous.completedTime != completedTimeFingerprint(task.CompletedTime)
				advance = !unchanged
			}
		}
		if c.StatusSync == "one-way" && (!exists || statusObservationChanged || pendingStatus) {
			externalID := "task:" + task.ID
			if !advance {
				batch.ReconcileStatusForUnchangedContent[externalID] = true
			}
			rawStatus := taskStatusObservationRaw(task)
			batch.ImportStatusObservations[externalID] = db.IssueStatusObservation{Raw: &rawStatus, Version: at}
		}
		v.Hash = fingerprint
		if advance {
			v.Version = at
			if exists && !v.Version.After(cp.Versions[task.ID].Version) {
				v.Version = cp.Versions[task.ID].Version.Add(time.Millisecond)
			}
		}
		cp.Versions[task.ID] = v
		priority, prioritized := map[int]int64{1: 3, 3: 2, 5: 1}[task.Priority]
		if !prioritized && task.Priority != 0 {
			return db.ImportBatchParams{}, old, fmt.Errorf("unknown TickTick priority")
		}
		title := task.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		if c.UseTitlePrefix() {
			title = "[TickTick] " + title
		}
		parts := []string{}
		for _, text := range []string{task.Content, task.Desc} {
			if text != "" {
				parts = append(parts, text)
			}
		}
		for _, i := range task.Items {
			mark := " "
			if i.Status == 1 {
				mark = "x"
			}
			parts = append(parts, "- ["+mark+"] "+i.Title)
		}
		for _, field := range []struct{ name, value string }{{"Start", task.StartDate}, {"Due", task.DueDate}, {"Time zone", task.TimeZone}, {"Recurrence", task.RepeatFlag}} {
			if field.value != "" {
				parts = append(parts, field.name+": "+field.value)
			}
		}
		link := "https://ticktick.com/webapp/#p/" + c.ProjectID + "/tasks/" + task.ID
		body := strings.Join(parts, "\n\n") + "\n\n---\nImported from TickTick: " + link
		item := db.ImportItem{ExternalID: "task:" + task.ID, Title: title, Body: body, Author: "ticktick-unknown", Status: "open", CreatedAt: v.FirstSeen, UpdatedAt: v.Version}
		if prioritized {
			item.Priority = &priority
		}
		if task.Assignee != "" {
			item.Owner = new("ticktick:" + task.Assignee)
		}
		if !c.UseTitlePrefix() {
			item.Labels = []string{"ticktick"}
		}
		if *task.Status == 2 {
			item.Status = "closed"
			item.ClosedReason = new("done")
			closedAt := v.Version
			if batch.ReconcileStatusForUnchangedContent[item.ExternalID] {
				closedAt = at
				if closedAt.Before(v.FirstSeen) {
					closedAt = v.FirstSeen
				}
			}
			item.ClosedAt = new(closedAt)
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return db.ImportBatchParams{}, old, err
		}
		total += len(raw)
		if total > 64<<20 {
			return db.ImportBatchParams{}, old, fmt.Errorf("TickTick import exceeds 64 MiB")
		}
		batch.Items = append(batch.Items, item)
		batch.ReconcileLabelsForUnchanged[item.ExternalID] = []string{"ticktick"}
	}
	if len(cp.Versions) > maxTasks {
		return db.ImportBatchParams{}, old, fmt.Errorf("TickTick checkpoint exceeds 10000 tracked tasks")
	}
	checkpointRaw, err := json.Marshal(cp)
	if err != nil || len(checkpointRaw) > maxCheckpointBytes {
		return db.ImportBatchParams{}, old, fmt.Errorf("TickTick checkpoint exceeds 2 MiB")
	}
	if err = db.ValidateImportBatch(batch); err != nil {
		return db.ImportBatchParams{}, old, err
	}
	return batch, cp, nil
}

func taskStatusObservationRaw(task Task) string {
	// Completion time belongs in the acknowledgement identity so a retry with
	// the same source observation stays idempotent while a changed completion
	// detail remains a new observation. The status prefix is used by recovery.
	return fmt.Sprintf("%d:%x", *task.Status, completedTimeFingerprint(task.CompletedTime))
}

type taskFingerprintObservation struct {
	mode          string
	status        int
	completedTime [8]byte
	// pendingRecovery distinguishes a staged recovery from a finalized observation.
	pendingRecovery bool
	// pendingStatus distinguishes a staged one-way status update from an acknowledged one.
	pendingStatus bool
}

var taskFingerprintMarker = [3]byte{0xb2, 0x02, 0xf1}

// taskFingerprint stores a status-independent content digest and the latest
// mode-specific status observation in the existing 64-character hash field.
// Its layout keeps DecodeCheckpoint's persisted representation unchanged.
func taskFingerprint(c Config, task Task) (string, error) {
	content, err := taskContentFingerprint(task)
	if err != nil {
		return "", err
	}
	status, err := taskStatusCode(*task.Status)
	if err != nil {
		return "", err
	}
	var packed [sha256.Size]byte
	copy(packed[:20], content[:])
	completedTime := completedTimeFingerprint(task.CompletedTime)
	copy(packed[20:28], completedTime[:])
	metadata := byte(1 << 3)
	if c.StatusSync == "two-way" {
		metadata |= 1 << 2
	}
	metadata |= status
	packed[28] = metadata
	copy(packed[29:], taskFingerprintMarker[:])
	return hex.EncodeToString(packed[:]), nil
}

func taskContentFingerprint(task Task) ([20]byte, error) {
	content := task
	content.Status = nil
	content.CompletedTime = ""
	encoded, err := json.Marshal(content, json.Deterministic(true))
	if err != nil {
		return [20]byte{}, err
	}
	hash := sha256.Sum256(encoded)
	var fingerprint [20]byte
	copy(fingerprint[:], hash[:20])
	return fingerprint, nil
}

func completedTimeFingerprint(value string) [8]byte {
	hash := sha256.Sum256([]byte(value))
	var fingerprint [8]byte
	copy(fingerprint[:], hash[:8])
	return fingerprint
}

func taskStatusCode(status int) (byte, error) {
	switch status {
	case 0:
		return 0, nil
	case 2:
		return 1, nil
	case -1:
		return 2, nil
	default:
		return 0, fmt.Errorf("unknown TickTick task status")
	}
}

func decodeTaskFingerprint(value string) (taskFingerprintObservation, [20]byte, bool) {
	var observation taskFingerprintObservation
	var content [20]byte
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != sha256.Size || !bytes.Equal(raw[29:], taskFingerprintMarker[:]) {
		return observation, content, false
	}
	metadata := raw[28]
	version := metadata >> 3
	if version < 1 || version > 4 || metadata&0x3 == 3 {
		return observation, content, false
	}
	observation.pendingRecovery = version == 2 || version == 4
	observation.pendingStatus = version == 3 || version == 4
	if metadata&(1<<2) != 0 {
		observation.mode = "two-way"
	} else {
		observation.mode = "one-way"
	}
	switch metadata & 0x3 {
	case 0:
		observation.status = 0
	case 1:
		observation.status = 2
	case 2:
		observation.status = -1
	}
	copy(content[:], raw[:20])
	copy(observation.completedTime[:], raw[20:28])
	return observation, content, true
}

// markTaskFingerprintRecoveryPending keeps a recovered task's observed content
// version while recording that its final import transaction has not completed.
func markTaskFingerprintRecoveryPending(value string) (string, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != sha256.Size || !bytes.Equal(raw[29:], taskFingerprintMarker[:]) {
		return "", fmt.Errorf("invalid TickTick recovery fingerprint")
	}
	observation, _, ok := decodeTaskFingerprint(value)
	if !ok {
		return "", fmt.Errorf("invalid TickTick recovery fingerprint")
	}
	version := byte(2)
	if observation.pendingStatus {
		version = 4
	}
	raw[28] = raw[28]&0x07 | version<<3
	return hex.EncodeToString(raw), nil
}

func markTaskFingerprintStatusPending(value string) (string, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != sha256.Size || !bytes.Equal(raw[29:], taskFingerprintMarker[:]) {
		return "", fmt.Errorf("invalid TickTick status fingerprint")
	}
	observation, _, ok := decodeTaskFingerprint(value)
	if !ok {
		return "", fmt.Errorf("invalid TickTick status fingerprint")
	}
	version := byte(3)
	if observation.pendingRecovery {
		version = 4
	}
	raw[28] = raw[28]&0x07 | version<<3
	return hex.EncodeToString(raw), nil
}

// matchingLegacyTaskFingerprint recognizes older checkpoints whose hash mixed
// content, status mode, and title-prefix configuration together.
func matchingLegacyTaskFingerprint(hash string, task Task) (taskFingerprintObservation, bool, error) {
	for _, mode := range []string{"one-way", "two-way"} {
		for _, prefix := range []bool{true, false} {
			config := Config{StatusSync: mode, TitlePrefix: new(prefix)}
			if mode == "two-way" {
				legacyHash, hashErr := legacyTaskFingerprint(config, task)
				if hashErr != nil {
					return taskFingerprintObservation{}, false, hashErr
				}
				if hash == legacyHash {
					return taskFingerprintObservation{mode: mode}, true, nil
				}
				continue
			}
			for _, status := range []int{0, 2, -1} {
				for _, completedTime := range []string{task.CompletedTime, ""} {
					candidate := task
					candidate.Status = new(status)
					candidate.CompletedTime = completedTime
					legacyHash, hashErr := legacyTaskFingerprint(config, candidate)
					if hashErr != nil {
						return taskFingerprintObservation{}, false, hashErr
					}
					if hash == legacyHash {
						return taskFingerprintObservation{mode: mode, status: status, completedTime: completedTimeFingerprint(completedTime)}, true, nil
					}
				}
			}
		}
	}
	return taskFingerprintObservation{}, false, nil
}

func legacyTaskFingerprint(c Config, task Task) (string, error) {
	if c.StatusSync == "two-way" {
		task.Status = nil
		task.CompletedTime = ""
	}
	encoded, err := json.Marshal(struct {
		Task   Task
		Prefix bool
		Mode   string
	}{task, c.UseTitlePrefix(), c.StatusSync}, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
