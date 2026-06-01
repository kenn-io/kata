package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGeneratedStubsMatchSource regenerates stubs_gen.go in memory and
// compares against the committed file. A drift means someone touched
// internal/db/storage.go (added a method, changed a signature) without
// re-running `go generate`, or hand-edited stubs_gen.go directly.
//
// The committed stubs_gen.go is the source of truth at build time; this
// test guards against a stale-but-still-compiling state where the
// interface has grown a method we never stubbed.
func TestGeneratedStubsMatchSource(t *testing.T) {
	got, err := Generate("../../storage.go")
	require.NoError(t, err)

	want, err := os.ReadFile("../stubs_gen.go")
	require.NoError(t, err)

	assert.Equal(t, string(want), string(got),
		"stubs_gen.go is out of date — run `go generate ./internal/db/pgstore`")
}
