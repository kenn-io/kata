package client

import (
	"os"
	"testing"

	"go.kenn.io/kata/internal/testfix"
)

func TestMain(m *testing.M) { os.Exit(testfix.RunIsolated(m)) }
