package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	beadsSource               = "beads"
	maxBeadsCommentsJSONBytes = 16 * 1024 * 1024
)

type beadsIssue struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	Priority     int               `json:"priority"`
	IssueType    string            `json:"issue_type"`
	Owner        string            `json:"owner"`
	CreatedAt    time.Time         `json:"created_at"`
	CreatedBy    string            `json:"created_by"`
	UpdatedAt    time.Time         `json:"updated_at"`
	ClosedAt     *time.Time        `json:"closed_at"`
	CloseReason  string            `json:"close_reason"`
	Labels       []string          `json:"labels"`
	Dependencies []beadsDependency `json:"dependencies"`
	CommentCount int               `json:"comment_count"`
}

type beadsDependency struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	CreatedBy   string `json:"created_by"`
	Metadata    string `json:"metadata"`
	CreatedAt   string `json:"created_at"`
}

type beadsComment struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type beadsImportRequest struct {
	Actor  string                  `json:"actor"`
	Source string                  `json:"source"`
	Items  []beadsImportIssueInput `json:"items"`
}

type beadsImportIssueInput struct {
	ExternalID   string                    `json:"external_id"`
	Title        string                    `json:"title"`
	Body         string                    `json:"body"`
	Author       string                    `json:"author"`
	Owner        *string                   `json:"owner,omitempty"`
	Priority     *int64                    `json:"priority,omitempty"`
	Status       string                    `json:"status"`
	ClosedReason *string                   `json:"closed_reason,omitempty"`
	CreatedAt    time.Time                 `json:"created_at"`
	UpdatedAt    time.Time                 `json:"updated_at"`
	ClosedAt     *time.Time                `json:"closed_at,omitempty"`
	Labels       []string                  `json:"labels,omitempty"`
	Comments     []beadsImportCommentInput `json:"comments,omitempty"`
	Links        []beadsImportLinkInput    `json:"links,omitempty"`
}

type beadsImportCommentInput struct {
	ExternalID string    `json:"external_id"`
	Author     string    `json:"author"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

type beadsImportLinkInput struct {
	Type             string `json:"type"`
	TargetExternalID string `json:"target_external_id"`
}

var (
	invalidLabelChar = regexp.MustCompile(`[^a-z0-9._:-]+`)
	repeatedDash     = regexp.MustCompile(`-+`)
)

type beadsImportSummary struct {
	Source    string `json:"source"`
	Created   int    `json:"created"`
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Comments  int    `json:"comments"`
	Links     int    `json:"links"`
}

func runBeadsImport(cmd *cobra.Command) error {
	ctx := cmd.Context()
	workspace, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return err
	}
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return err
	}
	projectID, err := resolveProjectID(ctx, baseURL, workspace)
	if err != nil {
		projectID, err = resolveBeadsProjectOrInit(cmd, baseURL, workspace, err)
		if err != nil {
			return err
		}
	}

	actor, _ := resolveActor(flags.As, nil)
	req, err := collectBeadsImportRequest(ctx, workspace, actor)
	if err != nil {
		return err
	}

	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/imports", baseURL, projectID), req)
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	return printBeadsImportResult(cmd, bs, projectID)
}

func collectBeadsImportRequest(ctx context.Context, workspace, actor string) (beadsImportRequest, error) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return beadsImportRequest{}, &cliError{
			Message:  "beads import requires bd on PATH",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	exportData, err := runBD(ctx, workspace, bdPath, "export", "--no-memories")
	if err != nil {
		return beadsImportRequest{}, err
	}
	issues, err := parseBeadsExport(bytes.NewReader(exportData))
	if err != nil {
		return beadsImportRequest{}, err
	}
	comments := make(map[string][]beadsComment, len(issues))
	for _, issue := range issues {
		data, err := runBD(ctx, workspace, bdPath, "comments", issue.ID, "--json")
		if err != nil {
			return beadsImportRequest{}, err
		}
		parsed, err := parseBeadsCommentsJSON(bytes.NewReader(data))
		if err != nil {
			return beadsImportRequest{}, err
		}
		comments[issue.ID] = parsed
	}
	return buildBeadsImportRequest(bytes.NewReader(exportData), comments, actor)
}

func runBD(ctx context.Context, workspace, bdPath string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bdPath, args...) //nolint:gosec // bd path comes from PATH lookup and args are fixed by kata.
	cmd.Dir = workspace
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("bd %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

func resolveBeadsProjectOrInit(cmd *cobra.Command, baseURL, start string, err error) (int64, error) {
	var ce *cliError
	if !errors.As(err, &ce) || ce.Code != "project_not_initialized" {
		return 0, err
	}
	if isBeadsImportUnattended(cmd) {
		return 0, beadsInitRequiredError()
	}
	ok, promptErr := confirmBeadsInit(cmd)
	if promptErr != nil {
		return 0, promptErr
	}
	if !ok {
		return 0, beadsInitRequiredError()
	}
	out, initErr := callInit(cmd.Context(), baseURL, start, callInitOpts{})
	if initErr != nil {
		return 0, initErr
	}
	if !flags.Quiet {
		if _, writeErr := fmt.Fprint(cmd.OutOrStdout(), out); writeErr != nil {
			return 0, writeErr
		}
	}
	return resolveProjectID(cmd.Context(), baseURL, start)
}

func confirmBeadsInit(cmd *cobra.Command) (bool, error) {
	if _, err := fmt.Fprint(cmd.OutOrStdout(), "No kata project found. Run kata init now? [y/N] "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func beadsInitRequiredError() error {
	return &cliError{Message: "run kata init first", Kind: kindValidation, ExitCode: ExitValidation}
}

func isBeadsImportUnattended(cmd *cobra.Command) bool {
	if currentOutputMode() != outputHuman || flags.Quiet {
		return true
	}
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isTTY(in) {
		return true
	}
	out, ok := cmd.OutOrStdout().(*os.File)
	return !ok || !isTTY(out)
}

func printBeadsImportResult(cmd *cobra.Command, bs []byte, projectID int64) error {
	if flags.JSON {
		_, err := cmd.OutOrStdout().Write(bs)
		return err
	}
	if flags.Quiet {
		return nil
	}
	var summary beadsImportSummary
	if err := json.Unmarshal(bs, &summary); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"OK import source_format=beads project=%d created=%d updated=%d unchanged=%d comments=%d links=%d\n",
			projectID, summary.Created, summary.Updated, summary.Unchanged, summary.Comments, summary.Links)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"imported beads: created %d, updated %d, unchanged %d, comments %d, links %d\n",
		summary.Created, summary.Updated, summary.Unchanged, summary.Comments, summary.Links)
	return err
}

func parseBeadsExport(r io.Reader) ([]beadsIssue, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []beadsIssue
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var issue beadsIssue
		if err := json.Unmarshal([]byte(line), &issue); err != nil {
			return nil, fmt.Errorf("decode beads export: %w", err)
		}
		out = append(out, issue)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan beads export: %w", err)
	}
	return out, nil
}

func parseBeadsCommentsJSON(r io.Reader) ([]beadsComment, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBeadsCommentsJSONBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read beads comments: %w", err)
	}
	if len(data) > maxBeadsCommentsJSONBytes {
		return nil, &cliError{
			Message:  fmt.Sprintf("beads comments JSON exceeds %d byte limit", maxBeadsCommentsJSONBytes),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var comments []beadsComment
	if err := json.Unmarshal(data, &comments); err == nil {
		return comments, nil
	}
	var wrapped struct {
		Comments []beadsComment `json:"comments"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("decode beads comments: %w", err)
	}
	return wrapped.Comments, nil
}

func buildBeadsImportRequest(r io.Reader, comments map[string][]beadsComment, actor string) (beadsImportRequest, error) {
	issues, err := parseBeadsExport(r)
	if err != nil {
		return beadsImportRequest{}, err
	}

	req := beadsImportRequest{Actor: actor, Source: beadsSource, Items: make([]beadsImportIssueInput, 0, len(issues))}
	indexByID := make(map[string]int, len(issues))
	for _, b := range issues {
		rawStatus := strings.TrimSpace(b.Status)
		status := mapBeadsStatus(rawStatus)

		labels := []string{"source:beads", beadsIDLabel(b.ID)}
		seenLabels := map[string]struct{}{}
		labels = appendNormalizedLabels(nil, seenLabels, labels...)
		// Preserve the original beads status as a label whenever the
		// raw value isn't trivially "open" or "closed" — keeps the
		// "blocked"/"in_progress"/etc. distinction visible after the
		// kata-side status collapse to open/closed.
		if rawStatus != "" && rawStatus != "open" && rawStatus != "closed" {
			labels = appendNormalizedLabels(labels, seenLabels, "beads-status:"+rawStatus)
		}
		for _, label := range b.Labels {
			labels = appendNormalizedLabels(labels, seenLabels, label)
		}

		var owner *string
		if trimmed := strings.TrimSpace(b.Owner); trimmed != "" {
			owner = &trimmed
		}
		author := strings.TrimSpace(b.CreatedBy)
		if author == "" {
			author = actor
		}
		if strings.TrimSpace(b.Title) == "" {
			return beadsImportRequest{}, fmt.Errorf("beads issue %q missing title", b.ID)
		}

		closedAt := b.ClosedAt
		var closedReason *string
		if status == "closed" {
			mapped := mapBeadsCloseReason(b.CloseReason)
			closedReason = &mapped
			if closedAt == nil {
				updatedAt := b.UpdatedAt
				closedAt = &updatedAt
			}
		}

		item := beadsImportIssueInput{
			ExternalID:   b.ID,
			Title:        b.Title,
			Body:         strings.TrimRight(b.Description, "\n") + beadsFooter(b),
			Author:       author,
			Owner:        owner,
			Priority:     mapBeadsPriority(b.Priority),
			Status:       status,
			ClosedReason: closedReason,
			CreatedAt:    b.CreatedAt,
			UpdatedAt:    b.UpdatedAt,
			ClosedAt:     closedAt,
			Labels:       labels,
		}
		for _, c := range comments[b.ID] {
			commentAuthor := strings.TrimSpace(c.Author)
			if commentAuthor == "" {
				commentAuthor = actor
			}
			commentBody := c.Text
			if commentBody == "" {
				commentBody = c.Body
			}
			item.Comments = append(item.Comments, beadsImportCommentInput{
				ExternalID: c.ID,
				Author:     commentAuthor,
				Body:       commentBody,
				CreatedAt:  c.CreatedAt,
			})
		}
		req.Items = append(req.Items, item)
		indexByID[b.ID] = len(req.Items) - 1
	}

	for _, b := range issues {
		for _, dep := range b.Dependencies {
			if strings.TrimSpace(dep.DependsOnID) == "" {
				continue
			}
			idx, ok := indexByID[dep.DependsOnID]
			if !ok {
				return beadsImportRequest{}, fmt.Errorf("beads dependency target %q for %s not found in export", dep.DependsOnID, b.ID)
			}
			req.Items[idx].Links = append(req.Items[idx].Links, beadsImportLinkInput{Type: "blocks", TargetExternalID: b.ID})
		}
	}

	return req, nil
}

func appendNormalizedLabels(out []string, seen map[string]struct{}, labels ...string) []string {
	for _, label := range labels {
		normalized := normalizeKataLabel(label)
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func normalizeKataLabel(s string) string {
	return normalizeKataLabelMax(s, 64)
}

func normalizeKataLabelMax(s string, maxLen int) string {
	normalized := strings.ToLower(strings.TrimSpace(s))
	normalized = strings.Join(strings.Fields(normalized), "-")
	normalized = invalidLabelChar.ReplaceAllString(normalized, "-")
	normalized = repeatedDash.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-._:")
	if normalized == "" {
		normalized = "imported"
	}
	if len(normalized) <= maxLen {
		return normalized
	}
	sum := sha256.Sum256([]byte(normalized))
	suffix := hex.EncodeToString(sum[:])[:8]
	prefixLen := maxLen - len(suffix) - 1
	if prefixLen < 1 {
		return suffix[:maxLen]
	}
	prefix := strings.TrimRight(normalized[:prefixLen], "-._:")
	if prefix == "" {
		prefix = "imported"
	}
	return prefix + "-" + suffix
}

func beadsIDLabel(id string) string {
	const prefix = "beads-id:"
	return prefix + normalizeKataLabelMax(id, 64-len(prefix))
}

func mapBeadsCloseReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "done", "wontfix", "duplicate":
		return strings.TrimSpace(reason)
	default:
		return "done"
	}
}

// mapBeadsStatus collapses beads' richer status vocabulary
// (open / in_progress / blocked / closed / merged / etc.) into
// kata's binary open|closed. Empty string defaults to open.
// Anything that looks terminal — "closed", "done", "merged", "wontfix",
// "duplicate" — maps to closed; everything else (open, in_progress,
// blocked, ready, triage, future statuses we haven't seen yet) maps to
// open. The original raw status is preserved as a "beads-status:<x>"
// label by the caller when it isn't trivially open/closed.
func mapBeadsStatus(raw string) string {
	switch raw {
	case "", "open", "in_progress", "blocked", "ready", "triage", "todo", "doing":
		return "open"
	case "closed", "done", "merged", "wontfix", "duplicate", "resolved":
		return "closed"
	default:
		// Conservative default: unknown beads statuses ride into kata as
		// open so the import keeps making forward progress. The raw
		// value is captured in the beads-status:<raw> label.
		return "open"
	}
}

func beadsFooter(b beadsIssue) string {
	labels, err := json.Marshal(b.Labels)
	if err != nil {
		labels = []byte("[]")
	}
	closedAt := ""
	if b.ClosedAt != nil {
		closedAt = b.ClosedAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("\n---\nImported from Beads\nbeads_id: %s\nbeads_type: %s\nbeads_original_labels: %s\nbeads_created_at: %s\nbeads_updated_at: %s\nbeads_closed_at: %s\nbeads_close_reason: %s\nbeads_comment_count: %d\n",
		b.ID,
		b.IssueType,
		string(labels),
		b.CreatedAt.Format(time.RFC3339Nano),
		b.UpdatedAt.Format(time.RFC3339Nano),
		closedAt,
		b.CloseReason,
		b.CommentCount,
	)
}

// mapBeadsPriority maps the beads priority integer (0-N) to a kata
// priority pointer (0..4 or nil). Beads ships values 0..3 (critical /
// high / medium / low) by convention; kata ranges 0..4 (0 = highest).
// We pass a value through verbatim when it lands inside the kata range
// and drop priorities outside [0..4] to nil rather than rejecting the
// whole import — preserves migration progress when a Beads workspace
// has stray data.
func mapBeadsPriority(p int) *int64 {
	if p < 0 || p > 4 {
		return nil
	}
	v := int64(p)
	return &v
}
