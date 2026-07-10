package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <issue-ref>",
		Short: "show issue + comments",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, args[0], "show")
		},
	}
}

func runShow(cmd *cobra.Command, issueRef, agentOperation string) error {
	ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, issueRef)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	httpStatus, bs, err := httpDoJSON(ctx, client, http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(ref.RefForAPI)), nil)
	if err != nil {
		return err
	}
	if httpStatus >= 400 {
		return apiErrFromBody(httpStatus, bs)
	}
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var b showResponseForCLI
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if mode == outputAgent {
		return printShowAgent(cmd.OutOrStdout(), b, ref.ProjectName, agentOperation)
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "%s  %s  [%s]  by %s\n",
		b.Issue.ShortID,
		textsafe.Line(b.Issue.Title),
		b.Issue.Status,
		textsafe.Line(b.Issue.Author)); err != nil {
		return err
	}
	if err := printShowClaimLines(out, b.Lease, b.PendingLeases, b.LeaseHubNow); err != nil {
		return err
	}
	if err := printShowClaimViolationLines(out, b.LeaseViolations); err != nil {
		return err
	}
	if b.Issue.Body != "" {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, textsafe.Block(b.Issue.Body)); err != nil {
			return err
		}
	}
	if len(b.Comments) > 0 {
		if _, err := fmt.Fprintln(out, "\n--- comments ---"); err != nil {
			return err
		}
		for _, c := range b.Comments {
			if _, err := fmt.Fprintf(out, "%s %s: %s\n",
				textsafe.Line(c.UID), textsafe.Line(c.Author), textsafe.Block(c.Body)); err != nil {
				return err
			}
		}
	}
	if len(b.Labels) > 0 {
		if _, err := fmt.Fprintln(out, "\n--- labels ---"); err != nil {
			return err
		}
		parts := make([]string, 0, len(b.Labels))
		for _, l := range b.Labels {
			parts = append(parts, textsafe.Line(l.Label))
		}
		if _, err := fmt.Fprintln(out, strings.Join(parts, ", ")); err != nil {
			return err
		}
	}
	if len(b.Links) > 0 {
		if _, err := fmt.Fprintln(out, "\n--- links ---"); err != nil {
			return err
		}
		for _, l := range b.Links {
			label, other := linkLabelFromPOV(l.Type, b.Issue.UID, ref.ProjectName, l.From, l.To)
			if _, err := fmt.Fprintf(out, "%s: %s\n", label, other); err != nil {
				return err
			}
		}
	}
	if len(b.Issue.Metadata) > 0 {
		if _, err := fmt.Fprintln(out, "\n--- metadata ---"); err != nil {
			return err
		}
		for _, kv := range sortedMetadata(b.Issue.Metadata) {
			if _, err := fmt.Fprintf(out, "%s = %s\n",
				textsafe.Line(kv.key), textsafe.Line(kv.value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// metadataKV is one rendered metadata entry: the flat key and its value as
// compact JSON (a string value renders with quotes, e.g. "feature/x").
type metadataKV struct {
	key   string
	value string
}

// sortedMetadata returns the metadata map's entries sorted by key for a stable
// render order, each value rendered as compact JSON.
func sortedMetadata(md map[string]json.RawMessage) []metadataKV {
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]metadataKV, 0, len(keys))
	for _, k := range keys {
		out = append(out, metadataKV{key: k, value: compactJSON(md[k])})
	}
	return out
}

// compactJSON renders raw as compact JSON, falling back to the verbatim bytes
// when compaction fails (raw is always valid JSON from the daemon, so the
// fallback is defensive).
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// showResponseForCLI is the show command's decode of the daemon's show
// response. It is shared by both the human and agent renderers; the
// federation lease fields are zero when the issue is not federated.
type showResponseForCLI struct {
	Issue struct {
		ShortID  string                     `json:"short_id"`
		UID      string                     `json:"uid"`
		Title    string                     `json:"title"`
		Body     string                     `json:"body"`
		Status   string                     `json:"status"`
		Author   string                     `json:"author"`
		Owner    *string                    `json:"owner"`
		Priority *int64                     `json:"priority"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	} `json:"issue"`
	Comments []struct {
		UID       string `json:"uid"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	} `json:"comments"`
	Labels []struct {
		Label string `json:"label"`
	} `json:"labels"`
	Links []struct {
		Type string         `json:"type"`
		From linkPeerForCLI `json:"from"`
		To   linkPeerForCLI `json:"to"`
	} `json:"links"`
	Lease           *claimForShowCLI       `json:"lease"`
	PendingLeases   []pendingClaimForCLI   `json:"pending_leases"`
	LeaseHubNow     *time.Time             `json:"lease_hub_now"`
	LeaseViolations []claimViolationForCLI `json:"lease_violations"`
}

func printShowAgent(w io.Writer, b showResponseForCLI, subjectProject, operation string) error {
	if _, err := fmt.Fprintf(w, "OK %s %s\n", operation, b.Issue.ShortID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Issue: %s %s\n", b.Issue.ShortID, agentValue(b.Issue.Title)); err != nil {
		return err
	}
	if b.Issue.Status != "" {
		if err := writeAgentField(w, "Status", agentValue(b.Issue.Status)); err != nil {
			return err
		}
	}
	if b.Issue.Owner != nil && *b.Issue.Owner != "" {
		if err := writeAgentField(w, "Owner", agentValue(*b.Issue.Owner)); err != nil {
			return err
		}
	}
	if len(b.Labels) > 0 {
		labels := make([]string, 0, len(b.Labels))
		for _, l := range b.Labels {
			labels = append(labels, l.Label)
		}
		if err := writeAgentField(w, "Labels", agentValue(strings.Join(labels, ","))); err != nil {
			return err
		}
	}
	if b.Issue.Priority != nil {
		if err := writeAgentField(w, "Priority", fmt.Sprint(*b.Issue.Priority)); err != nil {
			return err
		}
	}
	if len(b.Issue.Metadata) > 0 {
		if _, err := fmt.Fprintln(w, "Metadata:"); err != nil {
			return err
		}
		for _, kv := range sortedMetadata(b.Issue.Metadata) {
			if err := writeAgentKVRow(w,
				agentRowField("key", kv.key),
				agentRowField("value", kv.value),
			); err != nil {
				return err
			}
		}
	}
	if err := printShowAgentLeaseLines(w, b.Lease, b.PendingLeases, b.LeaseHubNow); err != nil {
		return err
	}
	if err := printShowAgentLeaseViolationLines(w, b.LeaseViolations); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, "Body:\n", agentFencedText(b.Issue.Body)); err != nil {
		return err
	}
	if len(b.Comments) > 0 {
		if _, err := fmt.Fprintln(w, "Comments:"); err != nil {
			return err
		}
		for _, c := range b.Comments {
			if err := writeAgentKVRow(w,
				agentRowField("uid", c.UID),
				agentRowField("author", c.Author),
				agentRowField("created_at", c.CreatedAt),
			); err != nil {
				return err
			}
			if _, err := fmt.Fprint(w, agentFencedText(c.Body)); err != nil {
				return err
			}
		}
	}
	if len(b.Links) > 0 {
		if _, err := fmt.Fprintln(w, "Links:"); err != nil {
			return err
		}
		for _, l := range b.Links {
			label, other := linkLabelFromPOV(l.Type, b.Issue.UID, subjectProject, l.From, l.To)
			if err := writeAgentKVRow(w,
				agentRowField("type", label),
				agentRowField("issue", other),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func printShowAgentLeaseLines(
	w io.Writer,
	claim *claimForShowCLI,
	pending []pendingClaimForCLI,
	hubNow *time.Time,
) error {
	if claim != nil {
		if err := writeAgentKVRow(w,
			agentRowField("lease", "active"),
			agentRowField("holder", claim.Holder),
			agentRowField("holder_instance", claim.HolderInstanceUID),
			agentRowField("kind", showClaimKind(claim, hubNow)),
		); err != nil {
			return err
		}
	}
	for _, p := range pending {
		if err := writeAgentKVRow(w,
			agentRowField("lease", "pending"),
			agentRowField("holder", p.Holder),
		); err != nil {
			return err
		}
	}
	return nil
}

func printShowAgentLeaseViolationLines(w io.Writer, violations []claimViolationForCLI) error {
	for _, v := range violations {
		if err := writeAgentKVRow(w,
			agentRowField("lease_violation", v.At.UTC().Format(time.RFC3339)),
			agentRowField("event", v.OffendingEventType),
			agentRowField("actor", v.Actor),
			agentRowField("offending_instance", v.OffendingOriginInstanceUID),
			agentRowField("reason", v.Reason),
		); err != nil {
			return err
		}
	}
	return nil
}

// linkPeerForCLI mirrors api.LinkPeer for the show command's decode path. UID
// is the stable handle; ShortID is the bare human-readable display. Project
// and QualifiedID are always populated (0.2.0) and used for cross-project
// rendering: when the peer's project differs from the subject's, QualifiedID
// is shown instead of ShortID.
type linkPeerForCLI struct {
	UID         string `json:"uid"`
	ShortID     string `json:"short_id"`
	Project     string `json:"project"`
	QualifiedID string `json:"qualified_id"`
}

type claimForShowCLI struct {
	Holder            string     `json:"holder"`
	HolderInstanceUID string     `json:"holder_instance_uid"`
	ClaimKind         string     `json:"claim_kind"`
	ExpiresAt         *time.Time `json:"expires_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type pendingClaimForCLI struct {
	Holder      string    `json:"holder"`
	RequestedAt time.Time `json:"requested_at"`
}

type claimViolationForCLI struct {
	OffendingEventType         string    `json:"offending_event_type"`
	OffendingOriginInstanceUID string    `json:"offending_origin_instance_uid"`
	Actor                      string    `json:"actor"`
	Reason                     string    `json:"reason"`
	At                         time.Time `json:"at"`
}

func printShowClaimLines(
	out interface {
		Write([]byte) (int, error)
	},
	claim *claimForShowCLI,
	pending []pendingClaimForCLI,
	hubNow *time.Time,
) error {
	if claim != nil {
		if _, err := fmt.Fprintf(out, "lease: %s from instance %s (%s)\n",
			textsafe.Line(claim.Holder),
			textsafe.Line(claim.HolderInstanceUID),
			showClaimKind(claim, hubNow)); err != nil {
			return err
		}
	}
	for _, p := range pending {
		if _, err := fmt.Fprintf(out, "lease: %s pending\n", textsafe.Line(p.Holder)); err != nil {
			return err
		}
	}
	return nil
}

func printShowClaimViolationLines(
	out interface {
		Write([]byte) (int, error)
	},
	violations []claimViolationForCLI,
) error {
	for _, v := range violations {
		if _, err := fmt.Fprintf(out, "lease violation: %s %s by %s from instance %s (%s)\n",
			v.At.UTC().Format(time.RFC3339),
			textsafe.Line(v.OffendingEventType),
			textsafe.Line(v.Actor),
			textsafe.Line(v.OffendingOriginInstanceUID),
			textsafe.Line(v.Reason)); err != nil {
			return err
		}
	}
	return nil
}

func showClaimKind(claim *claimForShowCLI, hubNow *time.Time) string {
	if claim.ClaimKind != "timed" || claim.ExpiresAt == nil {
		return "hard"
	}
	now := time.Now().UTC()
	if hubNow != nil && !hubNow.IsZero() {
		now = hubNow.UTC()
	}
	return fmt.Sprintf("timed, %s left", formatClaimTimeLeft(claim.ExpiresAt.Sub(now)))
}

func formatClaimTimeLeft(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		hours := int(d / time.Hour)
		minutes := int((d % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// peerRefForDisplay renders a peer's ref bare when it belongs to the same
// project as the subject issue, and qualified ("project#short_id") otherwise.
// QualifiedID embeds a project name — user-supplied data that can reach the
// DB unvalidated through crafted import envelopes — and this helper feeds
// human terminal sinks only (show link lines, create/edit echoes), so it
// sanitizes like every other user-authored field. JSON paths marshal the
// wire structs directly and keep the daemon's raw bytes.
func peerRefForDisplay(p linkPeerForCLI, subjectProject string) string {
	if p.Project != "" && p.Project != subjectProject {
		return textsafe.Line(p.QualifiedID)
	}
	return textsafe.Line(p.ShortID)
}

// linkLabelFromPOV returns the label and the OTHER endpoint's display ref,
// framed from the viewing issue's point of view. The display matches the
// relationship-flag vocabulary on `kata create` / `kata edit`: "parent" /
// "child" for the parent slot, "blocks" / "blocked-by" for the directed
// blocks edge, and "related" for the symmetric one. subjectProject is the
// project name of the issue being shown; it is used to render foreign peers
// qualified while same-project peers stay bare.
func linkLabelFromPOV(linkType, viewerUID, subjectProject string, from, to linkPeerForCLI) (label, other string) {
	if from.UID == viewerUID {
		switch linkType {
		case "parent":
			return "parent", peerRefForDisplay(to, subjectProject)
		case "blocks":
			return "blocks", peerRefForDisplay(to, subjectProject)
		case "related":
			return "related", peerRefForDisplay(to, subjectProject)
		default:
			return linkType, peerRefForDisplay(to, subjectProject)
		}
	}
	switch linkType {
	case "parent":
		return "child", peerRefForDisplay(from, subjectProject)
	case "blocks":
		return "blocked-by", peerRefForDisplay(from, subjectProject)
	case "related":
		return "related", peerRefForDisplay(from, subjectProject)
	default:
		return linkType, peerRefForDisplay(from, subjectProject)
	}
}
