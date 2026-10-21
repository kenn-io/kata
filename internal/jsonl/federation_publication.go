package jsonl

import (
	"context"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationpublication"
)

// EncodeFederationPublication freezes the closed version-1 publication object.
func EncodeFederationPublication(p db.FederationPublication) ([]byte, error) {
	return federationpublication.Encode(p)
}

// DecodeFederationPublication validates the closed object and its exact digest.
func DecodeFederationPublication(raw []byte) (db.FederationPublication, error) {
	return federationpublication.Decode(raw)
}

// CaptureFederationPublication captures and validates one consistent snapshot.
func CaptureFederationPublication(ctx context.Context, store db.Storage, projectID int64, p db.FederationPublication) (db.FederationPublication, error) {
	return federationpublication.Capture(ctx, store, projectID, p)
}
