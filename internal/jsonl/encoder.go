package jsonl

import (
	"encoding/json/v2"
	"fmt"
	"io"
)

// Encoder writes compact JSONL envelopes.
type Encoder struct {
	w io.Writer
}

// NewEncoder returns an encoder that writes one envelope per line.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Write writes one compact JSON object followed by '\n'.
func (e *Encoder) Write(env Envelope) error {
	bs, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := e.w.Write(append(bs, '\n')); err != nil {
		return fmt.Errorf("write envelope: %w", err)
	}
	return nil
}
