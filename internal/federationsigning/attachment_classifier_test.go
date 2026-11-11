package federationsigning

import (
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: only the exact binary upload and immutable content routes get the
// larger budgets. Malformed IDs and route aliases retain ordinary controls.
func TestAttachmentTransferClassifier(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", uploadPath, true},
		{"GET", "/api/v1/projects/9223372036854775807/federation/docbank/versions/6f1c2e8a-2222-4222-8222-222222222222/content", true},
		{"GET", uploadPath, false}, {"HEAD", uploadPath, false},
		{"POST", strings.Replace(uploadPath, "/1/", "/0/", 1), false},
		{"POST", strings.Replace(uploadPath, "/1/", "/-1/", 1), false},
		{"POST", strings.Replace(uploadPath, "/1/", "/9223372036854775808/", 1), false},
		{"POST", strings.Replace(uploadPath, "abc4", "", 1), false},
		{"POST", strings.Replace(uploadPath, "abc4", ".", 1), false},
		{"POST", strings.Replace(uploadPath, "abc4", "..", 1), false},
		{"POST", strings.Replace(uploadPath, "abc4", "%2f", 1), false},
		{"POST", strings.Replace(uploadPath, "abc4", "a\\b", 1), false},
		{"POST", uploadPath + "/extra", false}, {"POST", uploadPath + "/", false},
		{"POST", "/mount" + uploadPath, false}, {"POST", uploadPath + "?x=1", false},
		{"POST", strings.Replace(uploadPath, "uploads", "uploads-more", 1), false},
		{"GET", "/api/v1/projects/1/federation/docbank/versions/not-a-uuid/content", false},
	} {
		require.Equal(t, tc.want, IsAttachmentTransfer(tc.method, tc.path), "%s %s", tc.method, tc.path)
	}
}

func TestAttachmentTransferClassifierValidProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Integers[int64](1, math.MaxInt64))
		ref := "ref-" + hex.EncodeToString([]byte(hegel.Draw(ht, hegel.Text())))
		version := hegel.Draw(ht, hegel.UUIDs())
		for method, path := range map[string]string{
			http.MethodPost: fmt.Sprintf("/api/v1/projects/%d/federation/docbank/issues/%s/uploads", id, ref),
			http.MethodGet:  fmt.Sprintf("/api/v1/projects/%d/federation/docbank/versions/%s/content", id, version),
		} {
			if !IsAttachmentTransfer(method, path) {
				ht.Fatalf("rejected %s %s", method, path)
			}
		}
	})
}

func TestAttachmentTransferClassifierArbitraryProperty(t *testing.T) {
	// Independent grammar oracle, including overflow and canonical integer IDs.
	upload := regexp.MustCompile(`^/api/v1/projects/([1-9][0-9]*)/federation/docbank/issues/([^/%\\?#\x00-\x20\x7f]+)/uploads$`)
	content := regexp.MustCompile(`^/api/v1/projects/([1-9][0-9]*)/federation/docbank/versions/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/content$`)
	hegel.Test(t, func(ht *hegel.T) {
		method := hegel.Draw(ht, hegel.SampledFrom([]string{"POST", "GET", "HEAD", "PUT", "DELETE", ""}))
		part := hegel.Draw(ht, hegel.Text())
		path := hegel.Draw(ht, hegel.SampledFrom([]string{part, strings.Replace(uploadPath, "abc4", part, 1), uploadPath + part, part + uploadPath}))
		var matches []string
		switch method {
		case "POST":
			matches = upload.FindStringSubmatch(path)
		case "GET":
			matches = content.FindStringSubmatch(path)
		}
		want := len(matches) != 0
		if want {
			_, err := strconv.ParseInt(matches[1], 10, 64)
			want = err == nil && (method != "POST" || matches[2] != "." && matches[2] != "..")
		}
		if got := IsAttachmentTransfer(method, path); got != want {
			ht.Fatalf("%s %q: got %v want %v", method, path, got, want)
		}
	})
}
