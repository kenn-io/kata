// Package projectscope defines the HTTP project-scope header contract.
package projectscope

import (
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// Header optionally narrows a request to a fixed set of projects.
// Each value is a JSON array; repeated values intersect. Coalescing two values
// with a comma produces invalid JSON and therefore fails closed.
const Header = "X-Kata-Project-Scope"

// ParseHeader validates one nonempty narrowing layer. Project UIDs
// are preferred; id:N preserves the legacy bound MCP API which lacks a UID.
func ParseHeader(value string) ([]string, error) {
	var refs []string
	if err := json.Unmarshal([]byte(value), &refs); err != nil || len(refs) == 0 {
		return nil, errors.New("project scope must be a nonempty JSON array")
	}
	for _, ref := range refs {
		if uid.Valid(ref) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(ref, "id:"), 10, 64)
		if err != nil || id <= 0 || ref != "id:"+strconv.FormatInt(id, 10) {
			return nil, errors.New("project scope contains an invalid project reference")
		}
	}
	return refs, nil
}
