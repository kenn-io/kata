package federationprovider

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os/exec"
	"strings"

	"go.kenn.io/kata/internal/processtree"
)

// Exchange runs a trusted, configured executable with explicit arguments, not a
// shell command. The helper is trusted same-user code, not a sandbox. It
// inherits the caller's environment, which may contain credentials. The request is
// sent only on stdin; stderr is discarded and stdout is bounded. Domain denials
// return a Response with no error; process errors return no response.
//
// Callers own durable request/token storage and exact replay checks against any
// previously accepted enrollment IDs. An error never permits replacing a token
// or forgetting an operation that might have committed remotely.
func Exchange(parent context.Context, command []string, request Request) (Response, error) {
	if !validRequest(request) || len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return Response{}, ErrInvalidRequest
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, '\x00') {
			return Response{}, ErrInvalidRequest
		}
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > MaxDocumentBytes {
		return Response{}, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(parent, AttemptTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Response{}, errors.Join(ErrProviderFailed, err)
	}
	cmd := exec.Command(command[0], command[1:]...) // #nosec G204 -- operator-configured executable and argv, never response data.
	cmd.Stdin = bytes.NewReader(data)
	output := &boundedOutput{cancel: cancel}
	cmd.Stdout = output
	// A nil Stderr connects to the null device, with no diagnostic pipe to retain.
	tree, err := processtree.New(cmd)
	if err != nil {
		return Response{}, ErrProviderFailed
	}
	if err := tree.Start(); err != nil {
		_ = tree.Close()
		return Response{}, ErrProviderFailed
	}
	terminated := make(chan struct{})
	var terminateErr error
	stop := context.AfterFunc(ctx, func() {
		terminateErr = tree.TerminateWithGrace(0)
		close(terminated)
	})
	waitErr := tree.Wait()
	if !stop() {
		<-terminated
	}
	closeErr := tree.Close()
	if output.overflow {
		return Response{}, ErrInvalidResponse
	}
	if err := ctx.Err(); err != nil {
		return Response{}, errors.Join(ErrProviderFailed, err)
	}
	if waitErr != nil || terminateErr != nil || closeErr != nil {
		if exit, ok := errors.AsType[*exec.ExitError](waitErr); ok && exit.ExitCode() == 2 {
			return Response{}, ErrInvalidRequest
		}
		return Response{}, ErrProviderFailed
	}
	return decodeResponse(output.buffer.Bytes(), request)
}

// Only the stdout-copy goroutine writes this buffer. Exchange reads it after
// Wait joins that goroutine. Cancelling on overflow also stops a noisy helper.
type boundedOutput struct {
	// Do not embed Buffer: its promoted ReadFrom would bypass Write in io.Copy.
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > MaxDocumentBytes-output.buffer.Len() {
		output.overflow = true
		output.cancel()
		return 0, ErrInvalidResponse
	}
	return output.buffer.Write(data)
}
