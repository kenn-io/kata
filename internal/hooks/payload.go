package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

// Per-field byte caps from spec §7.2. issue.body is not in the IssueSnapshot
// surface in v1; the spec lists it for forward compatibility but the
// envelope assembler does not include or truncate it today.
const (
	maxStdinBytes       = 256 * 1024
	maxIssueTitleBytes  = 1 * 1024
	maxCommentBodyBytes = 8 * 1024
)

// Resolver function types match DispatcherDeps in dispatcher.go (Task 8).
// They live here so payload.go has no compile-time dependency on the
// dispatcher package surface.
type (
	projectResolver func(context.Context, int64) (ProjectSnapshot, error)
	issueResolver   func(context.Context, int64) (IssueSnapshot, error)
	commentResolver func(context.Context, int64) (CommentSnapshot, error)
	aliasResolver   func(context.Context, db.Event) (AliasSnapshot, bool, error)
	logfn           func(format string, args ...any)
)

// buildStdinJSON assembles the §7 envelope. Returns (bytes, truncated)
// where truncated is true if total exceeded 256KB after per-field caps;
// the fallback dropped fields to try to fit, but the returned bytes may
// still exceed the cap if even the minimal envelope is over.
func buildStdinJSON(
	ctx context.Context,
	evt db.Event,
	rp projectResolver,
	ri issueResolver,
	rc commentResolver,
	ra aliasResolver,
	log logfn,
) ([]byte, bool) {
	env := baseEnvelope(evt)
	env["project"] = buildProjectBlock(ctx, evt, rp, log)
	if a := buildAliasBlock(ctx, evt, ra, log); a != nil {
		env["alias"] = a
	}
	if i := buildIssueBlock(ctx, evt, ri, log); i != nil {
		env["issue"] = i
	}
	if p := buildPayloadBlock(ctx, evt, rc, log); len(p) > 0 {
		env["payload"] = p
	}
	return marshalWithFallback(env)
}

func baseEnvelope(evt db.Event) map[string]any {
	return map[string]any{
		"kata_hook_version": 1,
		"event_id":          evt.ID,
		"type":              evt.Type,
		"actor":             evt.Actor,
		"created_at":        evt.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func buildProjectBlock(ctx context.Context, evt db.Event, rp projectResolver, log logfn) map[string]any {
	proj := map[string]any{"id": evt.ProjectID, "name": evt.ProjectName}
	if ps, err := rp(ctx, evt.ProjectID); err != nil {
		log("hooks: project resolver: %v", err)
	} else {
		proj["name"] = ps.Name
	}
	return proj
}

func buildAliasBlock(ctx context.Context, evt db.Event, ra aliasResolver, log logfn) map[string]any {
	asnap, has, err := ra(ctx, evt)
	if err != nil {
		log("hooks: alias resolver: %v", err)
		return nil
	}
	if !has {
		return nil
	}
	return map[string]any{
		"alias_identity": asnap.Identity,
		"alias_kind":     asnap.Kind,
		"root_path":      asnap.RootPath,
	}
}

func buildIssueBlock(ctx context.Context, evt db.Event, ri issueResolver, log logfn) map[string]any {
	if evt.IssueID == nil {
		return nil
	}
	isnap, err := ri(ctx, *evt.IssueID)
	if err != nil {
		log("hooks: issue resolver: %v", err)
		return nil
	}
	block := map[string]any{
		"uid":      isnap.UID,
		"short_id": isnap.ShortID,
		"status":   isnap.Status,
		"owner":    isnap.Owner,
		"author":   isnap.Author,
		"labels":   isnap.Labels,
	}
	truncStringField(block, "title", isnap.Title, maxIssueTitleBytes)
	return block
}

func buildPayloadBlock(ctx context.Context, evt db.Event, rc commentResolver, log logfn) map[string]any {
	payload := map[string]any{}
	if len(evt.Payload) > 0 {
		var raw map[string]any
		if err := json.Unmarshal([]byte(evt.Payload), &raw); err != nil {
			log("hooks: payload unmarshal: %v", err)
		} else {
			for k, v := range raw {
				payload[k] = v
			}
		}
	}
	if evt.Type == "issue.commented" {
		if body, ok := payload["body"].(string); ok {
			truncStringField(payload, "comment_body", body, maxCommentBodyBytes)
		} else if cidF, ok := payload["comment_id"].(float64); ok {
			cid := int64(cidF)
			if csnap, err := rc(ctx, cid); err != nil {
				log("hooks: comment resolver: %v", err)
			} else {
				truncStringField(payload, "comment_body", csnap.Body, maxCommentBodyBytes)
			}
		}
	}
	return payload
}

func marshalWithFallback(env map[string]any) ([]byte, bool) {
	out, _ := json.Marshal(env)
	if len(out) <= maxStdinBytes {
		return out, false
	}
	env["payload_truncated"] = true
	for _, key := range []string{"payload", "issue.title"} {
		if dropOptional(env, key) {
			out, _ = json.Marshal(env)
			if len(out) <= maxStdinBytes {
				return out, true
			}
		}
	}
	return out, true
}

// truncStringField puts (possibly truncated) value at key, marking the
// parent block with _truncated/_full_size when the byte length exceeds
// limit. Cuts on a rune boundary so the truncated string is always
// valid UTF-8 — important for downstream JSON consumers that may treat
// fields as Unicode-text rather than opaque bytes.
func truncStringField(parent map[string]any, key, value string, limit int) {
	if len(value) <= limit {
		parent[key] = value
		return
	}
	end := limit
	// UTF-8 continuation bytes have the bit pattern 10xxxxxx (0x80-0xBF).
	// Walk back to find the start of the rune that straddles `limit`.
	for end > 0 && (value[end]&0xC0) == 0x80 {
		end--
	}
	parent[key] = value[:end]
	parent["_truncated"] = true
	parent["_full_size"] = len(value)
}

// dropOptional removes the addressed field. Supports "payload" (top-level
// removal) and dotted keys like "issue.title" (sub-field removal). Returns
// true when the key existed and was removed.
func dropOptional(env map[string]any, addr string) bool {
	parts := strings.SplitN(addr, ".", 2)
	if len(parts) == 1 {
		if _, ok := env[parts[0]]; !ok {
			return false
		}
		delete(env, parts[0])
		return true
	}
	parent, ok := env[parts[0]].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := parent[parts[1]]; !ok {
		return false
	}
	delete(parent, parts[1])
	return true
}
