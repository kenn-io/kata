// Package authorname formats stored author names for display.
package authorname

import "regexp"

var suffix = regexp.MustCompile(` \[(member:[0-9a-f-]{36}; enrollment|service-account:[0-9a-f-]{36}; token):[0-9a-f-]{36}\]$`)

// Display removes a federated connection suffix from an author's name.
func Display(author string) string {
	return suffix.ReplaceAllString(author, "")
}
