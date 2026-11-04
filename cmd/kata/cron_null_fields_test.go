package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// JSON null decodes to a nil object; field injection must reject it, not panic.
func TestCronStringFieldRejectsNullObject(t *testing.T) {
	require.Error(t, setCronStringField(nil, "expected_event_uid", "01ARZ3NDEKTSV4RRFFQ69G5FAV"))
}
