package importlabels

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	invalidChar  = regexp.MustCompile(`[^a-z0-9._:-]+`)
	repeatedDash = regexp.MustCompile(`-+`)
)

// Normalize converts an external label into kata's import label alphabet.
func Normalize(s string) string {
	return NormalizeMax(s, 64)
}

// NormalizeMax converts an external label into kata's import label alphabet
// and bounds it to maxLen bytes.
func NormalizeMax(s string, maxLen int) string {
	normalized := strings.ToLower(strings.TrimSpace(s))
	normalized = strings.Join(strings.Fields(normalized), "-")
	normalized = invalidChar.ReplaceAllString(normalized, "-")
	normalized = repeatedDash.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-._:")
	if normalized == "" {
		normalized = "imported"
	}
	if len(normalized) <= maxLen {
		return normalized
	}
	sum := sha256.Sum256([]byte(normalized))
	suffix := hex.EncodeToString(sum[:])[:8]
	prefixLen := maxLen - len(suffix) - 1
	if prefixLen < 1 {
		return suffix[:maxLen]
	}
	prefix := strings.TrimRight(normalized[:prefixLen], "-._:")
	if prefix == "" {
		prefix = "imported"
	}
	return prefix + "-" + suffix
}

// AppendNormalized appends normalized labels that have not already been seen.
func AppendNormalized(out []string, seen map[string]struct{}, labels ...string) []string {
	for _, label := range labels {
		normalized := Normalize(label)
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}
