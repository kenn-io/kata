package daemon

import (
	"cmp"
	"crypto/sha256"
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
	const suffixA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const suffixB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	marker := func(version, hash, suffix string) string { return version + "." + hash + "." + suffix + ".sha256" }
	for _, tc := range []struct {
		name, doc, version string
		markers            []string
		contains           []string
		summary            string
	}{
		{"collision", a, "0.45.0", []string{marker("0.45.0", hashA, suffixA), marker("0.45.0", hashB, suffixB)}, []string{"0.46.0", marker("0.45.0", hashA, suffixA), marker("0.45.0", hashB, suffixB), "feature minimum"}, ""},
		{"historical collision", b, "0.46.0", []string{marker("0.45.0", hashA, suffixA), marker("0.45.0", hashB, suffixB), marker("0.46.0", hashB, suffixA)}, []string{"0.45.0", "0.47.0"}, ""},
		{"matching summaries", a, "0.45.0", []string{marker("0.45.0", hashA, suffixA), marker("0.45.0", hashA, suffixB)}, nil, ""},
		{"stale", b, "0.45.0", []string{marker("0.45.0", hashA, suffixA)}, []string{"not yet on origin/main", hashB, "0.46.0"}, ""},
		{"next recorded", b, "0.46.0", []string{marker("0.45.0", hashA, suffixA), marker("0.46.0", hashB, suffixB)}, nil, ""},
		{"older matching", a, "0.45.0", []string{marker("0.45.0", hashA, suffixA), marker("0.46.0", hashB, suffixB)}, []string{"0.47.0"}, ""},
		{"older missing", a, "0.45.0", []string{marker("0.46.0", hashB, suffixB)}, []string{"0.47.0"}, ""},
		{"missing", a, "0.45.0", nil, []string{hashA, "sha256sum"}, ""},
		{"summary", a, "0.45.0", []string{marker("0.45.0", hashA, suffixA)}, nil, "Add route"},
		{"blank summary", a, "0.45.0", []string{marker("0.45.0", hashA, suffixA)}, []string{"one-line summary"}, " \n"},
		{"multiline summary", a, "0.45.0", []string{marker("0.45.0", hashA, suffixA)}, []string{"one-line summary"}, "Add route\nAdd field\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.markers {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(cmp.Or(tc.summary, "Add route\n")), 0600))
			}
			if tc.name == "matching summaries" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tc.markers[1]), []byte("Another complete summary\n"), 0600))
			}
			err := checkAPIVersionRecord(dir, tc.version, []byte(tc.doc))
			if len(tc.contains) == 0 {
				require.NoError(t, err)
				if tc.name == "matching summaries" {
					first, err := os.ReadFile(filepath.Join(dir, tc.markers[0]))
					require.NoError(t, err)
					second, err := os.ReadFile(filepath.Join(dir, tc.markers[1]))
					require.NoError(t, err)
					require.Equal(t, "Add route\n", string(first))
					require.Equal(t, "Another complete summary\n", string(second))
				}
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

func checkAPIVersionRecord(dir, version string, doc []byte) error {
	digest := fmt.Sprintf("%x", sha256.Sum256(doc))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	records := map[string]map[string][]string{}
	next := "v" + version
	for _, entry := range entries {
		name := entry.Name()
		parts := strings.Split(name, ".")
		if len(parts) != 6 || parts[5] != "sha256" || len(parts[3]) != 64 || len(parts[4]) != 64 {
			continue
		}
		recorded := "v" + strings.Join(parts[:3], ".")
		if !semver.IsValid(recorded) {
			continue
		}
		if err := checkAPIVersionSummary(filepath.Join(dir, name)); err != nil {
			return err
		}
		if records[recorded] == nil {
			records[recorded] = map[string][]string{}
		}
		records[recorded][parts[3]] = append(records[recorded][parts[3]], filepath.Join(dir, name))
		if semver.Compare(recorded, next) > 0 {
			next = recorded
		}
	}
	pattern := filepath.Join(dir, fmt.Sprintf("%s.%s.<summary-sha256>.sha256", version, digest))
	create := fmt.Sprintf("add %s with a one-line summary; hash the chosen summary including its newline with printf '%%s\\n' 'your summary' | sha256sum", pattern)
	current := records["v"+version]
	var collisions []string
	for _, hashes := range records {
		if len(hashes) > 1 {
			for _, paths := range hashes {
				collisions = append(collisions, paths...)
			}
		}
	}
	if len(collisions) == 0 && semver.Compare("v"+version, next) == 0 {
		if len(current[digest]) > 0 {
			return nil
		}
		if len(current) == 0 {
			return fmt.Errorf("API contract has no record; %s", create)
		}
	}
	minor, err := strconv.Atoi(strings.TrimPrefix(semver.MajorMinor(next), semver.Major(next)+"."))
	if err != nil {
		return err
	}
	suggested := fmt.Sprintf("%s.%d.0", strings.TrimPrefix(semver.Major(next), "v"), minor+1)
	hint := ""
	if len(collisions) == 0 && semver.Compare("v"+version, next) == 0 && len(current) == 1 {
		hint = fmt.Sprintf("if %s is not yet on origin/main, replace your marker's contract digest with %s while retaining its summary suffix; otherwise ", version, digest)
	}
	return fmt.Errorf("API contract version %s has older, conflicting, or stale records (%s); %stake %s in APISchemaVersion and api/openapi.yaml, and move feature minimum checks introduced by this change (search requireDaemonAPIVersion and requireAPIVersion in cmd/kata/api_compat.go, internal/mcp, internal/tui, and internal/recurrence); regenerate the combined contract and rename this branch's marker with its new version and contract digest %s, retaining the summary suffix", version, strings.Join(collisions, ", "), hint, suggested, digest)
}

func checkAPIVersionSummary(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	summary := strings.TrimSuffix(string(content), "\n")
	if strings.TrimSpace(summary) == "" || strings.ContainsAny(summary, "\r\n") {
		return fmt.Errorf("API contract record must have a one-line summary: %s", path)
	}
	return nil
}

// contractDocument removes documentation fields from Huma objects while leaving
// schema property names, extension values, defaults, and validation intact.
func contractDocument(doc *huma.OpenAPI) ([]byte, error) {
	doc.Info = &huma.Info{}
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
	if ref == "" {
		t.Skip("set by make api-breaking")
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
		if err := checkAPIVersionSummary(filepath.Join(dir, name)); os.IsNotExist(err) {
			missing = append(missing, name)
		} else if err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("base API contract records must be preserved: %s; if your branch is behind the base, rebase and rerun the test", strings.Join(missing, ", "))
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
	doc.Info.TermsOfService = "https://example.com/terms"
	doc.Info.Contact = &huma.Contact{Name: "Team"}
	doc.Info.License = &huma.License{Name: "License"}
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

	delete(doc.Paths, "/new-route")
	op := &huma.Operation{OperationID: "contract-test", Responses: map[string]*huma.Response{"200": {Description: "ok"}}}
	doc.Paths["/contract-test"] = &huma.PathItem{Post: op}
	baseline, err := contractDocument(doc)
	require.NoError(t, err)
	op.Parameters = []*huma.Param{{Name: "filter", In: "query", Schema: &huma.Schema{Type: "string"}}}
	parameter, err := contractDocument(doc)
	require.NoError(t, err)
	require.NotEqual(t, string(baseline), string(parameter))
	op.Parameters = nil
	op.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"input": {Type: "string"}}}}}}
	request, err := contractDocument(doc)
	require.NoError(t, err)
	require.NotEqual(t, string(baseline), string(request))
	op.RequestBody = nil
	op.Responses["200"].Content = map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"output": {Type: "string"}}}}}
	response, err := contractDocument(doc)
	require.NoError(t, err)
	require.NotEqual(t, string(baseline), string(response))
}

func TestAPIBaseRecordLoading(t *testing.T) {
	records := []string{
		"0.44.0.dcc4a8689ae0d97113d2349d9afcbbf7fe95caa08a516bcc2e376d021d998166.sha256",
		"0.45.0.95abbaa7a1794b458dae61670a105f01339b2978a65310dc6575f987536650c9.sha256",
	}
	dir := t.TempDir()
	err := checkBaseAPIVersionRecords(dir, records)
	require.ErrorContains(t, err, records[0])
	require.ErrorContains(t, err, "rebase")
	for _, name := range records {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("Add route\n"), 0600))
	}
	require.NoError(t, checkBaseAPIVersionRecords(dir, records))
	require.NoError(t, os.WriteFile(filepath.Join(dir, records[0]), []byte("Corrected summary\n"), 0600))
	require.NoError(t, checkBaseAPIVersionRecords(dir, records))
	require.NoError(t, os.WriteFile(filepath.Join(dir, records[0]), nil, 0600))
	require.ErrorContains(t, checkBaseAPIVersionRecords(dir, records), records[0])
}
