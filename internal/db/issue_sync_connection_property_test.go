package db_test

import (
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

// Contract: generation is a nonempty, bounded ASCII identity, never a label,
// URL or credential value. The oracle states the documented alphabet directly.
func TestIssueSyncConnectionGenerationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		generation := hegel.Draw(ht, hegel.Text())
		selection := db.IssueSyncConnection{
			OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
			ConnectionUID:    "01ARZ3NDEKTSV4RRFFQ69G5FAW",
			Generation:       generation,
		}
		valid := len(generation) > 0 && len(generation) <= 128 && strings.Trim(generation, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
		if got := selection.Validate() == nil; got != valid {
			ht.Fatalf("generation validation disagrees with the declared alphabet")
		}
	}, hegel.WithTestCases(1000))
}
