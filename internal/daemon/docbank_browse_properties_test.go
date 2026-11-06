package daemon

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"go.kenn.io/kata/internal/api"
	"hegel.dev/go/hegel"
)

// Segment semantics are an independent oracle, without path.Clean or the
// production validator. Arbitrary bytes include malformed UTF-8 and NUL.
func TestDocbankBrowseCanonicalPathProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := string(hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255))))
		want := len(value) > 0 && len(value) <= 4096 && value[0] == '/' && utf8.ValidString(value)
		if want && value != "/" {
			for segment := range strings.SplitSeq(value[1:], "/") {
				if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\\\x00") {
					want = false
				}
			}
		}
		if got := canonicalDocbankBrowsePath(value); got != want {
			ht.Fatalf("canonical path %q: got %t want %t", value, got, want)
		}
	}, hegel.WithTestCases(2048))
}

func TestDocbankBrowseNativeNamePathProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := "n" + strings.NewReplacer("/", "%2F", "\\", "%5C", "\x00", "%00").Replace(hegel.Draw(ht, hegel.Text()))
		value := "/folder/" + name
		want := len(value) <= 4096
		node := api.DocbankBrowseNode{ID: 1, Name: name, Kind: "file", Size: hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64)), Path: value}
		if got := validDocbankBrowseNode(node); got != want {
			ht.Fatalf("native single name path rejected/accepted incorrectly: %q size=%d got=%t want=%t", value, node.Size, got, want)
		}
	}, hegel.WithTestCases(2048))
}

func TestDocbankBrowsePageFullWidthOffsetsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		limit := hegel.Draw(ht, hegel.Integers(1, 200))
		offset := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		total := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		directory := api.DocbankBrowseNode{ID: 1, Name: "folder", Kind: "dir", Path: "/folder"}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			directory.Name, directory.Path = "/", "/"
		}
		// Walk the model's index interval, at most200 steps. The last possible
		// index is total-1, so incrementing it cannot exceed MaxInt.
		count := 0
		for index := offset; index < total && count < limit; index++ {
			count++
		}
		page := api.DocbankChildrenBody{Directory: directory, Items: []api.DocbankBrowseNode{}, Total: total, Limit: limit, Offset: offset}
		for i := range count {
			name := fmt.Sprintf("file-%d", i)
			prefix := directory.Path
			if prefix == "/" {
				prefix = ""
			}
			page.Items = append(page.Items, api.DocbankBrowseNode{ID: int64(i + 2), Name: name, Kind: "file", Path: prefix + "/" + name, Size: int64(i)})
		}
		if !validDocbankBrowsePage(page, directory.Path, limit, offset) {
			ht.Fatalf("valid page rejected: total=%d limit=%d offset=%d count=%d root=%q", total, limit, offset, count, directory.Path)
		}
	}, hegel.WithTestCases(2048))
}

func TestDocbankBrowsePageRejectsMalformedProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		limit := hegel.Draw(ht, hegel.Integers(1, 200))
		page := api.DocbankChildrenBody{Directory: api.DocbankBrowseNode{ID: 1, Name: "folder", Kind: "dir", Path: "/folder"}, Items: []api.DocbankBrowseNode{{ID: 2, Name: "file", Kind: "file", Path: "/folder/file"}}, Total: 1, Limit: limit}
		which := hegel.Draw(ht, hegel.Integers(0, 7))
		switch which {
		case 0:
			page.Items[0].Size = -1 - hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))
		case 1:
			page.Items[0].ID = -hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))
		case 2:
			page.Items[0].Name = "../" + hegel.Draw(ht, hegel.Text())
		case 3:
			page.Offset = hegel.Draw(ht, hegel.Integers(1, math.MaxInt))
		case 4:
			page.Total = 0
		case 5:
			page.Items = nil
		case 6:
			page.Directory.Kind = "file"
		case 7:
			page.Items[0].Path = "/outside/" + hegel.Draw(ht, hegel.Text())
		}
		if validDocbankBrowsePage(page, "/folder", limit, 0) {
			ht.Fatalf("malformed page accepted: mutation=%d page=%+v", which, page)
		}
	}, hegel.WithTestCases(2048))
}

type browseClosedBody struct {
	io.Reader
	closed bool
}

func (b *browseClosedBody) Close() error { b.closed = true; return nil }

func TestDocbankBrowseDecodeBoundAndPathScale(t *testing.T) {
	for _, body := range []string{`{"origin":"x"}` + strings.Repeat(" ", 1<<20), `{"items":[`, `{} {}`} {
		reader := &browseClosedBody{Reader: strings.NewReader(body)}
		var out api.DocbankChildrenBody
		if err := decodeDocbankBrowseResponse(&http.Response{Body: reader}, &out); err == nil || !reader.closed {
			t.Fatalf("oversized/malformed response accepted or leaked: bytes=%d closed=%t err=%v", len(body), reader.closed, err)
		}
	}
	for _, size := range []int{4096, 4097, 100000} {
		value := "/" + strings.Repeat("n", size-1)
		if got := canonicalDocbankBrowsePath(value); got != (size == 4096) {
			t.Fatalf("path size %d: accepted=%t", size, got)
		}
	}
}

func TestDocbankBrowseDecodeRequiresPageFields(t *testing.T) {
	for _, field := range []string{"directory", "items", "total", "limit", "offset"} {
		t.Run(field, func(t *testing.T) {
			page := map[string]any{"directory": map[string]any{"id": 1, "name": "folder", "kind": "dir", "path": "/folder", "size": 0}, "items": []any{}, "total": 0, "limit": 100, "offset": 0}
			delete(page, field)
			raw, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			body := &browseClosedBody{Reader: strings.NewReader(string(raw))}
			var result api.DocbankChildrenBody
			if err := decodeDocbankBrowseResponse(&http.Response{Body: body}, &result); err == nil || !body.closed {
				t.Fatalf("missing %s accepted or leaked: closed=%t error=%v", field, body.closed, err)
			}
		})
	}
}

func TestDocbankBrowseRootCannotBeItsOwnChild(t *testing.T) {
	for _, name := range []string{"", "/"} {
		page := api.DocbankChildrenBody{Directory: api.DocbankBrowseNode{ID: 1, Name: "/", Kind: "dir", Path: "/"}, Items: []api.DocbankBrowseNode{{ID: 2, Name: name, Kind: "dir", Path: "/"}}, Total: 1, Limit: 1}
		if validDocbankBrowsePage(page, "/", 1, 0) {
			t.Fatalf("root entry accepted as a child under name %q", name)
		}
	}
}
