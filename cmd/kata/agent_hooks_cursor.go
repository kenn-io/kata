package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"

	"go.kenn.io/kit/agenthook"
)

// writeNativeAgentContract retains Kit's native validation, event dispatch and
// response encoding. Cursor's SessionStart additional_context is documented at
// https://cursor.com/docs/hooks#sessionstart but absent from Kit v0.26.0's
// response encoder. The bridge adds only that field to Kit's neutral response.
// Remove the bridge once released Kit encodes this SessionStart context itself.
func writeNativeAgentContract(ctx context.Context, agent agenthook.Agent, input io.Reader, output io.Writer, text string) error {
	handler := &nativeAgentContractHandler{agent: agent, text: text}
	var native bytes.Buffer
	if err := agenthook.Handle(ctx, agent, input, &native, handler); err != nil {
		return err
	}
	response := native.Bytes()
	if handler.cursorContext != "" {
		envelope := make(map[string]jsontext.Value)
		if err := json.Unmarshal(response, &envelope); err != nil {
			return fmt.Errorf("decode Cursor hook response: %w", err)
		}
		contextJSON, err := json.Marshal(handler.cursorContext)
		if err != nil {
			return fmt.Errorf("encode Cursor hook context: %w", err)
		}
		envelope["additional_context"] = contextJSON
		response, err = json.Marshal(envelope)
		if err != nil {
			return fmt.Errorf("encode Cursor hook response: %w", err)
		}
		response = append(response, '\n')
	}
	_, err := output.Write(response)
	return err
}
