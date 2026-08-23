// Package connector executes configured connector processes.
package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/processtree"
	protocol "go.kenn.io/kata/pkg/connector"
)

const (
	maxResponseBytes             = 4 << 20
	connectorProcessCleanupGrace = 100 * time.Millisecond
)

var (
	errResponseTooLarge      = errors.New("connector response exceeds 4 MiB")
	errConnectorChildTimeout = errors.New("connector child timeout")
	errConnectorCleanupWait  = errors.New("connector process cleanup timed out")

	// ErrProcessFailure marks connector executable startup, transport, or exit
	// failure without rendering child paths, stderr, or configuration.
	ErrProcessFailure = errors.New("external connector process failed")
	// ErrProtocolFailure marks an invalid connector protocol response.
	ErrProtocolFailure = errors.New("external connector protocol failed")
	// ErrRequestTimeout marks a configured child timeout while the parent
	// request context remains live.
	ErrRequestTimeout = errors.New("external connector request timed out")
)

type callFailure struct {
	kind  error
	cause error
}

func (e *callFailure) Error() string { return e.kind.Error() }

func (e *callFailure) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

func newCallFailure(kind, cause error) error {
	return &callFailure{kind: kind, cause: cause}
}

// Client invokes every operation in the external connector protocol.
type Client interface {
	Describe(context.Context) (protocol.Description, error)
	ResolveRoot(context.Context, protocol.ResolveRootParams) (protocol.Root, error)
	ReadRoot(context.Context, protocol.ReadRootParams) (protocol.Root, error)
	ListComments(context.Context, protocol.ListCommentsParams) (protocol.ListCommentsResult, error)
	CompleteRoot(context.Context, protocol.CompleteRootParams) (protocol.Root, error)
	PublishComment(context.Context, protocol.PublishCommentParams) (protocol.Comment, error)
	ListFields(context.Context) (protocol.ListFieldsResult, error)
	ReadFields(context.Context, protocol.ReadFieldsParams) (protocol.ReadFieldsResult, error)
	WriteFields(context.Context, protocol.WriteFieldsParams) (protocol.WriteFieldsResult, error)
}

type processClient struct {
	config config.ConnectorConfig
}

// NewProcessClient constructs a client for a validated connector config.
func NewProcessClient(cfg config.ConnectorConfig) Client {
	return newProcessClient(cfg)
}

func newProcessClient(cfg config.ConnectorConfig) *processClient {
	return &processClient{config: cfg}
}

func (p *processClient) Describe(ctx context.Context) (protocol.Description, error) {
	return callResult(ctx, p, "describe", protocol.DescribeParams{}, validateDescription)
}

func (p *processClient) ResolveRoot(ctx context.Context, params protocol.ResolveRootParams) (protocol.Root, error) {
	return callResult(ctx, p, "resolve_root", params, validateRoot)
}

func (p *processClient) ReadRoot(ctx context.Context, params protocol.ReadRootParams) (protocol.Root, error) {
	return callResult(ctx, p, "read_root", params, func(result protocol.Root) error {
		if err := validateRoot(result); err != nil {
			return err
		}
		if result.Key != params.RootKey {
			return errors.New("read_root result key does not match request")
		}
		return nil
	})
}

func (p *processClient) ListComments(ctx context.Context, params protocol.ListCommentsParams) (protocol.ListCommentsResult, error) {
	return callResult(ctx, p, "list_comments", params, validateComments)
}

func (p *processClient) CompleteRoot(ctx context.Context, params protocol.CompleteRootParams) (protocol.Root, error) {
	return callResult(ctx, p, "complete_root", params, func(result protocol.Root) error {
		if err := validateRoot(result); err != nil {
			return err
		}
		if result.Key != params.RootKey {
			return errors.New("complete_root result key does not match request")
		}
		if result.State != "complete" {
			return errors.New("complete_root result state is not complete")
		}
		return nil
	})
}

func (p *processClient) PublishComment(ctx context.Context, params protocol.PublishCommentParams) (protocol.Comment, error) {
	if !protocol.ValidOperationID(params.OperationID) {
		return protocol.Comment{}, errors.New("operation_id must be nonempty and canonical")
	}
	description, err := p.Describe(ctx)
	if err != nil {
		return protocol.Comment{}, err
	}
	if !slices.Contains(description.Capabilities, protocol.CapabilityPublishComment) ||
		!requiredCanonical(description.SelfActorID) {
		return protocol.Comment{}, invalidResult(errors.New("connector does not advertise a publishing actor"))
	}
	return callResult(ctx, p, "publish_comment", params, func(result protocol.Comment) error {
		if err := validateComment(result); err != nil {
			return err
		}
		if result.Body != params.Body || result.Deleted || result.Author.ID != description.SelfActorID {
			return errors.New("publish_comment result does not match the requested publication")
		}
		return nil
	})
}

func (p *processClient) ListFields(ctx context.Context) (protocol.ListFieldsResult, error) {
	return callResult(ctx, p, "list_fields", protocol.ListFieldsParams{}, validateFields)
}

func (p *processClient) ReadFields(ctx context.Context, params protocol.ReadFieldsParams) (protocol.ReadFieldsResult, error) {
	return callResult(ctx, p, "read_fields", params, func(result protocol.ReadFieldsResult) error {
		return validateReadFields(params, result)
	})
}

func (p *processClient) WriteFields(ctx context.Context, params protocol.WriteFieldsParams) (protocol.WriteFieldsResult, error) {
	for _, value := range params.Fields {
		if err := protocol.ValidateFieldValue(value); err != nil {
			return protocol.WriteFieldsResult{}, err
		}
	}
	return callResult(ctx, p, "write_fields", params, func(result protocol.WriteFieldsResult) error {
		if result.Fields == nil {
			return errors.New("write_fields result is missing fields")
		}
		for _, value := range result.Fields {
			if err := protocol.ValidateFieldValue(value); err != nil {
				return err
			}
		}
		if !maps.Equal(result.Fields, params.Fields) {
			return errors.New("write_fields result does not match requested values")
		}
		return nil
	})
}

func callResult[R any](ctx context.Context, p *processClient, method string, params any, validate func(R) error) (R, error) {
	var result R
	if err := p.call(ctx, method, params, &result); err != nil {
		return result, err
	}
	if err := validate(result); err != nil {
		return *new(R), invalidResult(err)
	}
	return result, nil
}

func validateReadFields(params protocol.ReadFieldsParams, result protocol.ReadFieldsResult) error {
	expected := make(map[string]struct{}, len(params.FieldIDs))
	for _, id := range params.FieldIDs {
		expected[id] = struct{}{}
	}
	if result.Fields == nil || len(result.Fields) != len(expected) {
		return errors.New("read_fields result does not match requested selectors")
	}
	for id := range expected {
		value, ok := result.Fields[id]
		if !ok {
			return errors.New("read_fields result does not match requested selectors")
		}
		if err := protocol.ValidateFieldValue(value); err != nil {
			return err
		}
	}
	return nil
}

func validateComments(result protocol.ListCommentsResult) error {
	if result.Comments == nil {
		return errors.New("list_comments result is missing comments")
	}
	seen := make(map[string]struct{}, len(result.Comments))
	for _, comment := range result.Comments {
		if err := validateComment(comment); err != nil {
			return err
		}
		if _, ok := seen[comment.ID]; ok {
			return errors.New("list_comments result contains duplicate comment identity")
		}
		seen[comment.ID] = struct{}{}
	}
	return nil
}

func validateFields(result protocol.ListFieldsResult) error {
	if result.Fields == nil {
		return errors.New("list_fields result is missing fields")
	}
	seen := make(map[string]struct{}, len(result.Fields))
	for _, descriptor := range result.Fields {
		if err := validateFieldDescriptor(descriptor); err != nil {
			return err
		}
		if _, ok := seen[descriptor.ID]; ok {
			return errors.New("list_fields result contains duplicate field identity")
		}
		seen[descriptor.ID] = struct{}{}
	}
	return nil
}

func invalidResult(cause error) error {
	return newCallFailure(ErrProtocolFailure, cause)
}

func validateDescription(result protocol.Description) error {
	if !requiredCanonical(result.ConnectorID) || !requiredText(result.DisplayName) || !requiredCanonical(result.AccountIdentity) {
		return errors.New("describe result is missing required identity")
	}
	if result.Protocol != protocol.ProtocolVersion {
		return errors.New("describe result has unsupported protocol")
	}
	if result.Capabilities == nil {
		return errors.New("describe result is missing capabilities")
	}
	if result.SelfActorID != "" && strings.TrimSpace(result.SelfActorID) != result.SelfActorID {
		return errors.New("describe result has noncanonical self actor identity")
	}
	return nil
}

func validateRoot(result protocol.Root) error {
	if !requiredCanonical(result.Key) || !requiredCanonical(result.IdentityKey) ||
		!requiredCanonical(result.State) || !requiredCanonical(result.Revision) {
		return errors.New("root result is missing required identity")
	}
	if result.UpdatedAt.IsZero() || result.ObservedAt.IsZero() {
		return errors.New("root result is missing required timestamp")
	}
	if result.ObservedAt.Before(result.UpdatedAt) {
		return errors.New("root result observation predates its update")
	}
	if result.Actor != nil {
		if err := validateActor(*result.Actor); err != nil {
			return err
		}
	}
	for _, value := range result.Fields {
		if err := protocol.ValidateFieldValue(value); err != nil {
			return err
		}
	}
	return nil
}

func validateComment(result protocol.Comment) error {
	if !requiredCanonical(result.ID) || !requiredCanonical(result.Revision) {
		return errors.New("comment result is missing required identity")
	}
	if err := validateActor(result.Author); err != nil {
		return err
	}
	if result.CreatedAt.IsZero() || result.UpdatedAt.IsZero() {
		return errors.New("comment result is missing required timestamp")
	}
	if result.UpdatedAt.Before(result.CreatedAt) {
		return errors.New("comment result update predates its creation")
	}
	return nil
}

func validateActor(result protocol.Actor) error {
	if !requiredCanonical(result.ID) || !requiredText(result.DisplayName) {
		return errors.New("actor result is missing required identity")
	}
	return nil
}

func validateFieldDescriptor(result protocol.FieldDescriptor) error {
	if !requiredCanonical(result.ID) || !requiredText(result.DisplayName) || !requiredCanonical(result.SchemaRevision) {
		return errors.New("field descriptor is missing required identity")
	}
	if result.AcceptedKinds == nil {
		return errors.New("field descriptor is missing accepted kinds")
	}
	return nil
}

func requiredCanonical(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func requiredText(value string) bool {
	return strings.TrimSpace(value) != ""
}

func (p *processClient) call(ctx context.Context, method string, params any, result any) error {
	settings := json.RawMessage(`{}`)
	if p.config.Settings != nil {
		encodedSettings, err := json.Marshal(p.config.Settings)
		if err != nil {
			return fmt.Errorf("connector %q: invalid settings", p.config.ID)
		}
		settings = encodedSettings
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("connector %q: invalid request parameters", p.config.ID)
	}
	request := protocol.Request{
		Protocol: protocol.ProtocolVersion,
		ID:       requestID(),
		Method:   method,
		Instance: p.config.ID,
		Settings: settings,
		Params:   encodedParams,
	}
	input, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("connector %q: encode request", p.config.ID)
	}

	childEnv, redactions, err := p.environment(settings)
	if err != nil {
		return newCallFailure(ErrProcessFailure, err)
	}
	commandCtx, cancel := context.WithTimeoutCause(ctx, p.config.Timeout(), errConnectorChildTimeout)
	defer cancel()
	command := exec.Command(p.config.Command, p.config.Args...) // #nosec G204 -- executable and args are operator-controlled validated configuration.
	command.Env = childEnv
	command.Stdin = bytes.NewReader(input)
	command.Stderr = io.Discard
	stdout := &cappedBuffer{limit: maxResponseBytes}
	command.Stdout = stdout
	command.WaitDelay = connectorProcessCleanupGrace
	if commandCtx.Err() != nil {
		return connectorContextError(ctx, context.Cause(commandCtx))
	}
	processTree, err := processtree.New(command)
	if err != nil {
		return newCallFailure(ErrProcessFailure, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = processTree.Close()
		}
	}()
	if commandCtx.Err() != nil {
		return connectorContextError(ctx, context.Cause(commandCtx))
	}
	if err := processTree.Start(); err != nil {
		return newCallFailure(ErrProcessFailure, err)
	}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- processTree.Wait()
	}()
	runErr, contextCause := awaitCommandOutcome(commandCtx, waitResult, func() error {
		return processTree.TerminateWithGrace(connectorProcessCleanupGrace)
	})
	if contextCause != nil {
		_ = processTree.Close()
		closed = true
		return connectorContextError(ctx, contextCause)
	}
	if runErr != nil {
		_ = processTree.Close()
		closed = true
		return newCallFailure(ErrProcessFailure, runErr)
	}
	if err := processTree.Close(); err != nil {
		closed = true
		return newCallFailure(ErrProcessFailure, err)
	}
	closed = true
	if stdout.exceeded {
		return newCallFailure(ErrProtocolFailure, errResponseTooLarge)
	}
	if !utf8.Valid(stdout.Bytes()) {
		return newCallFailure(ErrProtocolFailure, errors.New("connector response is not valid UTF-8"))
	}

	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var response protocol.Response
	if err := decoder.Decode(&response); err != nil {
		return newCallFailure(ErrProtocolFailure, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return newCallFailure(ErrProtocolFailure, errors.New("connector response contains trailing JSON"))
		}
		return newCallFailure(ErrProtocolFailure, err)
	}
	if response.Protocol != protocol.ProtocolVersion {
		return newCallFailure(ErrProtocolFailure, errors.New("connector response has unsupported protocol"))
	}
	if response.ID != request.ID {
		return newCallFailure(ErrProtocolFailure, errors.New("connector response ID does not match request"))
	}
	if (len(response.Result) == 0) == (response.Error == nil) {
		return newCallFailure(ErrProtocolFailure, errors.New("connector response must contain exactly one of result or error"))
	}
	if response.Error != nil {
		return redactError(response.Error, redactions)
	}
	if bytes.Equal(bytes.TrimSpace(response.Result), []byte("null")) {
		return newCallFailure(ErrProtocolFailure, errors.New("connector response result must not be null"))
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return newCallFailure(ErrProtocolFailure, err)
	}
	return nil
}

func awaitCommandOutcome(
	ctx context.Context,
	waitResult <-chan error,
	kill func() error,
) (error, error) {
	select {
	case runErr := <-waitResult:
		return runErr, nil
	default:
	}
	select {
	case runErr := <-waitResult:
		return runErr, nil
	case <-ctx.Done():
		select {
		case runErr := <-waitResult:
			return runErr, nil
		default:
		}
		terminationErr := kill()
		timer := time.NewTimer(connectorProcessCleanupGrace)
		defer timer.Stop()
		select {
		case <-waitResult:
			if terminationErr != nil {
				return terminationErr, nil
			}
			return nil, context.Cause(ctx)
		case <-timer.C:
			return errors.Join(terminationErr, errConnectorCleanupWait), nil
		}
	}
}

func connectorContextError(parent context.Context, cause error) error {
	if errors.Is(cause, errConnectorChildTimeout) {
		return newCallFailure(ErrRequestTimeout, context.DeadlineExceeded)
	}
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

func redactError(response *protocol.Error, redactions []string) error {
	safe := *response
	normalized := normalizeRedactions(redactions)
	safe.Code = redactErrorField(safe.Code, normalized)
	safe.Message = redactErrorField(safe.Message, normalized)
	return &safe
}

func redactErrorField(field string, redactions []string) string {
	var decoded string
	if err := json.Unmarshal([]byte(field), &decoded); err == nil {
		return replaceRedactions(decoded, redactions)
	}
	return replaceRedactions(field, redactions)
}

func replaceRedactions(value string, redactions []string) string {
	for _, redaction := range redactions {
		value = strings.ReplaceAll(value, redaction, "[redacted]")
		if !strings.Contains(value, `\`) {
			continue
		}
		pattern, err := regexp.Compile(jsonEncodedRedactionPattern(redaction))
		if err != nil {
			continue
		}
		value = pattern.ReplaceAllString(value, "[redacted]")
	}
	return value
}

func jsonEncodedRedactionPattern(value string) string {
	var pattern strings.Builder
	for _, char := range value {
		alternatives := []string{regexp.QuoteMeta(string(char))}
		if escaped, ok := simpleJSONEscape(char); ok {
			alternatives = append(alternatives, regexp.QuoteMeta(escaped))
		}
		alternatives = append(alternatives, jsonUnicodeEscapePattern(char))
		pattern.WriteByte('(')
		pattern.WriteString(strings.Join(alternatives, "|"))
		pattern.WriteByte(')')
	}
	return pattern.String()
}

func simpleJSONEscape(char rune) (string, bool) {
	switch char {
	case '"':
		return `\"`, true
	case '\\':
		return `\\`, true
	case '/':
		return `\/`, true
	case '\b':
		return `\b`, true
	case '\f':
		return `\f`, true
	case '\n':
		return `\n`, true
	case '\r':
		return `\r`, true
	case '\t':
		return `\t`, true
	default:
		return "", false
	}
}

func jsonUnicodeEscapePattern(char rune) string {
	if char <= 0xffff {
		return jsonUTF16CodeUnitPattern(uint16(char)) // #nosec G115 -- the branch bounds char to one UTF-16 code unit.
	}
	value := char - 0x10000
	high := uint16(0xd800 + (value >> 10)) // #nosec G115 -- valid runes produce a bounded high surrogate.
	low := uint16(0xdc00 + (value & 0x3ff))
	return jsonUTF16CodeUnitPattern(high) + jsonUTF16CodeUnitPattern(low)
}

func jsonUTF16CodeUnitPattern(value uint16) string {
	hex := fmt.Sprintf("%04x", value)
	var pattern strings.Builder
	pattern.WriteString(`\\u`)
	for _, digit := range hex {
		if digit >= 'a' && digit <= 'f' {
			pattern.WriteByte('[')
			pattern.WriteRune(digit)
			pattern.WriteRune(digit - ('a' - 'A'))
			pattern.WriteByte(']')
			continue
		}
		pattern.WriteRune(digit)
	}
	return pattern.String()
}

func normalizeRedactions(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if len(normalized[i]) == len(normalized[j]) {
			return normalized[i] < normalized[j]
		}
		return len(normalized[i]) > len(normalized[j])
	})
	return normalized
}

func (p *processClient) environment(_ json.RawMessage) ([]string, []string, error) {
	env := minimalRuntimeEnv(runtime.GOOS)
	redactions := make([]string, 0, len(p.config.Env))
	for target, source := range p.config.Env {
		value, ok := os.LookupEnv(source)
		if !ok {
			return nil, nil, fmt.Errorf("connector %q: environment source %q is not set", p.config.ID, source)
		}
		env = append(env, target+"="+value)
		redactions = append(redactions, value)
	}
	return env, normalizeRedactions(redactions), nil
}

func minimalRuntimeEnv(goos string) []string {
	keys := []string{"HOME", "TMPDIR"}
	if goos == "windows" {
		keys = append(keys, "SystemRoot", "ComSpec", "TEMP", "TMP", "USERPROFILE")
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func requestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request"
	}
	return hex.EncodeToString(value[:])
}

type cappedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.exceeded = true
		return len(p), nil
	}
	_, err := b.buffer.Write(p)
	return len(p), err
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}
