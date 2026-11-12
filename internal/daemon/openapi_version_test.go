package daemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

func TestCheckAPIVersionRecord(t *testing.T) {
	const a = `{"info":{"version":""},"paths":{"/a":{}}}`
	const b = `{"info":{"version":""},"paths":{"/b":{}}}`
	const hashA = "a796a0b24a661cae44ecfaf054e4e53ee32d80f219424fb40c319a1d036aba5f"
	const hashB = "95abbaa7a1794b458dae61670a105f01339b2978a65310dc6575f987536650c9"
	for _, tc := range []struct {
		name, doc, version string
		markers            []string
		contains           []string
	}{
		{"collision", a, "0.45.0", []string{"0.45.0." + hashA + ".sha256", "0.45.0." + hashB + ".sha256"}, []string{"0.46.0", "0.45.0." + hashA + ".sha256", "0.45.0." + hashB + ".sha256", "feature minimum checks introduced by this change"}},
		{"stale", b, "0.45.0", []string{"0.45.0." + hashA + ".sha256"}, []string{"not yet on origin/main", "0.45.0." + hashB + ".sha256", "0.46.0"}},
		{"next recorded", b, "0.46.0", []string{"0.45.0." + hashA + ".sha256", "0.46.0." + hashB + ".sha256"}, nil},
		{"older matching", a, "0.45.0", []string{"0.45.0." + hashA + ".sha256", "0.46.0." + hashB + ".sha256"}, []string{"0.47.0"}},
		{"older missing", a, "0.45.0", []string{"0.46.0." + hashB + ".sha256"}, []string{"0.47.0"}},
		{"missing", a, "0.45.0", nil, []string{"0.45.0." + hashA + ".sha256"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.markers {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
			}
			err := checkAPIVersionRecord(dir, tc.version, []byte(tc.doc))
			if len(tc.contains) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, s := range tc.contains {
				require.Contains(t, err.Error(), s)
			}
		})
	}
}

func TestAPISchemaVersionMatchesRecordedContract(t *testing.T) {
	doc, err := contractDocument(OpenAPIDocument())
	require.NoError(t, err)
	require.NoError(t, checkAPIVersionRecord("testdata/api-contracts", APISchemaVersion, doc))
}

func TestAPIVersionRecordMustBeEmpty(t *testing.T) {
	dir := t.TempDir()
	doc := []byte(`{"info":{"version":""},"paths":{"/a":{}}}`)
	name := "0.45.0.a796a0b24a661cae44ecfaf054e4e53ee32d80f219424fb40c319a1d036aba5f.sha256"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("content"), 0600))
	require.ErrorContains(t, checkAPIVersionRecord(dir, "0.45.0", doc), name)
}

func checkAPIVersionRecord(dir, version string, doc []byte) error {
	digest := sha256.Sum256(doc)
	marker := fmt.Sprintf("%s.%x.sha256", version, digest)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count, match := 0, false
	var records []string
	next := "v" + version
	for _, entry := range entries {
		name := entry.Name()
		parts := strings.Split(name, ".")
		if len(parts) != 5 || parts[4] != "sha256" || len(parts[3]) != 64 {
			continue
		}
		recorded := "v" + strings.Join(parts[:3], ".")
		if !semver.IsValid(recorded) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return fmt.Errorf("API contract record must be an empty regular file: %s", filepath.Join(dir, name))
		}
		if semver.Compare(recorded, next) > 0 {
			next = recorded
		}
		if strings.Join(parts[:3], ".") == version {
			count++
			records = append(records, filepath.Join(dir, name))
			match = match || name == marker
		}
	}
	if semver.Compare("v"+version, next) == 0 && count == 1 && match {
		return nil
	}
	if semver.Compare("v"+version, next) == 0 && count == 0 {
		return fmt.Errorf("API contract has no record; add empty marker %s", filepath.Join(dir, marker))
	}
	minor, err := strconv.Atoi(strings.TrimPrefix(semver.MajorMinor(next), semver.Major(next)+"."))
	if err != nil {
		return err
	}
	suggested := fmt.Sprintf("%s.%d.0", strings.TrimPrefix(semver.Major(next), "v"), minor+1)
	if semver.Compare("v"+version, next) == 0 && count == 1 {
		return fmt.Errorf("API contract version %s has a stale record (%s); if %s is not yet on origin/main, delete your old marker and add %s; otherwise take %s in APISchemaVersion and api/openapi.yaml, and find and move feature minimum checks introduced by this change; move this branch's marker and recompute it for the combined document", version, strings.Join(records, ", "), version, filepath.Join(dir, marker), suggested)
	}
	return fmt.Errorf("API contract version %s has older, conflicting, or stale records (%s); take %s in APISchemaVersion and api/openapi.yaml, and find and move feature minimum checks introduced by this change; move this branch's marker and recompute it for the combined document", version, strings.Join(records, ", "), suggested)
}

// contractDocument removes documentation fields from Huma objects while leaving
// schema property names, extension values, defaults, and validation intact.
func contractDocument(doc *huma.OpenAPI) ([]byte, error) {
	doc.Info.Version = ""
	seen := map[uintptr]bool{}
	var strip func(reflect.Value)
	strip = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				if _, ok := reflect.TypeAssert[*huma.Schema](v); ok {
					strip(v.Elem())
				}
			}
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			strip(v.Elem())
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				strip(v.Index(i))
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				strip(iter.Value())
			}
		case reflect.Struct:
			if v.Type().PkgPath() != "github.com/danielgtaylor/huma/v2" {
				return
			}
			for i := 0; i < v.NumField(); i++ {
				f, name := v.Field(i), v.Type().Field(i).Name
				if !f.CanSet() {
					continue
				}
				switch name {
				case "Description", "Summary", "Title", "Example", "Examples", "Tags", "ExternalDocs", "PatternDescription":
					f.SetZero()
				default:
					strip(f)
				}
			}
		}
	}
	strip(reflect.ValueOf(doc))
	for _, schema := range doc.Components.Schemas.Map() {
		strip(reflect.ValueOf(schema))
	}
	return doc.MarshalJSON()
}

func TestAPIVersionBaseRecords(t *testing.T) {
	ref := os.Getenv("API_VERSION_BASE_REF")
	explicit := ref != ""
	if !explicit {
		ref = "origin/main"
	}
	head := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "--quiet", "HEAD")
	if err := head.Run(); err != nil {
		var exitErr *exec.ExitError
		if !explicit && t.Context().Err() == nil && (errors.Is(err, exec.ErrNotFound) || errors.As(err, &exitErr) && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 128)) {
			t.Skip("git metadata unavailable")
		}
		t.Fatalf("resolve HEAD: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}") //nolint:gosec // The base ref is supplied by the developer or CI.
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !explicit && t.Context().Err() == nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			t.Skip("origin/main unavailable")
		}
		t.Fatalf("resolve API version base %q: %v", ref, err)
	}
	records, err := baseAPIVersionRecords(t, ref)
	require.NoError(t, err)
	require.NoError(t, checkBaseAPIVersionRecords("testdata/api-contracts", records))
}

func baseAPIVersionRecords(t *testing.T, ref string) ([]string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "ls-tree", "-r", "--name-only", ref, "internal/daemon/testdata/api-contracts/") //nolint:gosec // The base ref is supplied by the developer or CI.
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read API version records at %q: %w", ref, err)
	}
	var names []string
	for name := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			names = append(names, filepath.Base(name))
		}
	}
	return names, nil
}

func checkBaseAPIVersionRecords(dir string, records []string) error {
	var missing []string
	for _, name := range records {
		if info, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
			missing = append(missing, name)
		} else if err != nil {
			return err
		} else if !info.Mode().IsRegular() || info.Size() != 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("base API contract records must be preserved: %s", strings.Join(missing, ", "))
	}
	return nil
}

func TestAPIContractDocument(t *testing.T) {
	doc := OpenAPIDocument()
	initial, err := contractDocument(doc)
	require.NoError(t, err)
	for _, op := range documentOperations(doc) {
		op.Description = "changed wording"
		op.Summary = "changed summary"
		op.Tags = []string{"changed"}
	}
	doc.Info.Description = "new wording"
	schema := doc.Components.Schemas.Map()["HealthResponseBody"]
	schema.Description = "new wording"
	wording, err := contractDocument(doc)
	require.NoError(t, err)
	require.JSONEq(t, string(initial), string(wording))
	schema.Properties["description"] = &huma.Schema{Type: "string"}
	field, err := contractDocument(doc)
	require.NoError(t, err)
	require.NotEqual(t, string(initial), string(field))
	delete(schema.Properties, "description")
	doc.Paths["/new-route"] = &huma.PathItem{Get: &huma.Operation{OperationID: "new-route", Responses: map[string]*huma.Response{"200": {Description: "ok"}}}}
	route, err := contractDocument(doc)
	require.NoError(t, err)
	require.NotEqual(t, string(initial), string(route))
}

func TestAPIBaseRecordLoading(t *testing.T) {
	records := []string{
		"0.44.0.dcc4a8689ae0d97113d2349d9afcbbf7fe95caa08a516bcc2e376d021d998166.sha256",
		"0.45.0.95abbaa7a1794b458dae61670a105f01339b2978a65310dc6575f987536650c9.sha256",
	}
	dir := t.TempDir()
	require.ErrorContains(t, checkBaseAPIVersionRecords(dir, records), records[0])
	for _, name := range records {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
	}
	require.NoError(t, checkBaseAPIVersionRecords(dir, records))
	require.NoError(t, os.WriteFile(filepath.Join(dir, records[0]), []byte("changed"), 0600))
	require.ErrorContains(t, checkBaseAPIVersionRecords(dir, records), records[0])
}
