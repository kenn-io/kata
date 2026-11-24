package db

import "strings"

// MinPriority is the highest priority value.
const MinPriority = 0

// MaxPriority is the lowest priority value.
const MaxPriority = 4

// IsPriorityLabel reports whether a label names a priority.
func IsPriorityLabel(label string) bool {
	label = strings.ToLower(strings.TrimSpace(label))
	return (len(label) == 2 && label[0] == 'p' && label[1] >= '0'+MinPriority && label[1] <= '0'+MaxPriority) || strings.HasPrefix(label, "priority:")
}
