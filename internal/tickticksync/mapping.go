package tickticksync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"strconv"
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

// validateTaskIdentity checks what a status read needs: scope, status, and kind.
func validateTaskIdentity(c Config, t Task) error {
	if ValidateID(t.ID) != nil || t.ProjectID != c.ProjectID || t.Status == nil {
		return fmt.Errorf("TickTick task identity or status is missing or outside the project")
	}
	if *t.Status != 0 && *t.Status != 2 && *t.Status != -1 {
		return fmt.Errorf("unknown TickTick task status")
	}
	if t.Kind != "" && t.Kind != "TEXT" && t.Kind != "CHECKLIST" && t.Kind != "NOTE" {
		return fmt.Errorf("unknown TickTick task kind")
	}
	return nil
}

// validateTask also bounds the content an import copies into Kata.
func validateTask(c Config, t Task) error {
	if err := validateTaskIdentity(c, t); err != nil {
		return err
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
	batch := db.ImportBatchParams{Source: c.SourceKey(), Actor: "ticktick-sync", Items: []db.ImportItem{}, ReconcileLabelsForUnchanged: map[string][]string{}, ReconcileStatusForUnchanged: true, ImportStatusObservations: map[string]db.IssueStatusObservation{}}
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
		contentHash, err := taskContentHash(task)
		if err != nil {
			return db.ImportBatchParams{}, old, err
		}
		taskAt := at
		if !task.observedAt.IsZero() {
			taskAt = task.observedAt.UTC().Truncate(time.Millisecond)
		}
		v, exists := cp.Versions[task.ID]
		if !exists {
			v.FirstSeen = taskAt
		}
		advance := !exists || v.Hash != contentHash
		statusOnly := false
		// One-way sends the current status every run. The store ignores a repeat of
		// the recorded observation, so local changes stay until TickTick changes,
		// even when another mode already saved this status in the checkpoint.
		if c.StatusSync == "one-way" {
			statusOnly = !advance
			rawStatus := taskStatusRaw(task)
			batch.ImportStatusObservations["task:"+task.ID] = db.IssueStatusObservation{Raw: &rawStatus, Version: taskAt}
		}
		v.Hash, v.Status, v.CompletedTime = contentHash, *task.Status, task.CompletedTime
		v.PendingRecovery = false
		if advance {
			v.Version = taskAt
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
			if statusOnly {
				closedAt = taskAt
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

// taskStatusRaw is the observed_status value for one TickTick status. Both the
// one-way import and the two-way status pass record it, so a retry of the same
// observation is idempotent while a new completion time is a new observation.
func taskStatusRaw(task Task) string {
	if task.CompletedTime == "" {
		return strconv.Itoa(*task.Status)
	}
	return strconv.Itoa(*task.Status) + "@" + task.CompletedTime
}

// taskContentHash covers imported content only. Status, completion time, and
// binding presentation settings never change it.
func taskContentHash(task Task) (string, error) {
	task.Status = nil
	task.CompletedTime = ""
	encoded, err := json.Marshal(task, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
