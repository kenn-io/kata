package definition

import (
	"fmt"
	"strings"
	"unicode"
)

func portableKey(k string) bool {
	if k == "" || len(k) > 256 || k == "." || k == ".." {
		return false
	}
	for _, r := range k {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.", r) {
			return false
		}
	}
	return true
}

// TopoOrder breaks every tie by definition order, and rejects unsafe artifact keys.
func TopoOrder(steps []Step) ([]string, error) {
	index := map[string]int{}
	for i, s := range steps {
		if _, ok := index[s.Key]; ok || !portableKey(s.Key) {
			return nil, fmt.Errorf("invalid or duplicate step key %q", s.Key)
		}
		index[s.Key] = i
	}
	for _, s := range steps {
		for _, a := range s.After {
			if _, ok := index[a]; !ok {
				return nil, fmt.Errorf("unknown after key %q", a)
			}
		}
	}
	done := map[string]bool{}
	order := make([]string, 0, len(steps))
	for len(order) < len(steps) {
		found := false
		for _, s := range steps {
			if done[s.Key] {
				continue
			}
			ready := true
			for _, a := range s.After {
				if !done[a] {
					ready = false
					break
				}
			}
			if ready {
				done[s.Key] = true
				order = append(order, s.Key)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("workflow cycle")
		}
	}
	return order, nil
}
