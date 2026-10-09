package commentref

import (
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func graphRecord(n int, issue string, parent string, kind string) Record {
	return Record{UID: fmt.Sprintf("01AAAAAAAAAAAAAAAAAA%06d", n), Author: "worker", Body: "Finding", CreatedAt: time.Unix(int64(n), 0), ReplyToUID: parent, ReplyKind: kind, IssueUID: issue, IssueShortID: issue, ProjectUID: "project-uid", ProjectName: "example-project"}
}

func checkIncomingEvidenceCap(t *testing.T, confirmations int) {
	t.Helper()
	root := graphRecord(1, "ab12", "", "")
	rows := []Record{root}
	for n := range confirmations {
		rows = append(rows, graphRecord(n+2, "cd34", root.UID, "confirm"))
	}
	for _, kind := range []string{"reply", "refute", "supersede", "supersede"} {
		rows = append(rows, graphRecord(len(rows)+1, "cd34", root.UID, kind))
	}
	projected := Project(rows, nil)[0]
	counts := map[string]int{}
	for _, link := range projected.Backlinks {
		counts[link.Kind]++
	}
	require.Equal(t, min(confirmations, 50), counts["confirm"])
	require.Equal(t, 1, counts["reply"])
	require.Equal(t, 1, counts["refute"])
	require.Equal(t, 2, counts["supersede"])
	require.Equal(t, confirmations > 50, projected.BacklinksTruncated)
	require.Equal(t, root.Body, projected.Body)
	if confirmations > 50 {
		require.Equal(t, rows[confirmations-49].UID, projected.Backlinks[0].UID,
			"keep the newest evidence for each kind in chronological order")
	}
}

func TestIncomingEvidenceCapKeepsConflictingKinds(t *testing.T) {
	checkIncomingEvidenceCap(t, 51)
}

func FuzzIncomingEvidenceCap(f *testing.F) {
	f.Add(uint8(51))
	f.Add(uint8(0))
	f.Fuzz(func(t *testing.T, confirmations uint8) {
		checkIncomingEvidenceCap(t, int(confirmations))
	})
}

func TestCommentGraphProjection(t *testing.T) {
	root := graphRecord(1, "ab12", "", "")
	reply := graphRecord(2, "cd34", root.UID, "refute")
	edited := time.Unix(3, 0)
	root.EditedAt = &edited
	got := Project([]Record{reply, root}, nil)
	require.Equal(t, "c:000002", got[0].Handle)
	require.Equal(t, "ab12:000001", got[0].Reply.Handle)
	require.Equal(t, "worker", got[0].Reply.Author)
	require.True(t, got[0].Reply.TargetEdited)
	require.Equal(t, "cd34:000002", got[1].Backlinks[0].Handle)
	require.Equal(t, "refute", got[1].Backlinks[0].Kind)
}

func TestCommentGraphUnavailableTargets(t *testing.T) {
	for _, status := range []string{"pending", "removed", "moved", "hidden"} {
		t.Run(status, func(t *testing.T) {
			reply := graphRecord(2, "ab12", graphRecord(1, "ab12", "", "").UID, "reply")
			got := Project([]Record{reply}, map[string]TargetState{reply.ReplyToUID: {Status: status}})
			if status == "hidden" {
				require.Nil(t, got[0].Reply)
				require.Empty(t, got[0].ReplyToUID)
				return
			}
			require.Equal(t, status, got[0].Reply.Status)
		})
	}
}

func TestCommentThreadTraversesKindsAndCycles(t *testing.T) {
	root := graphRecord(1, "ab12", "", "")
	middle := graphRecord(2, "cd34", root.UID, "reply")
	leaf := graphRecord(3, "ef56", middle.UID, "refute")
	root.ReplyToUID, root.ReplyKind = leaf.UID, "confirm"
	got, err := Select([]Record{leaf, middle, root}, "ab12", Options{Thread: "c:000001", Kind: "refute"})
	require.NoError(t, err)
	require.Len(t, got.Comments, 2)
	require.Equal(t, root.UID, got.Comments[0].UID)
	require.Equal(t, leaf.UID, got.Comments[1].UID)
	got, err = Select([]Record{leaf, root}, "ab12", Options{Thread: "c:000001"})
	require.NoError(t, err)
	require.Len(t, got.Comments, 1, "a hidden intermediate must not expand")
}

func TestCommentThreadCapIncludesRoot(t *testing.T) {
	root := graphRecord(1, "ab12", "", "")
	rows := []Record{root}
	for n := 2; n <= 51; n++ {
		rows = append(rows, graphRecord(n, "cd34", root.UID, "reply"))
	}
	got, err := Select(rows, "ab12", Options{Thread: "c:000001"})
	require.NoError(t, err)
	require.True(t, got.Truncated)
	require.Len(t, got.Comments, 50)
	require.Equal(t, root.UID, got.Comments[0].UID)
}

func TestCommentThreadRetainsLateRootWithinCap(t *testing.T) {
	root := graphRecord(1, "ab12", "", "")
	root.CreatedAt = time.Unix(100, 0)
	rows := []Record{root}
	for n := 2; n <= 51; n++ {
		rows = append(rows, graphRecord(n, "cd34", root.UID, "reply"))
	}
	got, err := Select(rows, "ab12", Options{Thread: "c:000001"})
	require.NoError(t, err)
	require.Len(t, got.Comments, 50)
	require.True(t, got.Truncated)
	require.Equal(t, root.UID, got.Comments[49].UID)
}

func TestCommentInboundAndSince(t *testing.T) {
	root := graphRecord(1, "ab12", "", "")
	root.Teammate = "reviewer"
	other := graphRecord(2, "ab12", "", "")
	other.Author = "another-worker"
	reply := graphRecord(3, "cd34", root.UID, "confirm")
	ignored := graphRecord(4, "cd34", other.UID, "reply")
	reply.CreatedAt, ignored.CreatedAt = root.CreatedAt, root.CreatedAt
	rows := []Record{ignored, reply, other, root}
	got, err := Select(rows, "ab12", Options{Inbound: "worker/reviewer", Kind: "confirm"})
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	require.Equal(t, reply.UID, got.Comments[0].UID)
	got, err = Select(rows, "ab12", Options{Thread: "c:000001", Since: "c:000001"})
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	require.Equal(t, reply.UID, got.Comments[0].UID)
}

func TestResolveCommentDoesNotCountInvisibleCollisions(t *testing.T) {
	row := graphRecord(1, "ab12", "", "")
	got, err := Resolve([]Record{row}, "ab12", "example-project#AB12:000001", "example-project")
	require.NoError(t, err)
	require.Equal(t, row.UID, got.UID)
	_, err = Resolve([]Record{row}, "cd34", "c:000001", "example-project")
	require.Error(t, err)
	_, err = Resolve([]Record{row}, "ab12", "other-project#ab12:000001", "example-project")
	require.Error(t, err)
}

// Selection must be stable across federation arrival order, including ties.
func FuzzCommentGraphOrder(f *testing.F) {
	f.Add(int64(0), true)
	f.Add(int64(42), false)
	f.Fuzz(func(t *testing.T, stamp int64, reverse bool) {
		a := graphRecord(1, "ab12", "", "")
		b := graphRecord(2, "ab12", "", "")
		a.CreatedAt = time.Unix(stamp%1000000, 0)
		b.CreatedAt = a.CreatedAt
		rows := []Record{a, b}
		if reverse {
			rows = []Record{b, a}
		}
		got, err := Select(rows, "ab12", Options{Since: "c:000001"})
		require.NoError(t, err)
		require.Len(t, got.Comments, 1)
		require.Equal(t, b.UID, got.Comments[0].UID)
	})
}

func TestQualifiedThreadFromIssueWithoutComments(t *testing.T) {
	records := []Record{{UID: "01AAAAAAAAAAAAAAAAAAAAAAAA", IssueUID: "target-issue", IssueShortID: "aaaa", ProjectName: "example-project"}}
	got, err := Select(Project(records, nil), "empty-issue", Options{Thread: "example-project#aaaa:aaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 1 {
		t.Fatal("qualified root missing")
	}
}

func TestMovedReplyUsesCurrentProjectHandle(t *testing.T) {
	target := Record{UID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Handle: "c:aaaaaa", IssueUID: "target-issue", IssueShortID: "aaaa", ProjectUID: "other-project", ProjectName: "other-project"}
	source := Record{UID: "01BBBBBBBBBBBBBBBBBBBBBBBB", ReplyToUID: target.UID, ReplyKind: "reply", IssueUID: "source-issue", ProjectUID: "source-project", ProjectName: "source-project"}
	out := Project([]Record{source}, map[string]TargetState{target.UID: {Status: "moved", Target: &target}})
	if out[0].Reply.Handle != "other-project#aaaa:aaaaaa" {
		t.Fatalf("moved handle = %s", out[0].Reply.Handle)
	}
}

func FuzzThreadHandlesResolveFromShownIssue(f *testing.F) {
	f.Add(uint8(3))
	f.Fuzz(func(t *testing.T, count uint8) {
		records := []Record{}
		n := int(count)%48 + 2
		for i := range n {
			r := Record{UID: fmt.Sprintf("%026d", i+1), CreatedAt: time.Unix(int64(i), 0), IssueUID: "root-issue", IssueShortID: "abcd", ProjectName: "example-project"}
			if i%2 == 1 {
				r.IssueUID = "other-issue"
				r.IssueShortID = "bcde"
			}
			if i > 0 {
				r.ReplyToUID = records[i-1].UID
				r.ReplyKind = "reply"
			}
			records = append(records, r)
		}
		records = Project(records, nil)
		selected, err := Select(records, "root-issue", Options{Thread: records[0].UID})
		require.NoError(t, err)
		for _, r := range selected.Comments {
			resolved, err := Resolve(records, "root-issue", r.Handle, "example-project")
			require.NoError(t, err, "selected thread handle must resolve from the shown issue")
			require.Equal(t, r.UID, resolved.UID)
			links := append([]Link{}, r.Backlinks...)
			if r.Reply != nil {
				links = append(links, *r.Reply)
			}
			for _, link := range links {
				endpoint, err := Resolve(records, "root-issue", link.Handle, "example-project")
				require.NoError(t, err, "reply and backlink handles must resolve from shown issue")
				require.Equal(t, link.UID, endpoint.UID)
			}
		}
	})
}

// The unmerged reply feature accepts only the owner-approved canonical kinds.
func TestCanonicalReplyKindRejectsAnswerAlias(t *testing.T) {
	require.True(t, ValidKind("reply"))
	require.False(t, ValidKind("answer"))
}

func FuzzCanonicalReplyKinds(f *testing.F) {
	for _, kind := range []string{"reply", "answer", "confirm", "refute", "supersede", "", "REPLY"} {
		f.Add(kind)
	}
	f.Fuzz(func(t *testing.T, kind string) {
		expected := kind == "reply" || kind == "confirm" || kind == "refute" || kind == "supersede"
		require.Equal(t, expected, ValidKind(kind))
	})
}

func TestIncomingEvidencePreservesBodyTimeAndEditedTarget(t *testing.T) {
	assertIncomingEvidence(t, "Reproduced two retries", "confirm")
}

func FuzzIncomingEvidenceProjection(f *testing.F) {
	f.Add("Reproduced two retries", uint8(1))
	f.Add("Competing replacement", uint8(3))
	f.Fuzz(func(t *testing.T, body string, kindIndex uint8) {
		if !utf8.ValidString(body) {
			return
		}
		kinds := []string{"reply", "confirm", "refute", "supersede"}
		assertIncomingEvidence(t, body, kinds[int(kindIndex)%len(kinds)])
	})
}

func assertIncomingEvidence(t *testing.T, body, kind string) {
	t.Helper()
	root := graphRecord(1, "ab12", "", "")
	reply := graphRecord(2, "cd34", root.UID, kind)
	reply.Body = body
	reply.Teammate = "worker-a"
	edited := reply.CreatedAt.Add(time.Minute)
	root.EditedAt = &edited
	projected := Project([]Record{root, reply}, nil)
	raw, err := json.Marshal(projected[0].Backlinks[0])
	require.NoError(t, err)
	var evidence map[string]any
	require.NoError(t, json.Unmarshal(raw, &evidence))
	projectedBody, _ := evidence["body"].(string)
	require.Equal(t, body, projectedBody)
	require.Equal(t, reply.CreatedAt.Format(time.RFC3339), evidence["created_at"])
	require.Equal(t, true, evidence["target_edited"])
	require.Equal(t, "worker-a", evidence["teammate"])
	// An unauthorized reply excluded before projection contributes no evidence.
	require.Empty(t, Project([]Record{root}, nil)[0].Backlinks)
}
