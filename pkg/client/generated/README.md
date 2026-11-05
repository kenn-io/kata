# Client generation

Run `make api-generate` with the repository's pinned Go toolchain and
`oapi-codegen-dd` version. Generated Go files and OpenAPI artifacts belong in a
separate commit from overlays and templates.

Cron validation can carry empty names and zero timestamps. The client-only
OpenAPI document represents their presence with pointers; the published wire
schema still rejects null response names and timestamps. Complex validators
skip nil pointers, so the overlay marks fields with `x-kata-require-presence`.
The template emits only their nil checks, then delegates all nested validation
to the generator. Simple validators already check required pointers.

Request definitions use `json.RawMessage` to retain number spelling and invalid
document shapes. Its optional value field needs both
`x-go-type-skip-optional-pointer` and `x-omitempty`: nil stays missing, while a
nonempty raw `null` value remains explicit null.
