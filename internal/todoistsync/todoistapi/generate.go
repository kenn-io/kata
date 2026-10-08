// Package todoistapi is a client for Todoist's published API v1, generated
// from https://developer.todoist.com/openapi.json. config.yaml selects the
// operations Kata uses. Run go generate to refresh it from the live spec; every
// other .go file in this package is generated.
//
// jq rewrites the spec's escaped UTF-16 surrogate pairs, which the generator's
// YAML parser rejects, as literal UTF-8.
package todoistapi

//go:generate sh -c "find . -maxdepth 1 -type f -name '*.go' ! -name 'generate.go' -delete"
//go:generate sh -c "curl -fsSL https://developer.todoist.com/openapi.json | jq . > openapi.json"
//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5 -config config.yaml openapi.json
//go:generate rm openapi.json
