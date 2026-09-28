// Package jsonutil provides focused options for JSON decoding.
package jsonutil

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

var numberLiteralUnmarshalers = json.UnmarshalFromFunc(func(dec *jsontext.Decoder, value *any) error {
	if dec.PeekKind() != '0' {
		return errors.ErrUnsupported
	}
	raw, err := dec.ReadValue()
	*value = raw.Clone()
	return err
})

// PreserveNumberLiterals decodes numbers into any as owned jsontext.Value tokens,
// retaining precision and spelling. Other values use standard JSON decoding.
// It does not impose an EOF policy or change decoding into concrete numeric types.
func PreserveNumberLiterals() json.Options {
	return json.WithUnmarshalers(numberLiteralUnmarshalers)
}
