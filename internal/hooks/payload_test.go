package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
)

func nowZero() time.Time { return time.Date(2026, 4, 30, 14, 22, 11, 482_000_000, time.UTC) }

func sampleEvent(t string) db.Event {
	id := int64(42)
	uid := "01HZNQ7VFPK1XGD8R5MABCD4EX"
	return db.Event{
		ID:          81237,
		Type:        t,
		Actor:       "claude-4.7-wesm-laptop",
		ProjectID:   3,
		ProjectName: "github.com/wesm/kata",
		IssueID:     &id,
		IssueUID:    &uid,
		Payload:     `{"comment_id":104}`,
		CreatedAt:   nowZero(),
	}
}

func okIssue(_ context.Context, _ int64) (IssueSnapshot, error) {
	return IssueSnapshot{
		UID:     "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ShortID: "abc4", Title: "fix login crash on Safari", Status: "open",
		Labels: []string{"bug", "safari"}, Owner: "claude-4.7-wesm-laptop", Author: "claude-4.7-wesm-laptop",
	}, nil
}

func okComment(_ context.Context, _ int64) (CommentSnapshot, error) {
	return CommentSnapshot{ID: 104, Body: "looks like a render bug"}, nil
}

func okProject(_ context.Context, _ int64) (ProjectSnapshot, error) {
	return ProjectSnapshot{Name: "kata"}, nil
}

func okAlias(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) {
	return AliasSnapshot{Identity: "github.com/wesm/kata", Kind: "git", RootPath: "/Users/wesm/code/kata"}, true, nil
}

// buildOpts captures the resolver/logger inputs to buildStdinJSON; tests
// override only the fields they care about via withXxx options.
type buildOpts struct {
	rp  projectResolver
	ri  issueResolver
	rc  commentResolver
	ra  aliasResolver
	log logfn
}

type buildOption func(*buildOpts)

func withProject(r projectResolver) buildOption { return func(o *buildOpts) { o.rp = r } }
func withIssue(r issueResolver) buildOption     { return func(o *buildOpts) { o.ri = r } }
func withComment(r commentResolver) buildOption { return func(o *buildOpts) { o.rc = r } }
func withAlias(r aliasResolver) buildOption     { return func(o *buildOpts) { o.ra = r } }
func withLog(l logfn) buildOption               { return func(o *buildOpts) { o.log = l } }

// runBuild invokes buildStdinJSON with the okXxx defaults, applies overrides,
// and unmarshals the result. Returns raw bytes, decoded envelope, and the
// truncated flag.
func runBuild(t *testing.T, evt db.Event, opts ...buildOption) ([]byte, map[string]any, bool) {
	t.Helper()
	o := buildOpts{rp: okProject, ri: okIssue, rc: okComment, ra: okAlias, log: nopLog()}
	for _, opt := range opts {
		opt(&o)
	}
	out, truncated := buildStdinJSON(context.Background(), evt, o.rp, o.ri, o.rc, o.ra, o.log)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out, got, truncated
}

func TestBuildStdin_HappyPath(t *testing.T) {
	_, got, truncated := runBuild(t, sampleEvent("issue.commented"))
	if truncated {
		t.Fatal("happy path should not truncate")
	}
	if got["kata_hook_version"].(float64) != 1 {
		t.Fatalf("kata_hook_version: %v", got["kata_hook_version"])
	}
	if got["type"] != "issue.commented" {
		t.Fatalf("type: %v", got["type"])
	}
	proj := got["project"].(map[string]any)
	if proj["name"] != "kata" {
		t.Fatalf("project.name: %v", proj["name"])
	}
	issue := got["issue"].(map[string]any)
	if issue["title"] != "fix login crash on Safari" {
		t.Fatalf("issue.title: %v", issue["title"])
	}
	if issue["uid"] != "01HZNQ7VFPK1XGD8R5MABCD4EX" {
		t.Fatalf("issue.uid: %v — UID must accompany short_id so hook consumers can key on stable identity",
			issue["uid"])
	}
	if issue["short_id"] != "abc4" {
		t.Fatalf("issue.short_id: %v", issue["short_id"])
	}
	pld := got["payload"].(map[string]any)
	if pld["comment_body"] != "looks like a render bug" {
		t.Fatalf("comment_body: %v", pld["comment_body"])
	}
	alias := got["alias"].(map[string]any)
	if alias["alias_identity"] != "github.com/wesm/kata" {
		t.Fatalf("alias_identity: %v", alias["alias_identity"])
	}
}

func TestBuildStdin_CommentBodyFromPortablePayload(t *testing.T) {
	evt := sampleEvent("issue.commented")
	evt.Payload = `{"comment_uid":"01HZNQ7VFPK1XGD8R5MABCD4EY","body":"direct body"}`
	_, got, _ := runBuild(t, evt, withComment(func(context.Context, int64) (CommentSnapshot, error) {
		return CommentSnapshot{}, errors.New("comment resolver should not be called")
	}))

	pld := got["payload"].(map[string]any)
	if pld["comment_body"] != "direct body" {
		t.Fatalf("comment_body: %v", pld["comment_body"])
	}
}

func TestBuildStdin_AliasMissing_OmitsBlock(t *testing.T) {
	noAlias := func(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) { return AliasSnapshot{}, false, nil }
	_, got, _ := runBuild(t, sampleEvent("issue.created"), withAlias(noAlias))
	if _, ok := got["alias"]; ok {
		t.Fatal("alias block should be omitted")
	}
}

func TestBuildStdin_AliasError_OmitsBlockLogs(t *testing.T) {
	errAlias := func(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) {
		return AliasSnapshot{}, false, errors.New("boom")
	}
	logged := []string{}
	logger := func(format string, _ ...any) { logged = append(logged, format) }
	_, got, _ := runBuild(t, sampleEvent("issue.created"), withAlias(errAlias), withLog(logger))
	if _, ok := got["alias"]; ok {
		t.Fatal("alias block should be omitted on resolver error")
	}
	if len(logged) == 0 {
		t.Fatal("alias resolver error should be logged")
	}
}

func TestBuildStdin_IssueResolverError_OmitsIssueBlock(t *testing.T) {
	bad := func(_ context.Context, _ int64) (IssueSnapshot, error) { return IssueSnapshot{}, errors.New("db down") }
	_, got, _ := runBuild(t, sampleEvent("issue.created"), withIssue(bad))
	if _, ok := got["issue"]; ok {
		t.Fatal("issue block should be omitted when IssueResolver errors")
	}
}

func TestBuildStdin_ProjectResolverError_KeepsIDAndName(t *testing.T) {
	bad := func(_ context.Context, _ int64) (ProjectSnapshot, error) {
		return ProjectSnapshot{}, errors.New("db down")
	}
	_, got, _ := runBuild(t, sampleEvent("issue.created"), withProject(bad))
	proj := got["project"].(map[string]any)
	if proj["id"].(float64) != 3 {
		t.Fatalf("project.id should still be present: %v", proj["id"])
	}
	if proj["name"] != "github.com/wesm/kata" {
		t.Fatalf("project.name should still be present: %v", proj["name"])
	}
}

func TestBuildStdin_NonCommentEvent_SkipsCommentResolver(t *testing.T) {
	called := false
	cr := func(_ context.Context, _ int64) (CommentSnapshot, error) {
		called = true
		return CommentSnapshot{}, nil
	}
	_, _, _ = runBuild(t, sampleEvent("issue.created"), withComment(cr))
	if called {
		t.Fatal("CommentResolver must not be invoked for non-issue.commented events")
	}
}

func TestBuildStdin_TitleTruncated(t *testing.T) {
	bigTitle := strings.Repeat("A", 2*1024)
	bigIssue := func(_ context.Context, _ int64) (IssueSnapshot, error) {
		return IssueSnapshot{ShortID: "abc4", Title: bigTitle, Status: "open"}, nil
	}
	_, got, _ := runBuild(t, sampleEvent("issue.created"), withIssue(bigIssue))
	issue := got["issue"].(map[string]any)
	if issue["_truncated"] != true {
		t.Fatal("issue._truncated should be true for 2KB title")
	}
	if int(issue["_full_size"].(float64)) != len(bigTitle) {
		t.Fatalf("_full_size = %v, want %d", issue["_full_size"], len(bigTitle))
	}
}

func TestBuildStdin_TitleTruncation_RuneBoundary(t *testing.T) {
	// 4-byte rune (😀 = U+1F600) repeated to overflow the 1KB title cap
	// at a non-aligned offset. 257 runes = 1028 bytes, cap = 1024 means
	// the cut lands inside the 257th rune.
	bigTitle := strings.Repeat("😀", 257)
	bigIssue := func(_ context.Context, _ int64) (IssueSnapshot, error) {
		return IssueSnapshot{ShortID: "abc4", Title: bigTitle, Status: "open"}, nil
	}
	_, got, _ := runBuild(t, sampleEvent("issue.created"), withIssue(bigIssue))
	issue := got["issue"].(map[string]any)
	title := issue["title"].(string)
	if !utf8.ValidString(title) {
		t.Fatalf("truncated title is not valid UTF-8: %q", title)
	}
	// Truncated string must be ≤ limit and end on a rune boundary, so it
	// should contain a whole number of 4-byte runes (≤ 256).
	if len(title) > 1024 {
		t.Fatalf("truncated length %d exceeds 1024", len(title))
	}
}

func TestBuildStdin_TopLevelTruncation_DropsOptionalFields(t *testing.T) {
	evt := sampleEvent("issue.commented")
	bigBody := strings.Repeat("X", 16*1024)
	bigIssue := func(_ context.Context, _ int64) (IssueSnapshot, error) {
		return IssueSnapshot{ShortID: "abc4", Title: strings.Repeat("T", 600), Status: "open"}, nil
	}
	bigComment := func(_ context.Context, _ int64) (CommentSnapshot, error) {
		return CommentSnapshot{ID: 1, Body: bigBody}, nil
	}
	bigPayload := strings.Repeat("Y", 250*1024)
	evt.Payload = `{"comment_id":104,"big":"` + bigPayload + `"}`
	out, got, truncated := runBuild(t, evt, withIssue(bigIssue), withComment(bigComment))
	if !truncated {
		t.Fatal("oversize payload should set top-level payload_truncated:true")
	}
	if len(out) > 256*1024 {
		t.Fatalf("output size %d exceeds 256KB cap", len(out))
	}
	if got["payload_truncated"] != true {
		t.Fatal("payload_truncated must be true")
	}
}

func nopLog() func(string, ...any) { return func(string, ...any) {} }
