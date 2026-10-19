package main

import (
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/localprofilestore"
)

// The kata binary links the storage engines, so it can inspect and start
// local profiles. Installing at package initialization covers every command
// and the tests that drive them.
func init() {
	client.UseLocalProfileStorage(localprofilestore.Storage{})
}
