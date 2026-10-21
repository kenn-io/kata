package main

import (
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/storageinspect"
)

// The kata binary links the storage engines, so it can check storage before
// it starts or replaces a local daemon. Installing at package initialization
// covers every command and the tests that drive them.
func init() {
	client.UseStorageInspector(storageinspect.Inspector{})
}
