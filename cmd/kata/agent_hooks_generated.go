package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

const nativeAgentHookDigestPrefix = "// kata-generated-sha256: "

// Metadata embedded in JavaScript comments and literals must escape line
// separators. Deterministic encoding also keeps ownership records stable.
var nativeAgentHookOwnedJSONOptions = json.JoinOptions(
	jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true), jsontext.AllowInvalidUTF8(true),
	json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), json.Deterministic(true),
)

// Hash the stored metadata and runtime, rather than reconstructing today's
// template. Lengths separate bundle members; the digest itself is excluded.
// This detects local edits, not authenticity against a repository writer.
func nativeAgentHookDigest(files ...[]byte) string {
	hash := sha256.New()
	for _, data := range files {
		_, _ = fmt.Fprintf(hash, "%d:", len(data))
		_, _ = hash.Write(data)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func sealNativeAgentHookCode(code []byte, companions ...[]byte) []byte {
	digest := nativeAgentHookDigest(append([][]byte{code}, companions...)...)
	return append(code, []byte(nativeAgentHookDigestPrefix+digest+"\n")...)
}

func nativeAgentHookCodeUnedited(code []byte, companions ...[]byte) bool {
	marker := bytes.LastIndex(code, []byte("\n"+nativeAgentHookDigestPrefix))
	if marker < 0 {
		return false
	}
	body := code[:marker+1]
	digest := nativeAgentHookDigest(append([][]byte{body}, companions...)...)
	return string(code[marker+1:]) == nativeAgentHookDigestPrefix+digest+"\n"
}
