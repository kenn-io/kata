package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

func TestRun(t *testing.T) {
	Run(t, newFixture())
}

func TestRunAcceptsFieldBaselineEqualToSamples(t *testing.T) {
	fixture := newFixture()
	fixture.fieldBaselineMatchesSamples = true
	Run(t, fixture)
}

type memoryFixture struct {
	description                 connector.Description
	root                        connector.Root
	comments                    []connector.Comment
	providerComments            []connector.Comment
	descriptors                 []connector.FieldDescriptor
	mutations                   []Mutation
	exchange                    func(context.Context, []byte) ([]byte, error)
	providerFields              map[string]connector.FieldValue
	discardProviderComment      bool
	discardProviderFields       bool
	advanceObservation          bool
	futureObservation           bool
	omitPrefrontierComment      bool
	errorMode                   string
	errorMethod                 string
	errorMessage                string
	forbiddenMutationKey        string
	forbiddenFieldValueKey      string
	wrongMutationType           bool
	trailingMutation            bool
	dateOnlyFields              bool
	unstableDescription         bool
	unstableResetIdentity       bool
	describeCalls               int
	resetCalls                  int
	rootLocator                 string
	invocation                  connector.Invocation
	requireInvocation           bool
	publications                map[string]connector.Comment
	noEditedComment             bool
	noDeletedComment            bool
	missingCommentTime          bool
	missingCommentRevision      bool
	constantCommentRevision     bool
	reverseComments             bool
	nonIdempotentComplete       bool
	unchangedCompletionRev      bool
	rejectFieldKind             string
	noActionableField           bool
	paddedFieldID               bool
	paddedSchemaRevision        bool
	fabricatedRootRead          bool
	fabricatedComments          bool
	duplicateAfterCrash         bool
	crashAfterMutation          bool
	publicationCrashed          bool
	staleCompletionTime         bool
	stalePublicationTime        bool
	nonIdempotentPublish        bool
	brokenReadOnlyField         bool
	fieldsWithoutCapability     bool
	fieldBaselineMatchesSamples bool
}

func newFixture() *memoryFixture {
	fixture := &memoryFixture{invocation: connector.Invocation{Instance: "example-instance", Settings: json.RawMessage(`{}`)}}
	_ = fixture.Reset(context.Background())
	return fixture
}

func (f *memoryFixture) RootLocator() string { return f.rootLocator }

func (f *memoryFixture) Invocation() connector.Invocation {
	return connector.Invocation{
		Instance: f.invocation.Instance,
		Settings: append(json.RawMessage(nil), f.invocation.Settings...),
	}
}

func (f *memoryFixture) Exchange(ctx context.Context, request []byte) ([]byte, error) {
	if f.exchange != nil {
		return f.exchange(ctx, request)
	}
	if f.requireInvocation {
		var decoded connector.Request
		if err := json.Unmarshal(request, &decoded); err != nil {
			return nil, err
		}
		if decoded.Instance != f.invocation.Instance || !bytes.Equal(decoded.Settings, f.invocation.Settings) {
			return nil, fmt.Errorf("connector invocation = %q %s, want %q %s", decoded.Instance, decoded.Settings, f.invocation.Instance, f.invocation.Settings)
		}
	}
	var response bytes.Buffer
	err := connector.ServeOne(ctx, bytes.NewReader(request), &response, f)
	if err == nil && f.crashAfterMutation {
		var decoded connector.Request
		if decodeErr := json.Unmarshal(request, &decoded); decodeErr != nil {
			return nil, decodeErr
		}
		if decoded.Method == "publish_comment" {
			f.crashAfterMutation = false
			f.publicationCrashed = true
			return nil, errors.New("connector exited after publication")
		}
	}
	return response.Bytes(), err
}

func TestRunPropagatesFixtureInvocation(t *testing.T) {
	fixture := newFixture()
	fixture.invocation = connector.Invocation{
		Instance: "configured-instance",
		Settings: json.RawMessage(`{"workspace":"example-workspace"}`),
	}
	fixture.requireInvocation = true
	Run(t, fixture)
}

func (f *memoryFixture) InjectFault(_ context.Context, fault Fault) error {
	if fault != FaultPublishCommentCrashAfterMutation {
		return fmt.Errorf("unsupported fault %q", fault)
	}
	f.crashAfterMutation = true
	return nil
}

func (f *memoryFixture) MutateComment(_ context.Context, commentID string) error {
	for index := range f.providerComments {
		if f.providerComments[index].ID != commentID {
			continue
		}
		updatedAt := f.providerComments[index].UpdatedAt.Add(time.Minute)
		f.providerComments[index].Body = "Externally edited comment"
		f.providerComments[index].UpdatedAt = updatedAt
		f.providerComments[index].Revision = "comment-revision-after-edit"
		for observedIndex := range f.comments {
			if f.comments[observedIndex].ID != commentID {
				continue
			}
			f.comments[observedIndex].Body = "Externally edited comment"
			f.comments[observedIndex].UpdatedAt = updatedAt
			if !f.constantCommentRevision {
				f.comments[observedIndex].Revision = "comment-revision-after-edit"
			}
			return nil
		}
		return fmt.Errorf("connector observation lacks comment %q", commentID)
	}
	return fmt.Errorf("provider state lacks comment %q", commentID)
}

func TestRunRejectsBrokenWireExchanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		subtest string
	}{
		{name: "wrong response ID", mode: "wrong-id", subtest: "language-neutral_protocol_transcripts"},
		{name: "response ID fixed to first request", mode: "fixed-id", subtest: "language-neutral_protocol_transcripts"},
		{name: "unsupported protocol accepted", mode: "accept-version", subtest: "language-neutral_protocol_transcripts"},
		{name: "malformed request accepted", mode: "accept-malformed", subtest: "language-neutral_protocol_transcripts"},
		{name: "trailing response JSON", mode: "trailing-response", subtest: "language-neutral_protocol_transcripts"},
		{name: "result and error response", mode: "result-and-error", subtest: "language-neutral_protocol_transcripts"},
		{name: "committed transcript response ID", mode: "wrong-transcript-id", subtest: "language-neutral_protocol_transcripts"},
		{name: "invalid UTF-8 success response", mode: "invalid-utf8-success", subtest: "language-neutral_protocol_transcripts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrokenWireExchangeProbe$") // #nosec G204,G702 -- the current test executable is fixed by the Go test runner.
			command.Env = append(os.Environ(), "KATA_CONFORMANCE_WIRE_PROBE="+test.mode)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("Run accepted broken exchange %q:\n%s", test.mode, output)
			}
			if !strings.Contains(string(output), test.subtest) {
				t.Fatalf("Run failed outside wire conformance for %q:\n%s", test.mode, output)
			}
		})
	}
}

func TestBrokenWireExchangeProbe(t *testing.T) {
	mode := os.Getenv("KATA_CONFORMANCE_WIRE_PROBE")
	if mode == "" {
		t.Skip("subprocess probe")
	}
	fixture := newFixture()
	fixture.exchange = func(ctx context.Context, raw []byte) ([]byte, error) {
		var request connector.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			if mode == "accept-malformed" {
				return []byte(`{"protocol":"kata.connector.v1","id":"accepted","result":{}}` + "\n"), nil
			}
			return nil, err
		}
		if mode == "accept-version" && request.Protocol != connector.ProtocolVersion {
			request.Protocol = connector.ProtocolVersion
			raw, _ = json.Marshal(request)
		}
		response, err := fixture.ExchangeWithoutOverride(ctx, raw)
		if mode == "wrong-id" && err == nil {
			var decoded connector.Response
			_ = json.Unmarshal(response, &decoded)
			decoded.ID = "wrong-response-id"
			response, _ = json.Marshal(decoded)
			response = append(response, '\n')
		}
		if mode == "fixed-id" && err == nil {
			var decoded connector.Response
			_ = json.Unmarshal(response, &decoded)
			decoded.ID = "request-conformance-1"
			response, _ = json.Marshal(decoded)
			response = append(response, '\n')
		}
		if mode == "trailing-response" && err == nil {
			response = append(response, []byte("{}\n")...)
		}
		if mode == "result-and-error" && err == nil {
			var decoded connector.Response
			_ = json.Unmarshal(response, &decoded)
			decoded.Error = &connector.Error{Code: "invalid_response", Message: "response is ambiguous"}
			response, _ = json.Marshal(decoded)
			response = append(response, '\n')
		}
		if mode == "wrong-transcript-id" && err == nil && strings.HasPrefix(request.ID, "transcript-") {
			var decoded connector.Response
			_ = json.Unmarshal(response, &decoded)
			decoded.ID = "wrong-transcript-response-id"
			response, _ = json.Marshal(decoded)
			response = append(response, '\n')
		}
		if mode == "invalid-utf8-success" && err == nil {
			response = bytes.Replace(
				response,
				[]byte(`"display_name":"Example Connector"`),
				[]byte("\"display_name\":\"Example \xff Connector\""),
				1,
			)
		}
		return response, err
	}
	Run(t, fixture)
}

func TestProtocolV1TranscriptFixturesAreCommitted(t *testing.T) {
	entries, err := os.ReadDir("testdata/protocol-v1")
	if err != nil {
		t.Fatalf("read protocol-v1 transcript fixtures: %v", err)
	}
	jsonFiles := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			jsonFiles++
		}
	}
	if jsonFiles < 3 {
		t.Fatalf("protocol-v1 transcript fixture count = %d, want at least 3", jsonFiles)
	}
}

func TestProtocolV1TranscriptFixturesAreDeclarative(t *testing.T) {
	entries, err := os.ReadDir("testdata/protocol-v1")
	if err != nil {
		t.Fatalf("read protocol-v1 transcript fixtures: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		encoded, err := os.ReadFile("testdata/protocol-v1/" + entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var document struct {
			Schema string `json:"schema"`
			Steps  []struct {
				Assert []json.RawMessage `json:"assert"`
				Expect json.RawMessage   `json:"expect"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}
		if document.Schema != "kata.connector.conformance/v1" {
			t.Errorf("%s schema = %q, want declarative conformance schema", entry.Name(), document.Schema)
		}
		for index, step := range document.Steps {
			if len(step.Expect) != 0 {
				t.Errorf("%s step %d uses opaque expectation %s", entry.Name(), index, step.Expect)
			}
			if len(step.Assert) == 0 {
				t.Errorf("%s step %d has no executable assertions", entry.Name(), index)
			}
		}
	}
}

func TestProtocolV1TranscriptAssertionsRejectMissingOperands(t *testing.T) {
	err := validateTranscriptAssertion(transcriptAssertion{
		Op:     "equal",
		Actual: transcriptValue{Path: "/steps/example/response/result"},
	})
	if err == nil {
		t.Fatal("equal assertion without an expected operand was accepted")
	}
}

func TestProtocolV1TranscriptPointersUseRFC6901Syntax(t *testing.T) {
	if err := validateTranscriptValue(transcriptValue{Path: "/invalid~2escape"}); err == nil {
		t.Fatal("JSON Pointer with an illegal escape was accepted")
	}
	if _, _, err := resolveJSONPointer([]any{"first", "second"}, "/01"); err == nil {
		t.Fatal("JSON Pointer with a non-canonical array index was accepted")
	}
	if err := removeJSONPointer([]any{"first", "second"}, "/01"); err == nil {
		t.Fatal("excluded JSON Pointer with a non-canonical array index was accepted")
	}
}

func TestProtocolV1TranscriptValidUTF8AllowsReplacementCharacter(t *testing.T) {
	runtime := &transcriptRuntime{root: map[string]any{
		"legitimate": "�",
		"invalid":    string([]byte{0xff}),
	}}
	legitimate, err := runtime.matches(transcriptAssertion{Op: "valid_utf8", Actual: transcriptValue{Path: "/legitimate"}})
	if err != nil || !legitimate {
		t.Fatalf("valid replacement character rejected: matches=%v err=%v", legitimate, err)
	}
	invalid, err := runtime.matches(transcriptAssertion{Op: "valid_utf8", Actual: transcriptValue{Path: "/invalid"}})
	if err != nil {
		t.Fatalf("validate invalid UTF-8: %v", err)
	}
	if invalid {
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestProtocolV1TranscriptsRejectEveryRunBehavior(t *testing.T) {
	for _, mode := range []string{
		"unstable-description",
		"unstable-reset-identity",
		"no-edited-comment",
		"no-deleted-comment",
		"missing-comment-time",
		"missing-comment-revision",
		"constant-comment-revision",
		"reverse-comments",
		"non-idempotent-completion",
		"unchanged-completion-revision",
		"non-idempotent-publication",
		"broken-read-only-field",
		"reject-local-datetime",
		"reject-null",
		"no-actionable-field",
		"padded-field-id",
		"padded-schema-revision",
		"unreadable-read-only-field",
		"fields-without-capability",
	} {
		for _, runner := range []string{"run", "transcripts"} {
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrokenDeclarativeContractProbe$") // #nosec G204,G702 -- the current test executable is fixed by the Go test runner.
			command.Env = append(os.Environ(), "KATA_CONFORMANCE_DECLARATIVE_PROBE="+mode, "KATA_CONFORMANCE_DECLARATIVE_RUNNER="+runner)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("%s accepted broken connector mode %q:\n%s", runner, mode, output)
			}
		}
	}
}

func TestBrokenDeclarativeContractProbe(t *testing.T) {
	if os.Getenv("KATA_CONFORMANCE_DECLARATIVE_PROBE") == "" {
		t.Skip("subprocess probe")
	}
	fixture := newFixture()
	switch os.Getenv("KATA_CONFORMANCE_DECLARATIVE_PROBE") {
	case "unstable-description":
		fixture.unstableDescription = true
	case "unstable-reset-identity":
		fixture.unstableResetIdentity = true
	case "no-edited-comment":
		fixture.noEditedComment = true
	case "no-deleted-comment":
		fixture.noDeletedComment = true
	case "missing-comment-time":
		fixture.missingCommentTime = true
	case "missing-comment-revision":
		fixture.missingCommentRevision = true
	case "constant-comment-revision":
		fixture.constantCommentRevision = true
	case "reverse-comments":
		fixture.reverseComments = true
	case "non-idempotent-completion":
		fixture.nonIdempotentComplete = true
	case "unchanged-completion-revision":
		fixture.unchangedCompletionRev = true
	case "non-idempotent-publication":
		fixture.nonIdempotentPublish = true
	case "broken-read-only-field":
		fixture.brokenReadOnlyField = true
	case "reject-local-datetime":
		fixture.rejectFieldKind = "local_datetime"
	case "reject-null":
		fixture.rejectFieldKind = "null"
	case "no-actionable-field":
		fixture.noActionableField = true
	case "padded-field-id":
		fixture.paddedFieldID = true
	case "padded-schema-revision":
		fixture.paddedSchemaRevision = true
	case "unreadable-read-only-field":
		fixture.brokenReadOnlyField = true
	case "fields-without-capability":
		fixture.fieldsWithoutCapability = true
	default:
		t.Fatal("unknown declarative contract probe")
	}
	if os.Getenv("KATA_CONFORMANCE_DECLARATIVE_RUNNER") == "transcripts" {
		invocation := fixture.Invocation()
		settings, err := decodeTranscriptJSON(invocation.Settings)
		if err != nil {
			t.Fatalf("decode fixture settings: %v", err)
		}
		runProtocolV1Transcripts(t.Context(), t, fixture, invocation.Instance, settings)
		return
	}
	Run(t, fixture)
}

func TestRunReadsProviderStateAndValidatesFrontiers(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		subtest string
	}{
		{name: "cached publication", mode: "cached-publication", subtest: "language-neutral protocol transcripts"},
		{name: "cached field write", mode: "cached-field", subtest: "language-neutral protocol transcripts"},
		{name: "fabricated root read", mode: "fabricated-root", subtest: "language-neutral protocol transcripts"},
		{name: "fabricated comments", mode: "fabricated-comments", subtest: "language-neutral protocol transcripts"},
		{name: "duplicate publication after crash", mode: "duplicate-publication-after-crash", subtest: "language-neutral protocol transcripts"},
		{name: "stale completion timestamp", mode: "stale-completion-time", subtest: "language-neutral protocol transcripts"},
		{name: "stale publication timestamp", mode: "stale-publication-time", subtest: "language-neutral protocol transcripts"},
		{name: "future observation", mode: "future-observation", subtest: "language-neutral protocol transcripts"},
		{name: "no prefrontier comment", mode: "no-prefrontier", subtest: "language-neutral protocol transcripts"},
		{name: "unsafe multiline diagnostic", mode: "unsafe-error", subtest: "language-neutral protocol transcripts"},
		{name: "nested forbidden mutation key", mode: "forbidden-mutation", subtest: "language-neutral protocol transcripts"},
		{name: "camel issue ID", mode: "forbidden-issue-id", subtest: "language-neutral protocol transcripts"},
		{name: "camel issue UID", mode: "forbidden-issue-uid", subtest: "language-neutral protocol transcripts"},
		{name: "camel issue short ID", mode: "forbidden-issue-short-id", subtest: "language-neutral protocol transcripts"},
		{name: "camel child root", mode: "forbidden-child-root", subtest: "language-neutral protocol transcripts"},
		{name: "camel work attention", mode: "forbidden-work-attention", subtest: "language-neutral protocol transcripts"},
		{name: "kebab issue ID", mode: "forbidden-kebab-issue-id", subtest: "language-neutral protocol transcripts"},
		{name: "dotted child root", mode: "forbidden-dotted-child-root", subtest: "language-neutral protocol transcripts"},
		{name: "camel Kata UID", mode: "forbidden-kata-uid", subtest: "language-neutral protocol transcripts"},
		{name: "kebab Kata ref", mode: "forbidden-kata-ref", subtest: "language-neutral protocol transcripts"},
		{name: "dotted Kata project ID", mode: "forbidden-kata-project-id", subtest: "language-neutral protocol transcripts"},
		{name: "camel Kata binding ID", mode: "forbidden-kata-binding-id", subtest: "language-neutral protocol transcripts"},
		{name: "camel Kata work branch", mode: "forbidden-kata-work-branch", subtest: "language-neutral protocol transcripts"},
		{name: "arbitrary Kata owner", mode: "forbidden-arbitrary-kata", subtest: "language-neutral protocol transcripts"},
		{name: "wrong mutation value type", mode: "wrong-mutation-type", subtest: "language-neutral protocol transcripts"},
		{name: "trailing mutation JSON", mode: "trailing-mutation", subtest: "language-neutral protocol transcripts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrokenProviderStateProbe$") // #nosec G204,G702 -- the current test executable is fixed by the Go test runner.
			command.Env = append(os.Environ(), "KATA_CONFORMANCE_STATE_PROBE="+test.mode)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("Run accepted broken fixture %q:\n%s", test.mode, output)
			}
			if !strings.Contains(string(output), strings.ReplaceAll(test.subtest, " ", "_")) {
				t.Fatalf("Run failed outside %q for %q:\n%s", test.subtest, test.mode, output)
			}
		})
	}
}

func TestBrokenProviderStateProbe(t *testing.T) {
	mode := os.Getenv("KATA_CONFORMANCE_STATE_PROBE")
	if mode == "" {
		t.Skip("subprocess probe")
	}
	fixture := newFixture()
	switch mode {
	case "cached-publication":
		fixture.discardProviderComment = true
	case "cached-field":
		fixture.discardProviderFields = true
	case "fabricated-root":
		fixture.fabricatedRootRead = true
	case "fabricated-comments":
		fixture.fabricatedComments = true
	case "duplicate-publication-after-crash":
		fixture.duplicateAfterCrash = true
	case "stale-completion-time":
		fixture.staleCompletionTime = true
	case "stale-publication-time":
		fixture.stalePublicationTime = true
	case "future-observation":
		fixture.futureObservation = true
	case "no-prefrontier":
		fixture.omitPrefrontierComment = true
	case "unsafe-error":
		fixture.errorMode = "unsafe"
	case "forbidden-mutation":
		fixture.forbiddenMutationKey = "issue_id"
	case "forbidden-issue-id":
		fixture.forbiddenMutationKey = "issueId"
	case "forbidden-issue-uid":
		fixture.forbiddenMutationKey = "issueUid"
	case "forbidden-issue-short-id":
		fixture.forbiddenMutationKey = "issueShortId"
	case "forbidden-child-root":
		fixture.forbiddenMutationKey = "childRoot"
	case "forbidden-work-attention":
		fixture.forbiddenMutationKey = "workAttention"
	case "forbidden-kebab-issue-id":
		fixture.forbiddenMutationKey = "issue-id"
	case "forbidden-dotted-child-root":
		fixture.forbiddenMutationKey = "child.root"
	case "forbidden-kata-uid":
		fixture.forbiddenFieldValueKey = "kataUid"
	case "forbidden-kata-ref":
		fixture.forbiddenFieldValueKey = "kata-ref"
	case "forbidden-kata-project-id":
		fixture.forbiddenFieldValueKey = "kata.project.id"
	case "forbidden-kata-binding-id":
		fixture.forbiddenFieldValueKey = "kataBindingId"
	case "forbidden-kata-work-branch":
		fixture.forbiddenFieldValueKey = "kataWorkBranch"
	case "forbidden-arbitrary-kata":
		fixture.forbiddenFieldValueKey = "kataOwnerId"
	case "wrong-mutation-type":
		fixture.wrongMutationType = true
	case "trailing-mutation":
		fixture.trailingMutation = true
	default:
		t.Fatalf("unknown state probe %q", mode)
	}
	Run(t, fixture)
}

func TestRunAllowsAdvancingObservation(t *testing.T) {
	fixture := newFixture()
	fixture.advanceObservation = true
	Run(t, fixture)
}

func TestRunAllowsHarmlessErrorCode(t *testing.T) {
	fixture := newFixture()
	fixture.errorMode = "configuration"
	Run(t, fixture)
}

func TestRunAllowsDateOnlyFieldsFixture(t *testing.T) {
	fixture := newFixture()
	fixture.dateOnlyFields = true
	Run(t, fixture)
}

func TestMutationAuditTreatsFieldIDsAsOpaque(t *testing.T) {
	raw := json.RawMessage(`{"root_key":"root-example","fields":{"issueId":{"kind":"date","value":"2026-08-26"},"katakana_start":{"kind":"null"},"kata_uid":{"kind":"null"}},"expected":{"issueId":{"kind":"null"},"katakana_start":{"kind":"null"},"kata_uid":{"kind":"null"}}}`)
	if err := auditMutationParams("write_fields", raw, "root-example"); err != nil {
		t.Fatalf("opaque external field IDs were treated as structural identity channels: %v", err)
	}
}

func TestMutationAuditRejectsForbiddenKataKeysInsideFieldValues(t *testing.T) {
	for _, key := range []string{
		"kata_uid", "kataUid", "kata-uid", "kata.uid",
		"kata_ref", "kataRef", "kata-ref", "kata.ref",
		"kata_project_id", "kataProjectId", "kata-project-id", "kata.project.id",
		"kata_binding_id", "kataBindingId", "kata-binding-id", "kata.binding.id",
		"kata_work_branch", "kataWorkBranch", "kata-work-branch", "kata.work.branch",
	} {
		raw, err := json.Marshal(map[string]any{
			"root_key": "root-example",
			"fields": map[string]any{
				"kata_uid": map[string]any{"kind": "date", "value": "2026-08-20", key: "neutral-forbidden"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := auditMutationParams("write_fields", raw, "root-example"); err == nil {
			t.Fatalf("forbidden key variant %q was accepted", key)
		}
	}
}

func TestMutationAuditRejectsWrongTypesAndArbitraryKataChannels(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"root_key":"root-example","body":{}}`),
		json.RawMessage(`{"root_key":"root-example","fields":"field-example"}`),
		json.RawMessage(`{"root_key":"root-example","fields":{"katakana_start":{"kind":"date","value":"2026-08-20","kataOwnerId":"neutral-forbidden"}}}`),
	} {
		method := "write_fields"
		if bytes.Contains(raw, []byte(`"body"`)) {
			method = "publish_comment"
		}
		if err := auditMutationParams(method, raw, "root-example"); err == nil {
			t.Fatalf("%s invalid params accepted: %s", method, raw)
		}
	}
}

func TestRunRejectsUnsafeDiagnosticErrors(t *testing.T) {
	for _, test := range []struct{ name, method, message string }{
		{name: "absolute Unix path", method: "resolve_root", message: "/example/runtime/adapter"},
		{name: "embedded absolute Unix path", method: "resolve_root", message: "open /opt/example-adapter/config.json: permission denied"},
		{name: "absolute Windows path", method: "read_root", message: `C:\example\runtime\adapter.exe`},
		{name: "embedded Windows path", method: "read_root", message: `open D:\example-adapter\config.json: permission denied`},
		{name: "embedded UNC path", method: "read_root", message: `open \\daemon.example\example-share\config.json: permission denied`},
		{name: "control character", method: "list_comments", message: "adapter failed\nwith details"},
		{name: "exec whitespace", method: "publish_comment", message: "exec adapter --mode check"},
		{name: "exec equals mixed case", method: "publish_comment", message: "ExEc = example-adapter --mode check"},
		{name: "exec colon mixed case", method: "publish_comment", message: "EXEC: example-adapter --mode check"},
		{name: "command equals", method: "publish_comment", message: "command = example-adapter --mode check"},
		{name: "stderr colon", method: "read_fields", message: "stderr: adapter unavailable"},
		{name: "stderr equals mixed case", method: "read_fields", message: "StdErR = adapter unavailable"},
		{name: "stdout colon", method: "read_fields", message: "stdout: adapter unavailable"},
		{name: "stdout equals spaced", method: "read_fields", message: "STDOUT  =  adapter unavailable"},
		{name: "exit status whitespace", method: "write_fields", message: "exit status 17"},
		{name: "exit status equals mixed case", method: "write_fields", message: "Exit Status = 17"},
		{name: "config path", method: "complete_root", message: "config path: example/runtime/settings"},
		{name: "raw config JSON", method: "complete_root", message: `config: {"token":"raw-value"}`},
		{name: "raw configuration JSON equals", method: "complete_root", message: `CONFIGURATION = ["raw-value"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUnsafeDiagnosticErrorProbe$") // #nosec G204,G702 -- fixed current test executable.
			command.Env = append(os.Environ(), "KATA_CONFORMANCE_ERROR_METHOD="+test.method, "KATA_CONFORMANCE_ERROR_MESSAGE="+test.message)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("Run accepted unsafe diagnostic for %s:\n%s", test.method, output)
			}
			if !strings.Contains(string(output), "language-neutral_protocol_transcripts") {
				t.Fatalf("Run failed outside safe-error conformance for %s:\n%s", test.method, output)
			}
		})
	}
}

func TestUnsafeDiagnosticErrorProbe(t *testing.T) {
	method := os.Getenv("KATA_CONFORMANCE_ERROR_METHOD")
	if method == "" {
		t.Skip("subprocess probe")
	}
	fixture := newFixture()
	fixture.errorMethod = method
	fixture.errorMessage = os.Getenv("KATA_CONFORMANCE_ERROR_MESSAGE")
	Run(t, fixture)
}

func (f *memoryFixture) ExchangeWithoutOverride(ctx context.Context, request []byte) ([]byte, error) {
	var response bytes.Buffer
	err := connector.ServeOne(ctx, bytes.NewReader(request), &response, f)
	return response.Bytes(), err
}

func (f *memoryFixture) Reset(context.Context) error {
	observed := time.Now().UTC()
	base := observed.Add(-10 * time.Minute)
	f.resetCalls++
	f.rootLocator = "fixture-root"
	f.description = connector.Description{
		ConnectorID: "example.connector", DisplayName: "Example Connector",
		Protocol: connector.ProtocolVersion,
		Capabilities: []connector.Capability{
			connector.CapabilityFields, connector.CapabilityPublishComment,
		},
		ConfigSchema:    []byte(`{"type":"object","additionalProperties":false}`),
		SelfActorID:     "actor-self",
		AccountIdentity: "account-example",
	}
	if f.fieldsWithoutCapability {
		f.description.Capabilities = []connector.Capability{connector.CapabilityPublishComment}
	}
	f.root = connector.Root{
		Key: "root-example", IdentityKey: "account-example", Title: "Example root", Body: "Example body",
		State: "open", Revision: "revision-1", UpdatedAt: base, ObservedAt: observed,
		Fields: map[string]connector.FieldValue{
			"katakana_start":  {Kind: "date", Value: "2026-08-20"},
			"field-read-only": {Kind: "date", Value: "2026-08-19"},
			"field-local":     {Kind: "local_datetime", Value: "2026-08-20T11:30:00", Timezone: "Europe/Paris"},
			"field-instant":   {Kind: "instant", Value: "2026-08-20T09:30:00Z"},
			"field-null":      {Kind: "null"},
		},
	}
	if f.unstableResetIdentity {
		suffix := "-" + strconv.Itoa(f.resetCalls)
		f.rootLocator += suffix
		f.description.ConnectorID += suffix
		f.description.AccountIdentity += suffix
		f.root.Key += suffix
		f.root.IdentityKey = f.description.AccountIdentity
	}
	if f.fieldBaselineMatchesSamples {
		f.root.Fields["katakana_start"] = connector.FieldValue{Kind: "date", Value: "2026-08-21"}
		f.root.Fields["field-local"] = connector.FieldValue{Kind: "local_datetime", Value: "2026-08-21T14:45:00", Timezone: "Asia/Tokyo"}
		f.root.Fields["field-instant"] = connector.FieldValue{Kind: "instant", Value: "2026-08-21T05:45:00Z"}
	}
	f.comments = []connector.Comment{
		{ID: "comment-before-frontier", Revision: "comment-revision-1", Body: "Existing comment", Author: connector.Actor{ID: "actor-history", DisplayName: "Historical Reviewer"}, CreatedAt: base.Add(-time.Minute), UpdatedAt: base.Add(-time.Minute)},
		{ID: "comment-active", Revision: "comment-revision-2", Body: "Active comment", Author: connector.Actor{ID: "actor-a", DisplayName: "Reviewer A"}, CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)},
		{ID: "comment-edited", Revision: "comment-revision-3", Body: "Corrected comment", Author: connector.Actor{ID: "actor-b", DisplayName: "Reviewer B"}, CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(3 * time.Minute)},
		{ID: "comment-deleted", Revision: "comment-revision-4", Author: connector.Actor{ID: "actor-c", DisplayName: "Reviewer C"}, CreatedAt: base.Add(4 * time.Minute), UpdatedAt: base.Add(5 * time.Minute), Deleted: true},
	}
	if f.noEditedComment {
		for index := range f.comments {
			f.comments[index].UpdatedAt = f.comments[index].CreatedAt
		}
	}
	if f.noDeletedComment {
		for index := range f.comments {
			f.comments[index].Deleted = false
		}
	}
	if f.missingCommentTime {
		f.comments[0].CreatedAt = time.Time{}
	}
	if f.missingCommentRevision {
		f.comments[0].Revision = ""
	}
	if f.constantCommentRevision {
		for index := range f.comments {
			f.comments[index].Revision = "comment-revision-constant"
		}
	}
	if f.omitPrefrontierComment {
		f.comments = f.comments[1:]
		for index := range f.comments {
			f.comments[index].CreatedAt = observed.Add(time.Duration(index+1) * time.Minute)
			f.comments[index].UpdatedAt = f.comments[index].CreatedAt
			if f.comments[index].ID == "comment-edited" {
				f.comments[index].UpdatedAt = f.comments[index].CreatedAt.Add(time.Minute)
			}
		}
	}
	if f.futureObservation {
		f.root.ObservedAt = time.Now().UTC().Add(time.Hour)
	}
	if f.dateOnlyFields {
		f.root.Fields = map[string]connector.FieldValue{
			"field-date-only": {Kind: "date", Value: "2026-08-20"},
		}
	}
	f.providerComments = append([]connector.Comment(nil), f.comments...)
	f.providerFields = make(map[string]connector.FieldValue, len(f.root.Fields))
	maps.Copy(f.providerFields, f.root.Fields)
	f.descriptors = []connector.FieldDescriptor{
		{ID: "katakana_start", DisplayName: "Date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
		{ID: "field-read-only", DisplayName: "Read only date", AcceptedKinds: []string{"date"}, Nullable: false, Writable: false, SchemaRevision: "schema-1"},
		{ID: "field-local", DisplayName: "Local", AcceptedKinds: []string{"local_datetime"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
		{ID: "field-instant", DisplayName: "Instant", AcceptedKinds: []string{"instant"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
		{ID: "field-null", DisplayName: "Nullable", AcceptedKinds: []string{"date", "local_datetime", "instant"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
	}
	if f.noActionableField {
		f.descriptors = []connector.FieldDescriptor{{
			ID: "field-inert", DisplayName: "Inert", AcceptedKinds: []string{}, Writable: true, SchemaRevision: "schema-1",
		}}
	}
	if f.dateOnlyFields {
		f.descriptors = []connector.FieldDescriptor{{
			ID: "field-date-only", DisplayName: "Date", AcceptedKinds: []string{"date"},
			Nullable: false, Writable: true, SchemaRevision: "schema-1",
		}}
	}
	if f.paddedFieldID {
		f.descriptors[0].ID = " " + f.descriptors[0].ID + " "
	}
	if f.paddedSchemaRevision {
		f.descriptors[0].SchemaRevision = " " + f.descriptors[0].SchemaRevision + " "
	}
	f.mutations = nil
	f.describeCalls = 0
	f.crashAfterMutation = false
	f.publicationCrashed = false
	f.publications = make(map[string]connector.Comment)
	return nil
}

func (f *memoryFixture) ExternalState(context.Context) (RootState, error) {
	fields := make(map[string]connector.FieldValue, len(f.providerFields))
	maps.Copy(fields, f.providerFields)
	return RootState{
		Root: f.root, Comments: append([]connector.Comment(nil), f.providerComments...),
		Fields: fields, Mutations: append([]Mutation(nil), f.mutations...),
	}, nil
}

func (f *memoryFixture) Describe(context.Context, connector.DescribeParams) (connector.Description, *connector.Error) {
	f.describeCalls++
	description := f.description
	if f.unstableDescription && f.describeCalls > 1 {
		description.DisplayName = "Changed connector"
	}
	return description, nil
}

func (f *memoryFixture) ResolveRoot(_ context.Context, params connector.ResolveRootParams) (connector.Root, *connector.Error) {
	if params.Locator != f.RootLocator() {
		return connector.Root{}, f.fixtureError("resolve_root")
	}
	return f.observedRoot(), nil
}

func (f *memoryFixture) ReadRoot(_ context.Context, params connector.ReadRootParams) (connector.Root, *connector.Error) {
	if params.RootKey != f.root.Key {
		return connector.Root{}, f.fixtureError("read_root")
	}
	return f.observedRoot(), nil
}

func (f *memoryFixture) observedRoot() connector.Root {
	if f.advanceObservation {
		now := time.Now().UTC()
		if !now.After(f.root.ObservedAt) {
			now = f.root.ObservedAt.Add(time.Nanosecond)
		}
		f.root.ObservedAt = now
	}
	root := f.root
	if f.fabricatedRootRead {
		root.Title = "Fabricated root"
	}
	return root
}

func (f *memoryFixture) ListComments(_ context.Context, params connector.ListCommentsParams) (connector.ListCommentsResult, *connector.Error) {
	if params.RootKey != f.root.Key {
		return connector.ListCommentsResult{}, f.fixtureError("list_comments")
	}
	comments := append([]connector.Comment(nil), f.comments...)
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	if f.reverseComments {
		for left, right := 0, len(comments)-1; left < right; left, right = left+1, right-1 {
			comments[left], comments[right] = comments[right], comments[left]
		}
	}
	if f.fabricatedComments {
		comments[0].Body = "Fabricated comment"
	}
	return connector.ListCommentsResult{Comments: comments}, nil
}

func (f *memoryFixture) CompleteRoot(_ context.Context, params connector.CompleteRootParams) (connector.Root, *connector.Error) {
	if params.RootKey != f.root.Key {
		return connector.Root{}, f.fixtureError("complete_root")
	}
	f.fabricatedRootRead = false
	if f.root.State != "complete" {
		f.root.State = "complete"
		if !f.unchangedCompletionRev {
			f.root.Revision = "revision-complete"
		}
		if !f.staleCompletionTime {
			f.root.UpdatedAt = nextMutationTime(f.root.UpdatedAt, f.root.ObservedAt)
		}
		f.root.ObservedAt = f.root.UpdatedAt
	}
	if f.nonIdempotentComplete {
		f.root.Revision += "-changed"
		f.root.UpdatedAt = nextMutationTime(f.root.UpdatedAt, f.root.ObservedAt)
		f.root.ObservedAt = f.root.UpdatedAt
	}
	f.recordMutation("complete_root", params)
	if f.forbiddenMutationKey != "" {
		encoded, err := json.Marshal(map[string]any{"root_key": "root-example", "nested": map[string]any{f.forbiddenMutationKey: "neutral-forbidden"}})
		if err != nil {
			panic(err)
		}
		f.mutations = append(f.mutations, Mutation{
			Method: "complete_root",
			Params: encoded,
		})
	}
	if f.trailingMutation {
		f.mutations = append(f.mutations, Mutation{Method: "complete_root", Params: json.RawMessage(`{"root_key":"root-example"} {"issueId":"neutral-forbidden"}`)})
	}
	return f.root, nil
}

func (f *memoryFixture) PublishComment(_ context.Context, params connector.PublishCommentParams) (connector.Comment, *connector.Error) {
	if params.RootKey != f.root.Key {
		return connector.Comment{}, f.fixtureError("publish_comment")
	}
	if !f.nonIdempotentPublish {
		if published, ok := f.publications[params.OperationID]; ok && (!f.duplicateAfterCrash || !f.publicationCrashed) {
			return published, nil
		}
	}
	f.publicationCrashed = false
	stamp := nextMutationTime(f.root.UpdatedAt, f.root.ObservedAt)
	for _, comment := range f.providerComments {
		stamp = nextMutationTime(stamp, comment.CreatedAt, comment.UpdatedAt)
	}
	created := connector.Comment{
		ID: "comment-published", Revision: "comment-revision-published", Body: params.Body,
		Author:    connector.Actor{ID: f.description.SelfActorID, DisplayName: "Connector Actor"},
		CreatedAt: stamp, UpdatedAt: stamp,
	}
	if f.stalePublicationTime {
		created.CreatedAt = f.root.UpdatedAt.Add(-time.Hour)
		created.UpdatedAt = created.CreatedAt
	}
	f.comments = append(f.comments, created)
	if !f.discardProviderComment {
		f.providerComments = append(f.providerComments, created)
	}
	f.publications[params.OperationID] = created
	f.recordMutation("publish_comment", params)
	if f.wrongMutationType {
		f.mutations = append(f.mutations, Mutation{
			Method: "publish_comment",
			Params: json.RawMessage(`{"root_key":"root-example","body":{}}`),
		})
	}
	return created, nil
}

func nextMutationTime(providerTimes ...time.Time) time.Time {
	stamp := time.Now().UTC()
	for _, providerTime := range providerTimes {
		if !stamp.After(providerTime) {
			stamp = providerTime.Add(time.Nanosecond)
		}
	}
	return stamp
}

func (f *memoryFixture) ListFields(context.Context, connector.ListFieldsParams) (connector.ListFieldsResult, *connector.Error) {
	return connector.ListFieldsResult{Fields: append([]connector.FieldDescriptor(nil), f.descriptors...)}, nil
}

func (f *memoryFixture) ReadFields(_ context.Context, params connector.ReadFieldsParams) (connector.ReadFieldsResult, *connector.Error) {
	if params.RootKey != f.root.Key {
		return connector.ReadFieldsResult{}, f.fixtureError("read_fields")
	}
	values := make(map[string]connector.FieldValue, len(params.FieldIDs))
	for _, id := range params.FieldIDs {
		if f.brokenReadOnlyField && id == "field-read-only" {
			return connector.ReadFieldsResult{}, &connector.Error{Code: "field_not_found", Message: "field was not found"}
		}
		value, ok := f.root.Fields[id]
		if !ok {
			return connector.ReadFieldsResult{}, &connector.Error{Code: "field_not_found", Message: "field was not found"}
		}
		values[id] = value
	}
	return connector.ReadFieldsResult{Fields: values}, nil
}

func (f *memoryFixture) WriteFields(_ context.Context, params connector.WriteFieldsParams) (connector.WriteFieldsResult, *connector.Error) {
	if params.RootKey != f.root.Key {
		return connector.WriteFieldsResult{}, f.fixtureError("write_fields")
	}
	for _, value := range params.Fields {
		if value.Kind == f.rejectFieldKind {
			return connector.WriteFieldsResult{}, &connector.Error{Code: "unsupported_field", Message: "field kind is unsupported"}
		}
	}
	if len(params.Expected) != len(params.Fields) {
		return connector.WriteFieldsResult{}, &connector.Error{Code: "invalid_field_value", Message: "expected field values are required"}
	}
	for id := range params.Fields {
		expected, ok := params.Expected[id]
		if !ok || f.root.Fields[id] != expected {
			return connector.WriteFieldsResult{}, &connector.Error{Code: "field_conflict", Message: "field changed before write"}
		}
	}
	for id, value := range params.Fields {
		f.root.Fields[id] = value
		if !f.discardProviderFields {
			f.providerFields[id] = value
		}
	}
	f.recordMutation("write_fields", params)
	if f.forbiddenFieldValueKey != "" {
		encoded, err := json.Marshal(map[string]any{
			"root_key": "root-example",
			"fields": map[string]any{
				"kata_uid": map[string]any{"kind": "date", "value": "2026-08-20", f.forbiddenFieldValueKey: "neutral-forbidden"},
			},
		})
		if err != nil {
			panic(err)
		}
		f.mutations = append(f.mutations, Mutation{Method: "write_fields", Params: encoded})
	}
	return connector.WriteFieldsResult{Fields: params.Fields}, nil
}

func (f *memoryFixture) recordMutation(method string, params any) {
	encoded, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	f.mutations = append(f.mutations, Mutation{Method: method, Params: encoded})
}

func (f *memoryFixture) fixtureError(method string) *connector.Error {
	if f.errorMethod == method {
		return &connector.Error{Code: "root_not_found", Message: f.errorMessage}
	}
	switch f.errorMode {
	case "configuration":
		return &connector.Error{Code: "configuration_invalid", Message: "configuration requires a valid root"}
	case "unsafe":
		return &connector.Error{Code: "root_not_found", Message: "request failed\nstack trace"}
	default:
		return &connector.Error{Code: "root_not_found", Message: "root was not found"}
	}
}
