Fixture JSON is generated from independent literal sources in
`fixtures_generate_test.go` and `workflow_generate_test.go`. Run their artifact
tests with `-update-golden` to generate missing files; normal runs reject missing
or changed files. V1 artifacts and default-export hashes remain frozen.
