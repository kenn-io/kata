package main

import (
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/textsafe"
)

func federationEmbeddingLines(status *api.FederationEmbeddingStatus) []string {
	fields := status.DisplayFields()
	for i := range fields {
		fields[i] = textsafe.Line(fields[i])
	}
	return fields
}
