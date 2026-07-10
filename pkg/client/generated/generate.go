package generated

//go:generate sh -c "go run ../../../cmd/kata openapi --version 3.0 --format yaml > ../openapi.yaml"
//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5 -config config.yaml ../openapi.yaml

// ReadyGlobalIssue is the pre-0.10 name of the cross-project ready row,
// renamed when ready responses were hydrated to full IssueOut. It lives in
// this file rather than the generated ones because regeneration deletes
// every other .go file in this package.
//
// Deprecated: use ReadyGlobalIssueOut.
type ReadyGlobalIssue = ReadyGlobalIssueOut
