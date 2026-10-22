// Package docbanktest implements Docbank's folder, upload, and content routes for tests.
package docbanktest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Fake keeps uploaded bytes in memory and records API requests.
type Fake struct {
	Server       *httptest.Server
	Fault        string
	UploadStatus int
	mu           sync.Mutex
	requests     []string
	dirs         map[string]int64
	versions     map[string]string
	content      map[string][]byte
}

// New starts an isolated Docbank fake and registers its cleanup.
func New(t testing.TB) *Fake {
	f := &Fake{dirs: map[string]int64{"/": 1}, versions: map[string]string{}, content: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// Requests returns the method and path of each observed request.
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}
func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("X-Api-Key") != "test-key" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status, ok := strings.CutPrefix(f.Fault, "status:"); ok {
		n, _ := strconv.Atoi(status)
		http.Error(w, "upstream", n)
		return
	}
	send := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.MarshalWrite(w, v) }
	switch {
	case r.URL.Path == "/api/v1/path" && r.Method == "GET":
		id, ok := f.dirs[r.URL.Query().Get("path")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		send(map[string]any{"id": id, "kind": "dir"})
	case r.URL.Path == "/api/v1/path/mkdir" && r.Method == "POST":
		var in struct {
			Path string `json:"path"`
		}
		if json.UnmarshalRead(r.Body, &in) != nil {
			http.Error(w, "path", 400)
			return
		}
		if _, ok := f.dirs[in.Path]; ok {
			http.Error(w, "exists", http.StatusConflict)
			return
		}
		if _, ok := f.dirs[path.Dir(in.Path)]; !ok {
			http.NotFound(w, r)
			return
		}
		id := int64(len(f.dirs) + 1)
		f.dirs[in.Path] = id
		send(map[string]any{"id": id, "kind": "dir"})
	case r.URL.Path == "/api/v1/uploads" && r.Method == "POST":
		if f.UploadStatus != 0 {
			http.Error(w, "upstream", f.UploadStatus)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, (256<<20)+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // Bounded by MaxBytesReader above.
			http.Error(w, "multipart", 400)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		files := r.MultipartForm.File["file"]
		if len(files) != 1 {
			http.Error(w, "file count", 400)
			return
		}
		file, err := files[0].Open()
		if err != nil {
			http.Error(w, "file", 400)
			return
		}
		defer func() { _ = file.Close() }()
		b, _ := io.ReadAll(file)
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		if hash != r.Header.Get("X-Docbank-Blob-Hash") || strconv.Itoa(len(b)) != r.Header.Get("X-Docbank-Blob-Size") {
			http.Error(w, "identity", http.StatusUnprocessableEntity)
			return
		}
		parent, _ := strconv.ParseInt(r.URL.Query().Get("parent_id"), 10, 64)
		found := false
		for _, id := range f.dirs {
			if id == parent {
				found = true
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Query().Get("name")
		if name != files[0].Filename {
			http.Error(w, "name", 400)
			return
		}
		key := fmt.Sprintf("%d/%s/%s", parent, name, hash)
		version, exists := f.versions[key]
		if !exists {
			version = fmt.Sprintf("6f1c2e8a-2222-4222-8222-%012d", len(f.versions)+1)
			f.versions[key] = version
			f.content[version] = b
		}
		send(map[string]any{"node": map[string]string{"current_version_id": version}})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/versions/") && strings.HasSuffix(r.URL.Path, "/content"):
		version := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/versions/"), "/content")
		b, ok := f.content[version]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeContent(w, b)
	default:
		http.NotFound(w, r)
	}
}

func writeContent(w http.ResponseWriter, b []byte) {
	sum := sha256.Sum256(b)
	w.Header().Set("X-Docbank-Blob-Size", strconv.Itoa(len(b)))
	w.Header().Set("X-Docbank-Blob-Hash", hex.EncodeToString(sum[:]))
	w.Header().Set("Set-Cookie", "must-not-leak=1")
	_, _ = w.Write(b)
}
